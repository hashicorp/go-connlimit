package connlimit

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
)

var (
	// ErrPerClientIPLimitReached is returned if accepting a new conn would exceed
	// the per-client-ip limit set.
	ErrPerClientIPLimitReached = errors.New("client connection limit reached")
)

// Limiter implements a simple limiter that tracks the number of connections
// from each client IP. It may be used in it's zero value although no limits
// will be configured initially - they can be set later with SetConfig.
type Limiter struct {
	cs  map[string]map[net.Conn]struct{}
	l   sync.Mutex
	cfg atomic.Value
}

// Config is the configuration for the limiter.
type Config struct {
	// MaxConnsPerClientIP limits how many concurrent connections are allowed from
	// a given client IP. The IP is the one reported by the connection so cannot
	// be relied upon if clients are connecting through multiple proxies or able
	// to spoof their source IP address in some way. Similarly, multiple clients
	// connected via a proxy or NAT gateway or similar will all be seen as coming
	// from the same IP and so limited as one client.
	MaxConnsPerClientIP int
}

// NewLimiter returns a limiter with the specified config.
func NewLimiter(cfg Config) *Limiter {
	l := &Limiter{}
	l.SetConfig(cfg)
	return l
}

// Accept is called as early as possible when handling a new conn. If the
// connection should be accepted according to the Limiter's Config, it will
// return a free func and nil error. The free func must be called when the
// connection is no longer being handled - typically in a defer statement in the
// main connection handling goroutine, this will decrement the counter for that
// client IP. If the configured limit has been reached, a no-op func is returned
// (doesn't need to be called), and ErrPerClientIPLimitReached is returned.
//
// If any other error is returned it signifies something wrong with the config
// or transient failure to read or parse the remote IP. The free func will be a
// no-op in this case and need not be called.
func (l *Limiter) Accept(conn net.Conn) (func(), error) {
	addrKey := addrKey(conn)

	// Load config outside locked section since it's not updated under lock anyway
	// and the atomic Load might be slower/contented so better to do outside lock.
	cfg := l.Config()

	l.l.Lock()
	defer l.l.Unlock()

	if l.cs == nil {
		l.cs = make(map[string]map[net.Conn]struct{})
	}

	cs := l.cs[addrKey]
	if cs == nil {
		cs = make(map[net.Conn]struct{})
		l.cs[addrKey] = cs
	}

	n := len(cs)

	// Might be greater since config is dynamic.
	if cfg.MaxConnsPerClientIP > 0 && n >= cfg.MaxConnsPerClientIP {
		return func() {}, ErrPerClientIPLimitReached
	}

	// Add the conn to the map
	cs[conn] = struct{}{}

	// Create a free func over the address key we used
	free := func() {
		l.freeConn(conn)
	}

	return free, nil
}

func addrKey(conn net.Conn) string {
	addr := conn.RemoteAddr()
	switch a := addr.(type) {
	case *net.TCPAddr:
		return addr.Network() + "/" + a.IP.String()
	case *net.UDPAddr:
		return addr.Network() + "/" + a.IP.String()
	case *net.IPAddr:
		return addr.Network() + "/" + a.IP.String()
	default:
		// not sure what to do with this, just assume whole Addr is relevant.
		return addr.Network() + "/" + addr.String()
	}
}

func (l *Limiter) freeConn(conn net.Conn) {
	addrKey := addrKey(conn)

	l.l.Lock()
	defer l.l.Unlock()

	cs, ok := l.cs[addrKey]
	if !ok {
		return
	}

	delete(cs, conn)
	if len(cs) == 0 {
		delete(l.cs, addrKey)
	}
}

// Config returns the current limiter configuration. It is safe to call from any
// goroutine.
func (l *Limiter) Config() Config {
	cfgRaw := l.cfg.Load()
	if cfg, ok := cfgRaw.(Config); ok {
		return cfg
	}
	return Config{}
}

// SetConfig dynamically updates the limiter configuration. It is safe to call
// from any goroutine.
func (l *Limiter) SetConfig(c Config) {
	l.cfg.Store(c)
}

// HTTPConnStateFunc returns a func that can be passed as the ConnState field of
// an http.Server. This intercepts new HTTP connections to the server and
// applies the limiting to new connections.
//
// Note that if the conn is hijacked from the HTTP server then it will be freed
// in the limiter as if it was closed. Servers that use Hijacking must implement
// their own calls if they need to continue limiting the number of concurrent
// hijacked connections.
func (l *Limiter) HTTPConnStateFunc() func(net.Conn, http.ConnState) {
	return func(conn net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			_, err := l.Accept(conn)
			if err != nil {
				conn.Close()
			}
		case http.StateHijacked:
			l.freeConn(conn)
		case http.StateClosed:
			l.freeConn(conn)
		}
	}
}
