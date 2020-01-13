# Go Server Client Connection Tracking

This package provides a library for network servers to track how many
concurrent connections they have from a given client address.

It's designed to be very simple and shared between several HashiCorp products
that provide network servers and need this kind of control to impose limits on
the resources that can be consumed by a single client.

## Usage

### TCP Server

```
// During server setup:
s.limiter = NewLimiter(Config{
  MaxConnsPerClientIP: 10,
})

```

```
// handleConn is called in it's own goroutine for each net.Conn accepted by the
// a net.Listener.
func (s *Server) handleConn(conn net.Conn) {
  defer conn.Close()

  // Track the connection
  free, err := s.limiter.Accept(conn)
  if err != nil {
    // Not accepted as limit has been reached (or some other error), log error
    // or warning and close.

    // The standard err.Error() message when limit is reached is generic so it
    // doesn't leak information which may potentially be sensitive (e.g. current
    // limits set or number of connections). This also allows comparison to
    // ErrPerClientIPLimitReached if it's important to handle it differently
    // from an internal library or io error (currently not possible but might be
    // in the future if additional functionality is added).

    // If you would like to log more information about the current limit that
    // can be obtained with s.limiter.Config().
    return
  }
  // Defer a call to free to decrement the counter for this client IP once we
  // are done with this conn.
  defer free()


  // Handle the conn
}
```

### HTTP Server

```
lim := NewLimiter(Config{
  MaxConnsPerClientIP: 10,
})
s := http.Server{
  // Other config here
  ConnState: lim.HTTPConnStateFunc(),
}
```

### Dynamic Configuration

The limiter supports dynamic reconfiguration. At any time, any goroutine may
call `limiter.SetConfig(c Config)` which will atomically update the config. All
subsequent calls to `Accept` will use the newly configured limits in their
decisions and calls to `limiter.Config()` will return the new config.