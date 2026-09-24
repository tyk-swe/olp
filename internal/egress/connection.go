package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ConnectionOptions configures one provider connection. CredentialID identifies
// an encrypted network credential; secret values never belong in this document.
// All proxy destinations are resolved and validated locally before tunneling.
// HTTP proxies must support CONNECT even for an HTTP destination.
type ConnectionOptions struct {
	ProxyURL                string `json:"proxy_url,omitempty"`
	TrustRootsPEM           string `json:"trust_roots_pem,omitempty"`
	CredentialID            string `json:"credential_id,omitempty"`
	ConnectTimeoutMS        *int64 `json:"connect_timeout_ms,omitempty"`
	TLSHandshakeTimeoutMS   *int64 `json:"tls_handshake_timeout_ms,omitempty"`
	ResponseHeaderTimeoutMS *int64 `json:"response_header_timeout_ms,omitempty"`
	IdleConnTimeoutMS       *int64 `json:"idle_conn_timeout_ms,omitempty"`
	MaxIdleConns            *int   `json:"max_idle_conns,omitempty"`
	MaxIdleConnsPerHost     *int   `json:"max_idle_conns_per_host,omitempty"`
	MaxConnsPerHost         *int   `json:"max_conns_per_host,omitempty"`
}

type connectionSecret struct {
	ProxyUsername        string `json:"proxy_username"`
	ProxyPassword        string `json:"proxy_password"`
	ClientCertificatePEM string `json:"client_certificate_pem"`
	ClientKeyPEM         string `json:"client_key_pem"`
}

// ValidateConnectionSecret validates plaintext obtained from the credential
// authority. Errors describe the field or constraint, never its secret value.
func ValidateConnectionSecret(secret []byte) error {
	if len(secret) == 0 {
		return errors.New("network credential must not be empty")
	}
	_, _, err := parseConnectionSecret(secret)
	return err
}

func parseConnectionSecret(secret []byte) (connectionSecret, *tls.Certificate, error) {
	var value connectionSecret
	if len(secret) == 0 {
		return value, nil, nil
	}
	if len(secret) > 256<<10 || !utf8.Valid(secret) {
		return value, nil, errors.New("network credential must be a UTF-8 JSON object of at most 256 KiB")
	}
	// Reject duplicate names and null fields: they cannot silently select a
	// different secret or turn configured authentication into an anonymous dial.
	d := json.NewDecoder(bytes.NewReader(secret))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return value, nil, errors.New("network credential must be a JSON object")
	}
	fields := map[string]string{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return value, nil, errors.New("invalid network credential JSON")
		}
		name, ok := token.(string)
		if !ok {
			return value, nil, errors.New("invalid network credential field")
		}
		if _, duplicate := fields[name]; duplicate {
			return value, nil, errors.New("duplicate network credential field")
		}
		switch name {
		case "proxy_username", "proxy_password", "client_certificate_pem", "client_key_pem":
		default:
			return value, nil, errors.New("unknown network credential field")
		}
		var field json.RawMessage
		if err := d.Decode(&field); err != nil || bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return value, nil, errors.New("network credential fields must be strings")
		}
		var text string
		if json.Unmarshal(field, &text) != nil {
			return value, nil, errors.New("network credential fields must be strings")
		}
		fields[name] = text
	}
	if _, err := d.Token(); err != nil {
		return value, nil, errors.New("invalid network credential JSON")
	}
	if _, err := d.Token(); err != io.EOF {
		return value, nil, errors.New("network credential must contain one JSON object")
	}
	value = connectionSecret{ProxyUsername: fields["proxy_username"], ProxyPassword: fields["proxy_password"], ClientCertificatePEM: fields["client_certificate_pem"], ClientKeyPEM: fields["client_key_pem"]}
	for _, field := range []string{value.ProxyUsername, value.ProxyPassword} {
		if len(field) > 4096 || strings.ContainsAny(field, "\r\n\x00") {
			return connectionSecret{}, nil, errors.New("proxy credentials must be at most 4096 bytes without line breaks or NUL")
		}
	}
	if strings.Contains(value.ProxyUsername, ":") {
		return connectionSecret{}, nil, errors.New("proxy username must not contain a colon")
	}
	if value.ProxyPassword != "" && value.ProxyUsername == "" {
		return connectionSecret{}, nil, errors.New("proxy password requires a username")
	}
	if (value.ClientCertificatePEM == "") != (value.ClientKeyPEM == "") {
		return connectionSecret{}, nil, errors.New("client certificate and private key must be supplied together")
	}
	if value.ClientCertificatePEM != "" {
		certificate, err := tls.X509KeyPair([]byte(value.ClientCertificatePEM), []byte(value.ClientKeyPEM))
		if err != nil {
			return connectionSecret{}, nil, errors.New("client certificate and private key are invalid or do not match")
		}
		return value, &certificate, nil
	}
	if value.ProxyUsername == "" {
		return connectionSecret{}, nil, errors.New("network credential must provide proxy authentication or a client certificate")
	}
	return value, nil, nil
}

// ValidateConnection validates the static connection document without making a
// DNS request or provider call. Dial-time checks validate every resolved address.
func (p Policy) ValidateConnection(options *ConnectionOptions) error {
	if options == nil {
		return nil
	}
	for _, setting := range []struct {
		name    string
		value   *int64
		maximum int64
	}{
		{"connect_timeout_ms", options.ConnectTimeoutMS, 120000},
		{"tls_handshake_timeout_ms", options.TLSHandshakeTimeoutMS, 120000},
		{"response_header_timeout_ms", options.ResponseHeaderTimeoutMS, 600000},
		{"idle_conn_timeout_ms", options.IdleConnTimeoutMS, 3600000},
	} {
		if setting.value != nil && (*setting.value < 1 || *setting.value > setting.maximum) {
			return fmt.Errorf("%s must be between 1 and %d", setting.name, setting.maximum)
		}
	}
	for _, setting := range []struct {
		name  string
		value *int
	}{
		{"max_idle_conns", options.MaxIdleConns}, {"max_idle_conns_per_host", options.MaxIdleConnsPerHost}, {"max_conns_per_host", options.MaxConnsPerHost},
	} {
		if setting.value != nil && (*setting.value < 1 || *setting.value > 4096) {
			return fmt.Errorf("%s must be between 1 and 4096", setting.name)
		}
	}
	if options.MaxIdleConns != nil && options.MaxIdleConnsPerHost != nil && *options.MaxIdleConnsPerHost > *options.MaxIdleConns {
		return errors.New("max_idle_conns_per_host must not exceed max_idle_conns")
	}
	if options.MaxConnsPerHost != nil && options.MaxIdleConnsPerHost != nil && *options.MaxIdleConnsPerHost > *options.MaxConnsPerHost {
		return errors.New("max_idle_conns_per_host must not exceed max_conns_per_host")
	}
	if len(options.CredentialID) > 128 || strings.TrimSpace(options.CredentialID) != options.CredentialID || strings.ContainsAny(options.CredentialID, "\r\n\x00") {
		return errors.New("network credential reference is invalid")
	}
	if options.ProxyURL != "" {
		if _, err := p.connectionProxy(options.ProxyURL); err != nil {
			return err
		}
	}
	if options.TrustRootsPEM != "" {
		if _, err := connectionRoots(options.TrustRootsPEM); err != nil {
			return err
		}
	}
	return nil
}

func (p Policy) connectionProxy(raw string) (*url.URL, error) {
	proxy, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("proxy_url must be an absolute HTTP, HTTPS or SOCKS5 URL")
	}
	if proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" {
		return nil, errors.New("proxy_url must use http, https or socks5 with local DNS; remote proxy DNS is unsupported")
	}
	if proxy.Path != "" && proxy.Path != "/" || proxy.Opaque != "" {
		return nil, errors.New("proxy_url must not contain a path")
	}
	validated := *proxy
	if validated.Scheme == "socks5" {
		validated.Scheme = "https"
	}
	if _, err := p.ValidateEndpoint(validated.String()); err != nil {
		return nil, fmt.Errorf("invalid proxy endpoint: %w", err)
	}
	return proxy, nil
}

func connectionRoots(certificates string) (*x509.CertPool, error) {
	if len(certificates) > 256<<10 {
		return nil, errors.New("trust_roots_pem must not exceed 256 KiB")
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, errors.New("system certificate roots are unavailable")
	}
	remaining := []byte(certificates)
	count := 0
	for len(bytes.TrimSpace(remaining)) > 0 {
		remaining = bytes.TrimSpace(remaining)
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("trust_roots_pem must contain only PEM certificates")
		}
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) > 0 {
			return nil, errors.New("trust_roots_pem must contain only PEM certificates")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("trust_roots_pem contains an invalid certificate")
		}
		pool.AddCert(certificate)
		count++
		remaining = rest
	}
	if count == 0 {
		return nil, errors.New("trust_roots_pem must contain a certificate")
	}
	return pool, nil
}

func milliseconds(value *int64, fallback time.Duration) time.Duration {
	if value == nil {
		return fallback
	}
	return time.Duration(*value) * time.Millisecond
}
func connectionCount(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

// ConnectionClient builds one reusable client. Use ConnectionClientCache at
// request-serving call sites rather than constructing a transport per request.
// Trust roots augment system roots on both TLS legs; a client certificate is
// offered only to the provider, never to an HTTPS proxy.
func (p Policy) ConnectionClient(options *ConnectionOptions, secret []byte, defaultResponseHeaderTimeout time.Duration) (*http.Client, error) {
	if err := p.ValidateConnection(options); err != nil {
		return nil, err
	}
	if defaultResponseHeaderTimeout < 0 {
		return nil, errors.New("default response header timeout must not be negative")
	}
	if options == nil {
		options = &ConnectionOptions{}
	}
	if options.CredentialID != "" && len(secret) == 0 {
		return nil, errors.New("network credential could not be resolved")
	}
	credentials, certificate, err := parseConnectionSecret(secret)
	if err != nil {
		return nil, err
	}
	credentials.ClientCertificatePEM = ""
	credentials.ClientKeyPEM = ""
	if credentials.ProxyUsername != "" && options.ProxyURL == "" {
		return nil, errors.New("proxy credentials require a configured proxy")
	}
	var roots *x509.CertPool
	if options.TrustRootsPEM != "" {
		roots, err = connectionRoots(options.TrustRootsPEM)
		if err != nil {
			return nil, err
		}
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	if certificate != nil {
		tlsConfig.Certificates = []tls.Certificate{*certificate}
	}
	connectTimeout := milliseconds(options.ConnectTimeoutMS, 5*time.Second)
	tlsTimeout := milliseconds(options.TLSHandshakeTimeoutMS, 5*time.Second)
	policy := Policy{AllowedNetworks: append([]netip.Prefix(nil), p.AllowedNetworks...), PlainHTTPHosts: append([]string(nil), p.PlainHTTPHosts...)}
	dial := connectionDialer{policy: policy, connectTimeout: connectTimeout, tlsTimeout: tlsTimeout, roots: roots, credentials: credentials, targetTLS: tlsConfig}
	if options.ProxyURL != "" {
		dial.proxy, err = policy.connectionProxy(options.ProxyURL)
		if err != nil {
			return nil, err
		}
		if dial.proxy.Scheme == "socks5" && credentials.ProxyUsername != "" && (len(credentials.ProxyUsername) > 255 || len(credentials.ProxyPassword) < 1 || len(credentials.ProxyPassword) > 255) {
			return nil, errors.New("SOCKS5 username and password must each contain 1 to 255 bytes")
		}
	}
	transport := &http.Transport{
		Proxy: nil, DialContext: dial.DialContext, DialTLSContext: dial.DialTLSContext, TLSClientConfig: tlsConfig, TLSHandshakeTimeout: tlsTimeout,
		ResponseHeaderTimeout: milliseconds(options.ResponseHeaderTimeoutMS, defaultResponseHeaderTimeout), ExpectContinueTimeout: time.Second, MaxResponseHeaderBytes: 32 << 10,
		IdleConnTimeout: milliseconds(options.IdleConnTimeoutMS, 90*time.Second), MaxIdleConns: connectionCount(options.MaxIdleConns, 128), MaxIdleConnsPerHost: connectionCount(options.MaxIdleConnsPerHost, 16), MaxConnsPerHost: connectionCount(options.MaxConnsPerHost, 64), ForceAttemptHTTP2: true,
	}
	return &http.Client{Transport: &connectionTransport{policy: policy, transport: transport}, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRedirect }}, nil
}

type connectionTransport struct {
	policy    Policy
	transport *http.Transport
	retired   atomic.Bool
}

type connectionRequestContextKey struct{}

func (t *connectionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL == nil || request.URL.Opaque != "" || request.URL.Fragment != "" {
		return nil, errors.New("provider request URL is invalid")
	}
	endpoint := *request.URL
	endpoint.RawQuery = ""
	endpoint.ForceQuery = false
	if _, err := t.policy.ValidateEndpoint(endpoint.String()); err != nil {
		return nil, err
	}
	// net/http deliberately detaches dial cancellation while retaining context
	// values. Keep the original request context available to our connection
	// establishment code so canceled provider work does not leave proxy/TLS
	// negotiation running in the background.
	request = request.WithContext(context.WithValue(request.Context(), connectionRequestContextKey{}, request.Context()))
	if t.retired.Load() {
		request.Close = true
	}
	response, err := t.transport.RoundTrip(request)
	if err != nil {
		if t.retired.Load() {
			t.transport.CloseIdleConnections()
		}
		return nil, err
	}
	if duplex, ok := response.Body.(io.ReadWriteCloser); ok {
		response.Body = &retiringDuplexBody{ReadWriteCloser: duplex, owner: t}
	} else {
		response.Body = &retiringConnectionBody{ReadCloser: response.Body, owner: t}
	}
	return response, nil
}
func (t *connectionTransport) CloseIdleConnections() { t.transport.CloseIdleConnections() }
func (t *connectionTransport) retire()               { t.retired.Store(true); t.transport.CloseIdleConnections() }

type retiringConnectionBody struct {
	io.ReadCloser
	owner *connectionTransport
}

func (b *retiringConnectionBody) Close() error {
	err := b.ReadCloser.Close()
	if b.owner.retired.Load() {
		b.owner.transport.CloseIdleConnections()
	}
	return err
}

type retiringDuplexBody struct {
	io.ReadWriteCloser
	owner *connectionTransport
}

func (b *retiringDuplexBody) Close() error {
	err := b.ReadWriteCloser.Close()
	if b.owner.retired.Load() {
		b.owner.transport.CloseIdleConnections()
	}
	return err
}
