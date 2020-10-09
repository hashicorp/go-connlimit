package connlimit

import (
	"errors"
	"fmt"
	"net"
)

// ListnerOption is used to configure NewListener.
type ListenerOption = func(*Listener) error

// Listener implements a limited net.Listener per-client IP address.
type Listener struct {
	net.Listener
	limiter    *Limiter
	limitError error
}

// NewListener returns a limited listener with the given options.
// By default, it will enforce a limit of 2 connections maximum per client IP address, and if a client
// exceeds its limit, it will close the connection and return ErrPerClientIPLimitReached. The limits,
// and the returned limit error are configurable to work in a variety of common TCP servers,
// HTTP servers, and gRPC server implementations.
func NewListener(opts ...ListenerOption) (*Listener, error) {
	// Create a default Listener configuration without actually listening on a port, yet.
	ln := &Listener{
		limiter:    NewLimiter(Config{MaxConnsPerClientIP: 2}),
		limitError: ErrPerClientIPLimitReached,
	}

	// Apply all the given options.
	for _, opt := range opts {
		err := opt(ln)
		if err != nil {
			return nil, err
		}
	}

	// If no raw listener was configured from the options, create one on localhost using a random port.
	if ln.Listener == nil {
		rawLn, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		ln.Listener = rawLn
	}
	return ln, nil
}

// WithLimiter provides a ListenerOption to configure the underling Limiter.
func WithLimiter(lm *Limiter) ListenerOption {
	return func(ln *Listener) error {
		ln.limiter = lm
		return nil
	}
}

// WithLimitError provides a ListenerOption to configure the underling limit error.
func WithLimitError(err error) ListenerOption {
	return func(ln *Listener) error {
		if err == nil {
			return fmt.Errorf("cannot configure nil limit error")
		}
		ln.limitError = err
		return nil
	}
}

// WithRawListener provides a ListenerOption to configure the underling raw net.Listener object.
func WithRawListener(rawLn net.Listener) ListenerOption {
	return func(ln *Listener) error {
		ln.Listener = rawLn
		return nil
	}
}

// Accept waits for and returns the next limited connection to the caller.
func (l *Listener) Accept() (net.Conn, error) {
	// Use the raw listener to accept a connection.
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	// Then attempt to accept the raw connection, which may fail if too many
	// client connections are already established.
	free, err := l.limiter.Accept(conn)
	if err != nil {
		conn.Close()
		if errors.Is(err, ErrPerClientIPLimitReached) {
			return nil, l.limitError
		}
		return nil, err
	}
	// When the client connection is accepted, and under the limit, it is wrapped to
	// ensure the close function will call the free function to decrement the limit
	// for the client to accept future connections.
	return Wrap(conn, free), nil
}
