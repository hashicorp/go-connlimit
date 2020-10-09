package connlimit

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func runLimitedServerWithLimitedListener(t *testing.T, ln *Listener, connTime time.Duration) func() {
	t.Helper()

	serverCtx, cancel := context.WithCancel(context.Background())

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				continue
			}
			go func() {
				defer conn.Close()

				ctx, cancel := context.WithCancel(serverCtx)
				defer cancel()

				// Send something back so we can be sure when clients have successfully
				// connected.
				conn.Write([]byte("Hello"))

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

	return func() {
		cancel()
	}
}

func runLimitedgRPCServerWithLimitedListener(t *testing.T, ln *Listener, connTime time.Duration) {
	t.Helper()

	srv := grpc.NewServer()

	go func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, io.EOF) {
			panic(err)
		}
	}()

	go func() {
		defer srv.Stop()
		<-time.After(connTime)
	}()
}

func runLimitedHTTPServerWithLimitedListener(t *testing.T, ln *Listener, connTime time.Duration) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	})

	go func() {
		err := http.Serve(ln, mux)
		if err != nil && !errors.Is(err, ErrPerClientIPLimitReached) {
			panic(err)
		}
	}()

	go func() {
		defer ln.Close()
		<-time.After(connTime)
	}()
}

func TestNewListener(t *testing.T) {
	limLn, err := NewListener()
	require.NoError(t, err)

	serverAddr := limLn.Addr().String()
	serverClose := runLimitedServerWithLimitedListener(t, limLn, 10*time.Minute)
	defer serverClose()

	// Two clients should be able to connect
	conn1, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)
	buff1 := make([]byte, 5)
	_, err = conn1.Read(buff1)
	require.NoError(t, err)
	require.Equal(t, "Hello", string(buff1))
	defer conn1.Close()

	conn2, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)
	buff2 := make([]byte, 5)
	_, err = conn2.Read(buff2)
	require.NoError(t, err)
	require.Equal(t, "Hello", string(buff2))
	defer conn2.Close()

	// Third client should fail (but only _after_ TCP conn is accepted)
	conn3, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)
	buff3 := make([]byte, 5)
	_, err = conn3.Read(buff3)
	require.Error(t, err)
	require.Contains(t, err.Error(), "EOF")
	defer conn3.Close()
}

func TestNewListener_gRPCServer(t *testing.T) {
	limLn, err := NewListener(
		WithLimitError(io.EOF), // required to provide a known error to the gRPC server
	)
	require.NoError(t, err)

	serverAddr := limLn.Addr().String()

	t.Logf("created new limited network litener %q", serverAddr)

	t.Logf("starting grpc server")
	runLimitedgRPCServerWithLimitedListener(t, limLn, 10*time.Minute)

	// Two clients should be able to connect
	t.Log("making first client connection")
	conn1, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)
	defer conn1.Close()
	buff1 := make([]byte, 1)
	_, err = conn1.Read(buff1)
	require.NoError(t, err)
	t.Log("read from first client connection")
	t.Log("holding first client connection")

	t.Log("making second client connection")
	conn2, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)
	buff2 := make([]byte, 1)
	_, err = conn2.Read(buff2)
	require.NoError(t, err)
	defer conn2.Close()
	t.Log("read from second client connection")
	t.Log("holding second client connection")

	// Third client should fail (but only _after_ TCP conn is accepted)
	t.Log("making third second connection")
	conn3, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)
	defer conn3.Close()
	buff := make([]byte, 1)
	t.Log("trying to read from third second connection")
	_, err = conn3.Read(buff)
	require.Error(t, err)
	require.Contains(t, err.Error(), "EOF")
	t.Log("but failed, as expected, because it exceeded the limit")

	// and the two clients should be able to read again
	buff1 = make([]byte, 1)
	_, err = conn1.Read(buff1)
	require.NoError(t, err)
	t.Log("read from first client connection again")

	buff2 = make([]byte, 1)
	_, err = conn2.Read(buff2)
	require.NoError(t, err)
	t.Log("read from second client connection again")

	// and also write back
	_, err = conn1.Write(buff1)
	require.NoError(t, err)
	t.Log("wrote to first client connection")

	_, err = conn2.Write(buff2)
	require.NoError(t, err)
	t.Log("wrote to second client connection")
}

func TestNewListener_HTTPServer(t *testing.T) {
	limLn, err := NewListener()
	require.NoError(t, err)

	serverAddr := limLn.Addr().String()

	t.Logf("created new limited network litener %q", serverAddr)

	t.Logf("starting http server")
	runLimitedHTTPServerWithLimitedListener(t, limLn, 10*time.Minute)

	// Two clients should be able to connect
	t.Log("making first client connection")
	conn1, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)
	defer conn1.Close()
	t.Log("holding first client connection")

	t.Log("making second client connection")
	conn2, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)
	defer conn2.Close()
	t.Log("holding second client connection")

	// Third client should fail (but only _after_ TCP conn is accepted)
	t.Log("making third second connection")
	conn3, err := net.Dial("tcp", serverAddr)
	require.NoError(t, err)
	defer conn3.Close()
	buff := make([]byte, 1)
	t.Log("trying to read from third second connection")
	_, err = conn3.Read(buff)
	require.Error(t, err)
	require.Contains(t, err.Error(), "EOF")
	t.Log("but failed, as expected, because it exceeded the limit")
}
