package adb

import (
	"net"
	"sync"
)

type writeGuardConn struct {
	net.Conn
	authorize func() error
	dispatch  sync.Mutex
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// GuardWrites serializes authorization with writes and joins in-flight writes
// after closing the underlying socket. The authorizer must not perform I/O.
func GuardWrites(conn net.Conn, authorize func() error) net.Conn {
	return &writeGuardConn{Conn: conn, authorize: authorize}
}

func (c *writeGuardConn) Write(data []byte) (int, error) {
	c.dispatch.Lock()
	defer c.dispatch.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	if err := c.authorize(); err != nil {
		return 0, err
	}
	return c.Conn.Write(data)
}

func (c *writeGuardConn) Close() error {
	c.closeOnce.Do(func() {
		// Interrupt a blocked syscall before joining the dispatch fence. Waiting
		// for the write lock first would deadlock cancellation behind a stalled peer.
		c.closeErr = c.Conn.Close()
		c.dispatch.Lock()
		c.closed = true
		c.dispatch.Unlock()
	})
	return c.closeErr
}
