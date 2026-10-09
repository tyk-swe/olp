package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestCodeNetworkChangeDuringAdmissionRefusesBeforeDispatch(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowedCIDRs = []string{"192.0.2.0/24"}
	h.gateway.CodeLedger = endUserCodeLedger{ledger, authority}
	response := codeDo(t, server, []byte(`{"model":"native-model","input":"hi","stream":true}`), http.Header{"Content-Type": {"application/json"}})
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 403 || !bytes.Contains(body, []byte("code_ip_not_allowed")) {
		t.Fatalf("fresh network policy: %d %s", response.StatusCode, body)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.marks) != 0 || len(ledger.aborts) != 1 || h.mock.count("a") != 0 {
		t.Fatal("network refusal dispatched or retained a prepared attempt")
	}
}

func TestKeyNetworkRestrictionsUseTrustedClientAddress(t *testing.T) {
	for _, test := range []struct {
		name, remote, forwarded, cidr string
		trusted, allowed              bool
	}{
		{"direct IPv4", "192.0.2.1:123", "", "192.0.2.0/24", false, true},
		{"direct IPv6", "[2001:db8::1]:123", "", "2001:db8::/32", false, true},
		{"mapped IPv4", "[::ffff:192.0.2.1]:123", "", "192.0.2.0/24", false, true},
		{"untrusted spoof", "198.51.100.1:123", "192.0.2.1", "192.0.2.0/24", false, false},
		{"trusted proxy", "10.0.0.1:123", "192.0.2.1", "192.0.2.0/24", true, true},
		{"untrusted middle hop", "10.0.0.1:123", "192.0.2.1, 198.51.100.1, 10.0.0.2", "192.0.2.0/24", true, false},
		{"malformed forwarding", "10.0.0.1:123", "invalid", "192.0.2.0/24", true, false},
		{"invalid peer", "invalid", "192.0.2.1", "0.0.0.0/0", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			key := h.rt.keys[fullKey]
			key.Policy.AllowedCIDRs = []string{test.cidr}
			h.rt.keys[fullKey] = key
			if test.trusted {
				h.gateway.cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
			}
			for _, scope := range []string{"inference", "models_read"} {
				r := httptest.NewRequest("GET", "/v1/models", nil)
				r.RemoteAddr = test.remote
				r.Header.Set("X-Forwarded-For", test.forwarded)
				_, failure := h.gateway.authenticateRequest(r, fullKey, scope)
				if test.allowed && failure != nil {
					t.Fatal(failure)
				}
				if !test.allowed && (failure == nil || failure.Status != 403 || failure.Code != "ip_not_allowed") {
					t.Fatalf("scope %s: %v", scope, failure)
				}
			}
		})
	}
}

func TestKeyNetworkRefusalPrecedesNativeIdentityBodyReading(t *testing.T) {
	h := endUserHarness(t, "native")
	key := h.rt.keys[fullKey]
	key.Policy.AllowedCIDRs = []string{"192.0.2.0/24"}
	h.rt.keys[fullKey] = key
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("not JSON"))
	r.RemoteAddr = "198.51.100.1:123"
	r.Header.Set("Content-Type", "application/json")
	if _, failure := h.gateway.authenticateRequest(r, fullKey, "inference"); failure == nil || failure.Code != "ip_not_allowed" {
		t.Fatalf("network refusal did not precede body parsing: %v", failure)
	}
}
