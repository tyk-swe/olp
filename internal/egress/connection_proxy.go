package egress

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

type connectionDialer struct {
	policy                     Policy
	connectTimeout, tlsTimeout time.Duration
	proxy                      *url.URL
	roots                      *x509.CertPool
	targetTLS                  *tls.Config
	credentials                connectionSecret
}

func (d connectionDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("provider connections require TCP")
	}
	ctx, release := connectionRequestContext(ctx)
	defer release()
	ctx, cancel := context.WithTimeout(ctx, d.connectTimeout)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("provider connection address is invalid")
	}
	addresses, err := d.policy.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	var last error
	for _, address := range addresses {
		// A proxy receives an IP address only. Passing a hostname here would
		// delegate DNS to the proxy and bypass the destination egress policy.
		target := net.JoinHostPort(address.String(), port)
		var connection net.Conn
		if d.proxy == nil {
			connection, err = dialer.DialContext(ctx, "tcp", target)
		} else {
			connection, err = d.tunnel(ctx, dialer, target)
		}
		if err == nil {
			return connection, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, last
}

// Bind both proxy negotiation and the provider TLS handshake to the request
// that initiated the dial. net/http's detached dial context preserves values.
func connectionRequestContext(ctx context.Context) (context.Context, func()) {
	bound, cancel := context.WithCancel(ctx)
	if original, ok := ctx.Value(connectionRequestContextKey{}).(context.Context); ok {
		stop := context.AfterFunc(original, cancel)
		if original.Err() != nil {
			cancel()
		}
		return bound, func() { stop(); cancel() }
	}
	return bound, cancel
}

func (d connectionDialer) DialTLSContext(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, release := connectionRequestContext(ctx)
	defer release()
	raw, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		raw.Close()
		return nil, errors.New("provider TLS address is invalid")
	}
	configuration := d.targetTLS.Clone()
	configuration.ServerName = host
	secured := tls.Client(raw, configuration)
	handshake, cancel := context.WithTimeout(ctx, d.tlsTimeout)
	defer cancel()
	if err = secured.HandshakeContext(handshake); err != nil {
		raw.Close()
		return nil, err
	}
	return secured, nil
}

func (d connectionDialer) tunnel(ctx context.Context, dialer *net.Dialer, target string) (net.Conn, error) {
	addresses, err := d.policy.Resolve(ctx, d.proxy.Hostname())
	if err != nil {
		return nil, err
	}
	port := d.proxy.Port()
	if port == "" {
		switch d.proxy.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		case "socks5":
			port = "1080"
		}
	}
	var raw net.Conn
	for _, address := range addresses {
		raw, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(address.String(), port))
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	// Bound negotiation and release a socket blocked in reads/writes on
	// cancellation. Only the immutable raw socket is captured by the callback.
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	failed := true
	defer func() {
		if failed {
			_ = raw.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err = raw.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	var connection net.Conn = raw
	if d.proxy.Scheme == "https" {
		secured := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: d.roots, ServerName: d.proxy.Hostname()})
		tlsCtx, cancel := context.WithTimeout(ctx, d.tlsTimeout)
		err = secured.HandshakeContext(tlsCtx)
		cancel()
		if err != nil {
			return nil, err
		}
		connection = secured
	}
	if d.proxy.Scheme == "socks5" {
		err = socksConnect(connection, target, d.credentials)
	} else {
		connection, err = httpConnect(connection, target, d.credentials)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if !stop() {
		return nil, ctx.Err()
	}
	if err = raw.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	failed = false
	return connection, nil
}

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConnection) Read(data []byte) (int, error) { return c.reader.Read(data) }

func httpConnect(connection net.Conn, target string, credentials connectionSecret) (net.Conn, error) {
	request := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if credentials.ProxyUsername != "" {
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials.ProxyUsername+":"+credentials.ProxyPassword)))
	}
	if err := request.Write(connection); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(connection, 4096)
	var header bytes.Buffer
	lineLength := 0
	for {
		line, err := reader.ReadSlice('\n')
		if header.Len()+len(line) > 32<<10 {
			return nil, errors.New("proxy response headers exceed 32 KiB")
		}
		header.Write(line)
		lineLength += len(line)
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			return nil, errors.New("proxy returned an incomplete CONNECT response")
		}
		if lineLength <= 2 && (bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n"))) {
			break
		}
		if err == nil {
			lineLength = 0
		}
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header.Bytes())), request)
	if err != nil {
		return nil, errors.New("proxy returned an invalid CONNECT response")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy refused CONNECT with status %d", response.StatusCode)
	}
	return &bufferedConnection{Conn: connection, reader: reader}, nil
}

func socksConnect(connection net.Conn, target string, credentials connectionSecret) error {
	method := byte(0)
	if credentials.ProxyUsername != "" {
		method = 2
	}
	if _, err := connection.Write([]byte{5, 1, method}); err != nil {
		return err
	}
	var reply [4]byte
	if _, err := io.ReadFull(connection, reply[:2]); err != nil {
		return err
	}
	if reply[0] != 5 || reply[1] != method {
		return errors.New("SOCKS5 proxy did not accept the configured authentication method")
	}
	if method == 2 {
		packet := append([]byte{1, byte(len(credentials.ProxyUsername))}, credentials.ProxyUsername...)
		packet = append(packet, byte(len(credentials.ProxyPassword)))
		packet = append(packet, credentials.ProxyPassword...)
		if _, err := connection.Write(packet); err != nil {
			return err
		}
		if _, err := io.ReadFull(connection, reply[:2]); err != nil {
			return err
		}
		if reply[0] != 1 || reply[1] != 0 {
			return errors.New("SOCKS5 proxy authentication failed")
		}
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return errors.New("invalid SOCKS5 target")
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return errors.New("SOCKS5 target must be a locally validated IP address")
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return errors.New("invalid SOCKS5 target port")
	}
	packet := []byte{5, 1, 0}
	address = address.Unmap()
	if address.Is4() {
		packet = append(packet, 1)
	} else {
		packet = append(packet, 4)
	}
	packet = append(packet, address.AsSlice()...)
	packet = binary.BigEndian.AppendUint16(packet, uint16(portNumber))
	if _, err := connection.Write(packet); err != nil {
		return err
	}
	if _, err := io.ReadFull(connection, reply[:]); err != nil {
		return err
	}
	if reply[0] != 5 || reply[1] != 0 || reply[2] != 0 {
		return errors.New("SOCKS5 proxy refused CONNECT")
	}
	length := 0
	switch reply[3] {
	case 1:
		length = 4
	case 4:
		length = 16
	case 3:
		if _, err := io.ReadFull(connection, reply[:1]); err != nil {
			return err
		}
		length = int(reply[0])
	default:
		return errors.New("SOCKS5 proxy returned an invalid address type")
	}
	_, err = io.CopyN(io.Discard, connection, int64(length+2))
	return err
}
