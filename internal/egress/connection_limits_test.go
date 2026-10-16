package egress

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConnectionOptionsAndSecretsRejectUnsafeOrUnboundedValues(t *testing.T) {
	for _, options := range []*ConnectionOptions{
		{ConnectTimeoutMS: connectionPointer(int64(0))}, {ConnectTimeoutMS: connectionPointer(int64(120001))},
		{TLSHandshakeTimeoutMS: connectionPointer(int64(-1))}, {TLSHandshakeTimeoutMS: connectionPointer(int64(120001))},
		{ResponseHeaderTimeoutMS: connectionPointer(int64(600001))}, {IdleConnTimeoutMS: connectionPointer(int64(3600001))},
		{MaxIdleConns: connectionPointer(0)}, {MaxIdleConnsPerHost: connectionPointer(4097)}, {MaxConnsPerHost: connectionPointer(-1)},
		{MaxIdleConns: connectionPointer(4), MaxIdleConnsPerHost: connectionPointer(5)},
		{MaxConnsPerHost: connectionPointer(4), MaxIdleConnsPerHost: connectionPointer(5)},
		{TrustRootsPEM: "not a certificate"}, {CredentialID: "secret\nreference"},
	} {
		if err := loopbackConnections().ValidateConnection(options); err == nil {
			t.Fatal("unsafe connection options accepted")
		}
	}
	for _, secret := range []string{"", `null`, `{}`, `{"unknown":"private-value"}`, `{"proxy_username":null}`, `{"proxy_username":"first","proxy_username":"second"}`, `{"proxy_password":"private-value"}`, `{"proxy_username":"user:name","proxy_password":"private-value"}`, `{"client_key_pem":"private-value"}`, `{"client_certificate_pem":"private-value","client_key_pem":"private-value"}`} {
		if err := ValidateConnectionSecret([]byte(secret)); err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("invalid network credential accepted or exposed")
		}
	}
	if _, err := loopbackConnections().ConnectionClient(&ConnectionOptions{CredentialID: "unavailable"}, nil, time.Second); err == nil {
		t.Fatal("missing referenced credential downgraded to anonymous")
	}
	secret := networkSecret(t, map[string]string{"proxy_username": "user", "proxy_password": "password"})
	if _, err := loopbackConnections().ConnectionClient(nil, secret, time.Second); err == nil {
		t.Fatal("proxy credentials accepted without proxy")
	}
	longSecret := networkSecret(t, map[string]string{"proxy_username": "user", "proxy_password": strings.Repeat("p", 512)})
	if err := ValidateConnectionSecret(longSecret); err != nil {
		t.Fatal("valid HTTP proxy credential was constrained by SOCKS framing", err)
	}
	if _, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: "socks5://127.0.0.1:1080", CredentialID: "network-reference"}, longSecret, time.Second); err == nil {
		t.Fatal("oversized SOCKS credential accepted")
	}
}

func TestConnectionHTTPSProxyRequiresItsOwnTrustRoot(t *testing.T) {
	targetCA := newConnectionTestCA(t)
	proxyCA := newConnectionTestCA(t)
	var targetCalls atomic.Int64
	target := tlsConnectionServer(t, targetCA, false, func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); io.WriteString(w, "two-roots") })
	proxy, observations := connectProxy(t, proxyCA, true)
	client, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: proxy.URL, TrustRootsPEM: targetCA.pem}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if _, err = client.Get(target.URL); err == nil {
		t.Fatal("untrusted HTTPS proxy certificate was accepted")
	}
	if targetCalls.Load() != 0 || len(observations) != 0 {
		t.Fatal("untrusted proxy received CONNECT or provider work")
	}
	trusted, err := loopbackConnections().ConnectionClient(&ConnectionOptions{ProxyURL: proxy.URL, TrustRootsPEM: targetCA.pem + proxyCA.pem}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer trusted.CloseIdleConnections()
	if got := connectionResponse(t, trusted, target.URL); got != "two-roots" {
		t.Fatal(got)
	}
}
