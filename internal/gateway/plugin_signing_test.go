package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/testutil"
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
	manifest := abi.Manifest{Name: "acme", Version: "1.0.0", Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Hosting: abi.Hosting{Address: h.upstream.URL + "/a/v1", Headers: map[string]string{"X-Acme-Client": "olp"}},
		Signing: true,
	}}}
	digest := strings.Repeat("cd", 32)
	plugin, err := connectors.NewPluginProfile(digest, manifest, "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string][]byte{}
	for id, provider := range h.rt.release.Snapshot.Providers {
		for _, slot := range provider.Slots {
			secrets[*slot.CredentialID], _ = h.rt.release.Credential(*slot.CredentialID)
		}
		if provider.Slots[0].ID == h.slotA {
			provider.Kind, provider.AuthMode, provider.Plugin = connectors.KindPlugin, connectors.AuthStaticCredential, plugin
			provider.ProfileID, provider.ProfileRevision, provider.Endpoint = "acme-chat", digest, plugin.Address(nil)
			h.rt.release.Snapshot.Providers[id] = provider
		}
	}
	if h.rt.release, err = runtime.NewRelease(uuid.NewString(), 7, h.rt.release.Snapshot, secrets); err != nil {
		t.Fatal(err)
	}
	return h
}

// The headers a signing hook returns reach the upstream and are redacted from
// upstream text, like the credential; the credential itself never travels.
func TestSignedHeadersReachTheUpstreamRedacted(t *testing.T) {
	h := newSigningHarness(t, &hashSigner{})
	var received http.Header
	var body []byte
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		body, _ = io.ReadAll(r.Body)
		echo := r.Header.Get("X-Acme-Signature")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "Invalid prompt; signed " + echo, "code": echo, "type": echo}})
	})
	resp, result := h.chat(fullKey, nil)
	if received.Get("X-Acme-Signature") != signature(secretA, body) || received.Get("X-Acme-Client") != "olp" || strings.Contains(fmt.Sprint(received), secretA) {
		t.Fatalf("the upstream received %v", received)
	}
	message := result["error"].(map[string]any)["message"].(string)
	if resp.StatusCode != http.StatusBadRequest || strings.Contains(message, received.Get("X-Acme-Signature")) || !strings.Contains(message, "signed [REDACTED]") {
		t.Fatalf("the signed header was not redacted: %d %s", resp.StatusCode, message)
	}
}

// The hook runs once per upstream request, never per stream event.
func TestAStreamingRequestIsSignedOnce(t *testing.T) {
	signer := &hashSigner{}
	h := newSigningHarness(t, signer)
	var events bytes.Buffer
	for i := range 40 {
		fmt.Fprintf(&events, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%d \"},\"finish_reason\":null}]}\n\n", modelA, i)
	}
	fmt.Fprintf(&events, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", modelA)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Acme-Signature") != signature(secretA, body) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := testutil.Stream(w, r, events.Bytes(), 1, 0); err != nil {
			t.Error(err)
		}
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer resp.Body.Close()
	streamed, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || bytes.Count(streamed, []byte("data: ")) != 42 || !bytes.Contains(streamed, []byte(`"39 "`)) {
		t.Fatalf("status %d stream %s", resp.StatusCode, streamed)
	}
	if env := h.sink.last(t); env.Outcome != "success" || len(env.Attempts) != 1 || signer.signed() != 1 {
		t.Fatalf("%d signing calls for %+v", signer.signed(), env.Attempts)
	}
}

// A signing hook that can't run, such as one past its limits or a plugin that
// crashed, fails the attempt before anything is sent, so the route fails
// over, and blames nothing on the credential: the next request uses its slot.
func TestAnUnavailableSigningHookFailsTheAttemptAsNotSent(t *testing.T) {
	for name, failure := range map[string]error{
		"past its limits": errors.New("plugin_timed_out: The plugin exceeded its 10s time limit."),
		"crashed":         &abi.Error{Code: abi.CodeInternal, Message: "The plugin panicked."},
	} {
		t.Run(name, func(t *testing.T) {
			signer := &hashSigner{fail: failure}
			h := newSigningHarness(t, signer)
			failed := failOverFromSigning(t, h)
			if failed.Class != classConnect {
				t.Fatalf("the failed attempt was classified %s", failed.Class)
			}
			signer.recover()
			if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 {
				t.Fatalf("the slot was cooled: status %d, calls a=%d", resp.StatusCode, h.mock.count("a"))
			}
		})
	}
}

// A failure the signing hook reports, such as a credential it can't sign
// with, is a credential failure: the slot cools down.
func TestAReportedSigningFailureCoolsTheCredential(t *testing.T) {
	signer := &hashSigner{fail: &abi.Error{Code: "credential_expired", Message: "The key expired."}}
	h := newSigningHarness(t, signer)
	if failed := failOverFromSigning(t, h); failed.Class != classCredential {
		t.Fatalf("the failed attempt was classified %s", failed.Class)
	}
	signer.recover()
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK || h.mock.count("a") != 0 || h.mock.count("b") != 2 {
		t.Fatalf("the cooled slot served: status %d, calls a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
	}
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
