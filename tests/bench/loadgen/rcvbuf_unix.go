//go:build bench && unix

package loadgen

import "syscall"

// limitReceiveBuffer returns a dialer control that sets each connection's
// receive buffer before it connects, so the kernel advertises a small window
// and a slow reader leaves its data with the server.
func limitReceiveBuffer(size int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var setErr error
		if err := c.Control(func(fd uintptr) {
			setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, size)
		}); err != nil {
			return err
		}
		return setErr
	}
}
