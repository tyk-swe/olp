package access

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func oidcURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || len(raw) > 2048 {
		return errors.New("invalid OIDC URL")
	}
	if u.Scheme != "https" && !(oidcTestBuild && u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())) {
		return errors.New("OIDC endpoints must use HTTPS; insecure loopback requires an oidctest build")
	}
	return nil
}
func oidcAddressAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if oidcTestBuild && ip.IsLoopback() {
		return true
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32"} {
		if netip.MustParsePrefix(block).Contains(ip) {
			return false
		}
	}
	return true
}
func oidcAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("OIDC host lookup failed")
	}
	for _, ip := range addresses {
		if !oidcAddressAllowed(ip) {
			return nil, errors.New("OIDC address is outside the allowed egress policy")
		}
	}
	return addresses, nil
}
func oidcHTTPClient() *http.Client {
	transport := &http.Transport{TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 32768, MaxIdleConns: 10, MaxConnsPerHost: 8, IdleConnTimeout: time.Minute, ForceAttemptHTTP2: true}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid OIDC address")
		}
		addresses, err := oidcAddresses(ctx, host)
		if err != nil {
			return nil, err
		}
		dialer := net.Dialer{}
		return oidcDialAddresses(ctx, network, port, addresses, dialer.DialContext)
	}
	return &http.Client{Transport: oidcTransport{transport}, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("OIDC HTTP redirects are not allowed") }}
}

// Dial only the validated addresses, sharing the connection timeout across them.
func oidcDialAddresses(ctx context.Context, network, port string, addresses []netip.Addr, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var lastErr error
	for i, ip := range addresses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attempt, cancel := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(addresses)-i))
		conn, err := dial(attempt, network, net.JoinHostPort(ip.String(), port))
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

type oidcTransport struct{ base http.RoundTripper }

func (t oidcTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := oidcURL(r.URL.String()); err != nil {
		return nil, err
	}
	response, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, errors.New("OIDC endpoint request failed")
	}
	if response.ContentLength > 1<<20 {
		response.Body.Close()
		return nil, errors.New("OIDC response is too large")
	}
	response.Body = &oidcBody{ReadCloser: response.Body, remaining: 1 << 20}
	return response, nil
}

type oidcBody struct {
	io.ReadCloser
	remaining int64
}

func (b *oidcBody) Read(p []byte) (int, error) {
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return 0, errors.New("OIDC response is too large")
	}
	return n, err
}
func safeReturn(value string) bool {
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "/") {
		return false
	}
	// Check escaped input for controls, backslashes, and authority prefixes,
	// while preserving safe query escapes in the original redirect destination.
	decoded, err := url.PathUnescape(value)
	if err != nil || !utf8.ValidString(decoded) || strings.HasPrefix(decoded, "//") || strings.Contains(decoded, "\\") || strings.IndexFunc(decoded, unicode.IsControl) >= 0 {
		return false
	}
	path, err := url.Parse(value)
	if err != nil {
		return false
	}
	base := url.URL{Scheme: "https", Host: "console.invalid", Path: "/"}
	destination := base.ResolveReference(path)
	return destination.Scheme == base.Scheme && destination.Host == base.Host && destination.User == nil
}

// OIDCTestBuild is false in ordinary and release binaries. Provider egress
// flags never affect this separately constructed identity HTTP client.
func OIDCTestBuild() bool { return oidcTestBuild }
