// Package egress validates provider destinations and builds bounded outbound
// HTTP clients whose dials are pinned to validated addresses.
package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// ErrUnsafeDestination is returned for addresses outside the public internet
// unless an operator exception covers them.
var ErrUnsafeDestination = errors.New("destination address is not allowed for provider egress")

// ErrRedirect is returned when an upstream answers with a redirect; the
// gateway never follows one because the target was validated ahead of time.
var ErrRedirect = errors.New("upstream redirects are refused")

// Policy holds the operator exceptions applied to every provider destination.
type Policy struct {
	AllowedNetworks []netip.Prefix
	PlainHTTPHosts  []string
}

// ValidateEndpoint checks and normalizes a provider base URL before storage or dispatch:
// HTTPS unless the host is an explicit plain-HTTP exception, no credentials,
// no query or fragment, and IP literals must be permitted by the policy.
func (p Policy) ValidateEndpoint(raw string) (*url.URL, error) {
	if len(raw) > 2048 || strings.TrimSpace(raw) != raw || raw == "" {
		return nil, errors.New("endpoint must be an absolute URL of at most 2048 characters without surrounding whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !u.IsAbs() {
		return nil, errors.New("endpoint must be an absolute http(s) URL")
	}
	host := strings.ToLower(u.Hostname())
	switch u.Scheme {
	case "https":
	case "http":
		if !p.PermitsPlainHTTP(host) {
			return nil, errors.New("endpoint must use https unless the host is listed in OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS")
		}
	default:
		return nil, errors.New("endpoint must use https")
	}
	if u.User != nil {
		return nil, errors.New("endpoint must not embed credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return nil, errors.New("endpoint must not carry a query or fragment")
	}
	if strings.ContainsAny(host, " \t\r\n") || strings.Contains(u.Path, "..") {
		return nil, errors.New("endpoint host or path is malformed")
	}
	if port := u.Port(); port != "" {
		if _, err := net.LookupPort("tcp", port); err != nil {
			return nil, errors.New("endpoint port is invalid")
		}
	}
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if !p.Permits(addr) {
			return nil, ErrUnsafeDestination
		}
	} else if !validHostname(host) {
		return nil, errors.New("endpoint host is not a valid DNS name")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func validHostname(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// PermitsPlainHTTP reports whether an operator listed the host for plain HTTP.
func (p Policy) PermitsPlainHTTP(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	for _, allowed := range p.PlainHTTPHosts {
		if strings.EqualFold(allowed, host) {
			return true
		}
	}
	return false
}

// Permits reports whether egress may reach the address: public unicast, or
// covered by an operator exception.
func (p Policy) Permits(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range p.AllowedNetworks {
		if prefix.Addr().Is4() == addr.Is4() && prefix.Contains(addr) {
			return true
		}
	}
	return !Blocked(addr)
}

// Blocked reports whether the address lies outside publicly routable unicast
// space. The table mirrors the reference gateway's denylist.
func Blocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	if addr.Is4() {
		for _, prefix := range blockedV4 {
			if prefix.Contains(addr) {
				return true
			}
		}
		return false
	}
	if addr.Is4In6() || addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.IsPrivate() {
		return true
	}
	for _, prefix := range blockedV6 {
		if prefix.Contains(addr) {
			return true
		}
	}
	// Only the currently allocated global unicast range is reachable.
	return !netip.MustParsePrefix("2000::/3").Contains(addr)
}

var blockedV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
}

var blockedV6 = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// Resolve answers the addresses the dialer may use for host. Every DNS answer
// must be permitted; one unsafe answer rejects the whole destination so a
// rebinding resolver cannot smuggle a private address into a public name.
func (p Policy) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.Trim(strings.ToLower(host), "[]")
	if addr, err := netip.ParseAddr(host); err == nil {
		if !p.Permits(addr) {
			return nil, ErrUnsafeDestination
		}
		return []netip.Addr{addr.Unmap()}, nil
	}
	answers, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	addrs := make([]netip.Addr, 0, len(answers))
	for _, addr := range answers {
		addr = addr.Unmap()
		if !p.Permits(addr) {
			return nil, ErrUnsafeDestination
		}
		addrs = append(addrs, addr)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	return addrs, nil
}

// Transport builds an outbound transport whose dials are pinned to validated
// addresses and whose per-phase limits are explicit. Response body limits are
// applied by callers because unary and streaming reads differ.
func (p Policy) Transport(responseHeaderTimeout time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addrs, err := p.Resolve(ctx, host)
			if err != nil {
				return nil, err
			}
			var last error
			for _, addr := range addrs {
				conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), port))
				if err == nil {
					return conn, nil
				}
				last = err
				if ctx.Err() != nil {
					break
				}
			}
			return nil, last
		},
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  responseHeaderTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 32 << 10,
		MaxIdleConnsPerHost:    16,
		IdleConnTimeout:        90 * time.Second,
		ForceAttemptHTTP2:      true,
	}
}

// Client wraps Transport with redirect refusal.
func (p Policy) Client(responseHeaderTimeout time.Duration) *http.Client {
	return &http.Client{
		Transport: p.Transport(responseHeaderTimeout),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrRedirect
		},
	}
}
