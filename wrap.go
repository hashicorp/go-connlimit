package connlimit

import "net"

type wrappedConn struct {
	net.Conn
	free func()
}

// Wrap wraps a net.Conn's Close method so free() is called when Close is
// called. Useful when handing off tracked connections to libraries that close
// them.
func Wrap(conn net.Conn, free func()) net.Conn {
	return wrappedConn{
		Conn: conn,
		free: free,
	}
}

// Close frees the tracked connection and closes the underlying net.Conn.
func (w wrappedConn) Close() error {
	w.free()
	return w.Conn.Close()
}
