//go:build bench && unix

package loadgen

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"testing"
)

// TestSlowReaderConnectionsHaveASmallReceiveBuffer checks the option takes
// effect on a real connection: without it the kernel would absorb a whole
// short stream and the server would never see the reader's pace.
func TestSlowReaderConnectionsHaveASmallReceiveBuffer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	buffer := func(dial func(context.Context, string, string) (net.Conn, error)) int {
		conn, err := dial(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		raw, err := conn.(*net.TCPConn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var size int
		raw.Control(func(fd uintptr) { size, err = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF) })
		if err != nil {
			t.Fatal(err)
		}
		return size
	}
	// The client of a run with slow readers dials with the small buffer; the
	// client of an ordinary run does not.
	dialer := func(cfg Config) func(context.Context, string, string) (net.Conn, error) {
		return newClient(cfg).Transport.(*http.Transport).DialContext
	}
	small := buffer(dialer(Config{MaxInFlight: 4, SlowRead: SlowRead{BytesPerSecond: 100, RcvBuf: DefaultSlowReadRcvBuf}}))
	normal := buffer(dialer(Config{MaxInFlight: 4}))
	// Linux reports twice the requested size, with a floor.
	if small > 16*1024 || normal < 4*small {
		t.Fatalf("receive buffer %d bytes for a slow reader and %d for an ordinary one", small, normal)
	}
}
