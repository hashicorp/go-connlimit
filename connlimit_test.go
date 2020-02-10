package connlimit

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func runLimitedServer(t *testing.T, lim *Limiter, connTime time.Duration) (string, func()) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	serverCtx, cancel := context.WithCancel(context.Background())

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				// Ignore errors...
				continue
			}
			go func() {
				defer conn.Close()

				free, err := lim.Accept(conn)
				if err != nil {
					return
				}
				defer free()

				ctx, cancel := context.WithCancel(serverCtx)
				defer cancel()

				// Send something back so we can be sure when clients have successfully
				// connected.
				_, err = conn.Write([]byte("Hello"))
				require.NoError(t, err)

				// Start a reading loop so that we detect of the client has closed the
				// conn.
				go func() {
					for {
						bs := make([]byte, 10)
						_, err := conn.Read(bs)
						if err != nil {
							cancel()
							return
						}
						if ctx.Err() != nil {
							return
						}
					}
				}()

				select {
				case <-ctx.Done():
					// Either server stopped or client went away
					return
				case <-time.After(connTime):
					return
				}
			}()
		}
	}()

	return l.Addr().String(), func() {
		l.Close()
		cancel()
	}
}

func clientRead(t *testing.T, serverAddr string) (net.Conn, string, error) {
	t.Helper()

	conn, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)

	// Prevent the test from hanging if the server somehow doesn't send but also
	// doesn't close the conn.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))

	buf := make([]byte, 10)
	n, err := conn.Read(buf)

	// Return the conn to keep it open so test can control when it closes.
	return conn, string(buf[0:n]), err
}

func TestLimiterDenies(t *testing.T) {
	lim := NewLimiter(Config{
		MaxConnsPerClientIP: 2,
	})

	serverAddr, serverClose := runLimitedServer(t, lim, 10*time.Minute)
	defer serverClose()

	// Two clients should be able to connect
	conn1, got, err := clientRead(t, serverAddr)
	require.NoError(t, err)
	require.Equal(t, "Hello", got)
	defer conn1.Close()

	conn2, got, err := clientRead(t, serverAddr)
	require.NoError(t, err)
	require.Equal(t, "Hello", got)
	defer conn2.Close()

	// Third client should fail (but only _after_ TCP conn is accepted)
	conn3, _, err := clientRead(t, serverAddr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "EOF")
	defer conn3.Close()

	// Close one of the first clients
	conn1.Close()

	attempts := 0
	for {
		// Now another should be allowed, but it might take a few cycles for the
		// server to notice the conn closing.
		conn4, got, err := clientRead(t, serverAddr)
		if err != nil {
			attempts++
			if attempts < 10 {
				// Failed, possibly server didn't finishing cleaning up closed conn yet.
				time.Sleep(20 * time.Millisecond)
				continue
			}
		}
		require.NoError(t, err, "attempt %d", attempts)
		require.Equal(t, "Hello", got)
		// Defer will not fire til the end of the test which is what we intend as we
		// want this conn to stay alive and are just about to exit this loop and
		// continue.
		defer conn4.Close()
		// If we made it here we succeeded.
		break
	}

	// Reset the limit
	lim.SetConfig(Config{
		MaxConnsPerClientIP: 3,
	})

	// Should be able to add another new connection now
	conn5, got, err := clientRead(t, serverAddr)
	require.NoError(t, err)
	require.Equal(t, "Hello", got)
	defer conn5.Close()

	require.Equal(t, 3, lim.NumOpen(conn5.RemoteAddr()))

	// But no more than that
	conn6, _, err := clientRead(t, serverAddr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "EOF")
	defer conn6.Close()
}

// TestLimiterConcurrencyFuzz is a probabalistic sanity check. It doesn't prove
// anything is correct as such but does exercise the limiter with concurrent
// requests. Running it with race detector enabled gives some confidence that
// the limiter is correctly handling synchronization.
func TestLimiterConcurrencyFuzz(t *testing.T) {
	lim := NewLimiter(Config{
		MaxConnsPerClientIP: 5,
	})

	// Run 1000 clients spread across 100 IP addresses
	n := 1000
	m := 100

	var wg sync.WaitGroup

	var denied uint64

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			addr := fmt.Sprintf("10.10.10.%d", i%m)
			conn := newTestConn(addr)
			free, err := lim.Accept(conn)
			if err != nil {
				// Track the number of denied connection attempts
				atomic.AddUint64(&denied, 1)
				return
			}
			defer free()

			// If we were successful wait for 1 second to prevent others from
			// succeeding.
			time.Sleep(1 * time.Second)
		}(i)
	}

	// Wait for all the clients to complete
	wg.Wait()

	// Sanity check the right number were denied.
	require.Equal(t, 500, int(atomic.LoadUint64(&denied)))
}

// testConn implements net.Conn and net.Addr to make a simple way to simulate
// connections from many client IPs without messing with the host's IP stack for
// real. Goroutines may write to `in` or read from `out` to simulate the client
// end of the conn.
type testConn struct {
	remoteAddr string
	in         io.ReadCloser
	out        io.WriteCloser
}

func newTestConn(addr string) *testConn {
	in, out := io.Pipe()
	return &testConn{
		remoteAddr: addr,
		in:         in,
		out:        out,
	}
}

// Network implements net.Addr
func (c *testConn) Network() string {
	return "tcp"
}

// String implements net.Addr
func (c *testConn) String() string {
	return c.remoteAddr
}

// Read implements net.Conn
func (c *testConn) Read(b []byte) (n int, err error) {
	return c.in.Read(b)
}

// Write implements net.Conn
func (c *testConn) Write(b []byte) (n int, err error) {
	return c.out.Write(b)
}

// Close implements net.Conn
func (c *testConn) Close() error {
	c.in.Close()
	return c.out.Close()
}

// LocalAddr implements net.Conn
func (c *testConn) LocalAddr() net.Addr {
	return &net.TCPAddr{
		IP:   net.IP{0x7f, 0x0, 0x0, 0x1}, // 127.0.0.1
		Port: 31245,
	}
}

// RemoteAddr implements net.Conn
func (c *testConn) RemoteAddr() net.Addr {
	return c // testConn implements net.Addr too
}

// Set*Deadline implements net.Conn
func (c *testConn) SetDeadline(t time.Time) error      { return nil }
func (c *testConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *testConn) SetWriteDeadline(t time.Time) error { return nil }

func TestHTTPServer(t *testing.T) {
	lim := NewLimiter(Config{
		MaxConnsPerClientIP: 5,
	})

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1 * time.Second)
		w.Write([]byte("OK"))
	}))

	srv.Config.ConnState = lim.HTTPConnStateFunc()
	srv.Start()

	client := srv.Client()

	// Use only http not https to force HTTP/1.1. That means each request will
	// consume a separate TCP conn as they all take 1 second. If we use https it
	// would automatically switch to http2 and multiplex the requests over a
	// single conn bypassing the limiter. This is current behavior but add this
	// assertion to check it stays that way.
	require.True(t, strings.HasPrefix(srv.URL, "http://"))

	// Start 10 requests concurrently so they use 10 different conns.
	var wg sync.WaitGroup
	var reset uint64
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.Get(srv.URL)
			if err != nil {
				if !strings.Contains(err.Error(), "server closed idle connection") &&
					!strings.Contains(err.Error(), "connection reset") &&
					!strings.Contains(err.Error(), "EOF") {
					require.NoError(t, err, "unexpected error type")
				}
				atomic.AddUint64(&reset, 1)
			} else {
				resp.Body.Close()
			}
		}(i)
	}

	wg.Wait()

	// 5 of the 10 requests should have had connections reset due to limit.
	require.Equal(t, 5, int(atomic.LoadUint64(&reset)))
}
