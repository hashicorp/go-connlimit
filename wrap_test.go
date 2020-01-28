package connlimit

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWrapClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	limit := 3

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	errCh := make(chan error, 1)
	connCh := make(chan net.Conn, 1)
	go func() {
		defer l.Close()

		lim := NewLimiter(Config{
			MaxConnsPerClientIP: limit,
		})

		for {
			conn, err := l.Accept()
			if err != nil {
				errCh <- err
				return
			}

			free, err := lim.Accept(conn)
			if err != nil {
				errCh <- err
				return
			}

			conn = Wrap(conn, free)
			select {
			case connCh <- conn:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Max out connections
	conns := make([]net.Conn, 0, limit)
	for i := 0; i < limit; i++ {
		conn, err := net.DialTimeout("tcp", l.Addr().String(), 2*time.Second)
		require.NoError(t, err)
		defer conn.Close()

		select {
		case c := <-connCh:
			conns = append(conns, c)
			defer c.Close()
		case err := <-errCh:
			t.Fatalf("error from server goroutine: %v", err)
		}
	}

	// Close one and assert it was freed by creating a new connection
	require.NoError(t, conns[0].Close())

	conn, err := net.DialTimeout("tcp", l.Addr().String(), 2*time.Second)
	require.NoError(t, err)
	defer conn.Close()

	select {
	case c := <-connCh:
		// Yay! Connected as expected!
		c.Close()
	case err := <-errCh:
		// An error from the limiter here indicates a problem with the
		// wrapper calling free()
		t.Fatalf("error from server goroutine: %v", err)
	}
}

// TestWrap_SubLimit asserts that already wrapped connections may be passed to
// a sub-limiter.
func TestWrap_SubLimit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	errCh := make(chan error, 1)
	connCh := make(chan net.Conn, 1)
	go func() {
		defer l.Close()

		for {
			conn, err := l.Accept()
			if err != nil {
				errCh <- err
				return
			}

			select {
			case connCh <- conn:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Make a "real" conn
	clientConn, err := net.DialTimeout("tcp", l.Addr().String(), 2*time.Second)
	require.NoError(t, err)
	defer clientConn.Close()

	// Limit it
	var serverConn net.Conn
	select {
	case serverConn = <-connCh:
	case err = <-errCh:
		t.Fatalf("error from listener: %v", err)
	}
	parent := NewLimiter(Config{
		MaxConnsPerClientIP: 5,
	})
	child := NewLimiter(Config{
		MaxConnsPerClientIP: 3,
	})

	freeParent, err := parent.Accept(serverConn)
	require.NoError(t, err)

	wrapParent := Wrap(serverConn, freeParent)
	defer wrapParent.Close()

	freeChild, err := child.Accept(wrapParent)
	require.NoError(t, err)

	// Success!
	freeChild()
}
