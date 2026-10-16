package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// hashSigner stands in for a plugin's signing hook: it signs a request with a
// hash of the credential and body, or fails when told to.
type hashSigner struct {
	mu    sync.Mutex
	calls int
	fail  error
}

func (s *hashSigner) Sign(_ context.Context, _ string, _ abi.Provider, request abi.SignRequest, _ []string) (abi.SignResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail != nil {
		return abi.SignResult{}, s.fail
	}
	return abi.SignResult{Headers: map[string]string{"X-Acme-Signature": signature(request.Credential, request.Body)}}, nil
}

func (s *hashSigner) signed() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

// recover makes the hook sign again.
func (s *hashSigner) recover() { s.mu.Lock(); defer s.mu.Unlock(); s.fail = nil }

func signature(credential string, body []byte) string {
	sum := sha256.Sum256(append([]byte(credential), body...))
	return hex.EncodeToString(sum[:])
}

// newSigningHarness serves provider a from a plugin profile whose signing
// hook signer runs; provider b stays its failover.
func newSigningHarness(t *testing.T, signer *hashSigner) *harness {
	t.Helper()
	h := newHarness(t, Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096, Signer: signer})
	h.servePlugin(abi.Profile{ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", Signing: true,
		Hosting: abi.Hosting{Address: h.upstream.URL + "/a/v1", Headers: map[string]string{"X-Acme-Client": "olp"}},
	})
	return h
}

// failOverFromSigning sends a request whose signing fails on provider a and
// returns the failed attempt, which sent nothing, after b served it.
func failOverFromSigning(t *testing.T, h *harness) AttemptFact {
	t.Helper()
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK || body["model"] != routeSlug {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[1].Class != classSuccess {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 1 {
		t.Fatalf("calls a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
	failed := env.Attempts[0]
	if failed.Status != 0 || failed.FirstByte != nil || failed.BillingUncertain || !failed.UsageComplete {
		t.Fatalf("the failed attempt was not recorded as not sent: %+v", failed)
	}
	return failed
}
