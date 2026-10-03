//go:build bench && !unix

package loadgen

import "syscall"

// limitReceiveBuffer does nothing where socket options are not portable: a
// slow reader then reads slowly into a default-sized buffer.
func limitReceiveBuffer(int) func(network, address string, c syscall.RawConn) error { return nil }
