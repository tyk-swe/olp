package egress

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/textproto"
	"strings"
)

// Raw clients use a fresh HTTP/1 connection. Capture its initial response
// Connection values before net/http consumes them, without changing wire bytes.
// The transport's response-header limit also bounds this temporary buffer.
type connectionHeaderConn struct {
	net.Conn
	header      []byte
	scan, line  int
	done        bool
	connections []string
}

func captureConnectionHeaders(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &connectionHeaderConn{Conn: connection}, nil
	}
}

func (c *connectionHeaderConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if !c.done {
		c.observe(p[:n])
	}
	return n, err
}

func (c *connectionHeaderConn) observe(p []byte) {
	c.header = append(c.header, p...)
	for c.scan < len(c.header) {
		end := c.scan + 1
		if c.header[c.scan] != '\n' {
			c.scan = end
			continue
		}
		blank := c.scan == c.line || c.scan == c.line+1 && c.header[c.line] == '\r'
		c.scan, c.line = end, end
		if !blank {
			continue
		}
		reader := textproto.NewReader(bufio.NewReader(bytes.NewReader(c.header[:end])))
		status, _ := reader.ReadLine()
		fields := strings.Fields(status)
		if len(fields) >= 2 && len(fields[1]) == 3 && fields[1][0] == '1' && fields[1] != "101" {
			// Informational responses precede the final HTTP/1 header block.
			c.header = append(c.header[:0], c.header[end:]...)
			c.scan, c.line = 0, 0
			continue
		}
		if headers, err := reader.ReadMIMEHeader(); err == nil {
			c.connections = headers.Values("Connection")
		}
		c.header, c.done = nil, true
		return
	}
}
