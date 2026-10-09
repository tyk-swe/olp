package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

type endUserRuntime struct{ *fakeRuntime }

func (f endUserRuntime) EndUserDigest(project *string, identifier string) string {
	return access.DigestEndUser(secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), "installation"), project, identifier)
}

func endUserHarness(t *testing.T, source string) *harness {
	t.Helper()
	h := strictHarness(t, nil)
	identifyHarness(h, source)
	return h
}

func identifyHarness(h *harness, source string) {
	authority := h.rt.keys[fullKey]
	authority.Policy.EndUserSource = &source
	h.rt.keys[fullKey] = authority
	h.gateway.Runtime = endUserRuntime{h.rt}
}

func TestEndUserHeaderStaysLocalAndAccountingKeepsOnlyDigest(t *testing.T) {
	h := endUserHarness(t, "header")
	const identifier = "private-customer-123"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(endUserHeader) != "" {
			t.Error("end-user header reached upstream")
		}
		completion(modelA, answerText)(w, r)
	})
	response, body := h.chat(fullKey, map[string]string{endUserHeader: identifier})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %v", response.StatusCode, body)
	}
	envelope := h.sink.last(t)
	want := h.gateway.Runtime.(endUserRuntime).EndUserDigest(nil, identifier)
	if envelope.EndUserDigest != want {
		t.Fatalf("digest = %q, want %q", envelope.EndUserDigest, want)
	}
	payload, err := usage.Encode(accountingEvent(envelope))
	if err != nil || bytes.Contains(payload, []byte(identifier)) {
		t.Fatalf("event must contain no identifier: %v", err)
	}
	event, err := usage.Decode(payload)
	if err != nil || event.EndUserDigest != want {
		t.Fatalf("event lost identity: %+v, %v", event, err)
	}
	if h.rt.keys[fullKey].EndUserDigest != "" {
		t.Fatal("request identity mutated cached authority")
	}
}

func TestEndUserRefusalNeverDispatchesOrEchoesIdentifier(t *testing.T) {
	for _, identifier := range []string{"", "private@example.com", strings.Repeat("a", 129)} {
		t.Run(identifier, func(t *testing.T) {
			h := endUserHarness(t, "header")
			response, body := h.chat(fullKey, map[string]string{endUserHeader: identifier})
			if response.StatusCode != http.StatusBadRequest || h.mock.count("a")+h.mock.count("b") != 0 {
				t.Fatalf("invalid identity dispatched: %d %v", response.StatusCode, body)
			}
			encoded, _ := json.Marshal(body)
			if identifier != "" && bytes.Contains(encoded, []byte(identifier)) {
				t.Fatal("refusal echoed the identifier")
			}
		})
	}
}

func TestEndUserHeaderRejectsDuplicatesAndDoesNotGateModelListing(t *testing.T) {
	h := endUserHarness(t, "header")
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Add(endUserHeader, "customer-a")
	req.Header.Add(endUserHeader, "customer-b")
	if _, err := h.gateway.authenticateRequest(req, fullKey, "inference"); err == nil || err.Code != "invalid_end_user" {
		t.Fatalf("duplicate header = %v", err)
	}
	req = httptest.NewRequest("GET", "/v1/models", nil)
	if _, err := h.gateway.authenticateRequest(req, fullKey, "models_read"); err != nil {
		t.Fatalf("model listing required end-user identity: %v", err)
	}
}

func TestNativeEndUserPreservesStrictBodyWithGzip(t *testing.T) {
	h := endUserHarness(t, "native")
	const identifier = "customer-123"
	body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"safety_identifier":"customer-123","user":"legacy-user"}`
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		upstream, _ := io.ReadAll(r.Body)
		var fields map[string]any
		if err := json.Unmarshal(upstream, &fields); err != nil || fields["safety_identifier"] != identifier || fields["user"] != "legacy-user" {
			t.Errorf("strict forwarding changed native identity: %s", upstream)
		}
		completion(modelA, answerText)(w, r)
	})
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	_, _ = w.Write([]byte(body))
	_ = w.Close()
	response := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, compressed.Bytes(), map[string]string{"Content-Encoding": "gzip", endUserHeader: "ignored-source"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("status %d: %s", response.StatusCode, data)
	}
	if got := h.sink.last(t).EndUserDigest; got != h.gateway.Runtime.(endUserRuntime).EndUserDigest(nil, identifier) {
		t.Fatalf("native digest = %q", got)
	}
}

func TestNativeEndUserReauthorizationDoesNotRereadConsumedBody(t *testing.T) {
	h := endUserHarness(t, "native")
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"user":"repeat"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+fullKey)
	first, err := h.gateway.authenticate(r, "inference")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	second, err := h.gateway.authenticate(r, "inference")
	if err != nil || second.EndUserDigest != first.EndUserDigest {
		t.Fatalf("reauthorization lost identity: %v", err)
	}
	h.rt.mu.Lock()
	delete(h.rt.keys, fullKey)
	h.rt.mu.Unlock()
	if _, err = h.gateway.authenticate(r, "inference"); err == nil {
		t.Fatal("cached identity bypassed fresh authorization")
	}
}

func TestNativeEndUserSelectionRejectsAmbiguity(t *testing.T) {
	for _, tc := range []struct{ surface, body, want string }{
		{"openai", `{"user":"legacy"}`, "legacy"},
		{"openai", `{"safety_identifier":"preferred","user":"legacy"}`, "preferred"},
		{"openai", `{"safety_identifier":null,"user":"legacy"}`, ""},
		{"openai", `{"safety_identifier":42,"user":"legacy"}`, ""},
		{"openai", `{"user":"one","user":"two"}`, ""},
		{"anthropic", `{"metadata":{"user_id":"native"},"user":"ignored"}`, "native"},
		{"anthropic", `{"user":"wrong-field"}`, ""},
	} {
		if got := nativeEndUser([]byte(tc.body), tc.surface); got != tc.want {
			t.Errorf("%s %s: got %q, want %q", tc.surface, tc.body, got, tc.want)
		}
	}
}

func assertEndUserEnvelope(t *testing.T, envelope Envelope, digest, identifier string) {
	t.Helper()
	if envelope.EndUserDigest != digest {
		t.Fatalf("terminal identity = %q, want %q", envelope.EndUserDigest, digest)
	}
	// Lists and pre-route refusals have terminal metadata but no billing event.
	if envelope.Route == "" {
		return
	}
	event := accountingEvent(envelope)
	if event == nil {
		t.Fatal("identified operation lost its accounting event")
	}
	if _, err := usage.Validate(event); err != nil {
		t.Fatalf("invalid accounting event: %v", err)
	}
	encoded, err := usage.Encode(event)
	if err != nil || bytes.Contains(encoded, []byte(identifier)) {
		t.Fatalf("identified terminal is invalid or contains the raw identifier: %v", err)
	}
}
