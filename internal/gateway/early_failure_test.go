package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestEveryEarlyInferenceFailureEmitsOneTerminalEnvelope(t *testing.T) {
	for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses} {
		for _, tc := range []struct {
			name, code string
			status     int
			authed     bool
			change     func(*harness, *http.Request)
		}{
			{"missing key", "invalid_api_key", 401, false, func(_ *harness, r *http.Request) { r.Header.Del("Authorization") }},
			{"invalid key", "invalid_api_key", 401, false, func(_ *harness, r *http.Request) { r.Header.Set("Authorization", "Bearer invalid-secret") }},
			{"scope missing", "permission_denied", 403, false, func(_ *harness, r *http.Request) { r.Header.Set("Authorization", "Bearer "+readKey) }},
			{"revoked key", "invalid_api_key", 401, false, func(h *harness, _ *http.Request) {
				a := h.rt.keys[fullKey]
				now := time.Now()
				a.RevokedAt = &now
				h.rt.keys[fullKey] = a
			}},
			{"expired key", "invalid_api_key", 401, false, func(h *harness, _ *http.Request) {
				a := h.rt.keys[fullKey]
				expired := time.Now().Add(-time.Hour)
				a.ExpiresAt = &expired
				h.rt.keys[fullKey] = a
			}},
			{"stale authority", "authority_unavailable", 503, false, func(h *harness, _ *http.Request) { h.rt.stale = true }},
			{"admission full", "request_admission_overloaded", 503, false, func(h *harness, _ *http.Request) {
				for h.gateway.admit() {
				}
			}},
			{"media type", "unsupported_media_type", 415, true, func(_ *harness, r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
			{"encoding", "unsupported_content_encoding", 415, true, func(_ *harness, r *http.Request) { r.Header.Set("Content-Encoding", "br") }},
			{"invalid gzip", "invalid_request", 400, true, func(_ *harness, r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }},
			{"read error", "invalid_request", 400, true, func(_ *harness, r *http.Request) { r.Body = io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF)) }},
			{"read timeout", "request_timeout", 408, true, func(_ *harness, r *http.Request) { r.Body = io.NopCloser(iotest.ErrReader(os.ErrDeadlineExceeded)) }},
			{"body limit", "request_too_large", 413, true, func(h *harness, r *http.Request) { h.gateway.cfg.MaxBodyBytes = 4 }},
			{"malformed JSON", "invalid_json", 400, true, func(_ *harness, r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`{"model":`)) }},
			{"missing fields", "missing_required_parameter", 400, true, func(_ *harness, r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`{}`)) }},
			{"route forbidden", "route_forbidden", 403, true, func(h *harness, _ *http.Request) {
				a := h.rt.keys[fullKey]
				a.Policy.AllowedRoutes = []string{"other"}
				h.rt.keys[fullKey] = a
			}},
			{"routing header", "invalid_request", 400, true, func(_ *harness, r *http.Request) { r.Header.Set(routingHeader, `{"max_attempts":0}`) }},
		} {
			t.Run(string(family)+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t, Config{})
				body := `{"model":"team-chat","messages":[{"role":"user","content":"private prompt"}]}`
				if family == openai.FamilyResponses {
					body = `{"model":"team-chat","input":"private prompt"}`
				}
				r := httptest.NewRequest(http.MethodPost, "/v1"+endpointPath(family), strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+fullKey)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-Request-Id", "early-failure")
				tc.change(h, r)
				admitted := len(h.gateway.admission)
				w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), beforeDelivery: func() {}}
				h.gateway.inference(family)(w, r)
				if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
					t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
				}
				if len(h.sink.envs) != 1 {
					t.Fatalf("terminal envelopes=%d, want 1", len(h.sink.envs))
				}
				env := h.sink.envs[0]
				if env.RequestID != "early-failure" || env.Family != string(family) || env.Actor != "api_key" || env.ReleaseSequence != 7 || env.Outcome != "failure" || env.Status != tc.status || env.StartedAt.IsZero() || env.Duration < 0 {
					t.Fatalf("incorrect terminal metadata: %+v", env)
				}
				keyID := ""
				if tc.authed {
					keyID = h.keyID
				}
				if env.KeyID != keyID || len(env.Attempts) != 0 || env.Usage != nil || env.Committed || h.mock.count("a") != 0 || h.mock.count("b") != 0 || len(h.gateway.admission) != admitted {
					t.Fatalf("early failure dispatched work or lost identity/admission: %+v", env)
				}
			})
		}
	}
}

func TestReadDeadlineFailureEmitsTerminalEnvelope(t *testing.T) {
	h := newHarness(t, Config{})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// ResponseRecorder deliberately has no SetReadDeadline implementation.
	w := httptest.NewRecorder()
	h.gateway.inference(openai.FamilyChat)(w, r)
	if w.Code != http.StatusInternalServerError || len(h.sink.envs) != 1 || h.sink.envs[0].Status != w.Code || h.sink.envs[0].Outcome != "failure" {
		t.Fatalf("deadline setup failure: response=%d envelopes=%+v", w.Code, h.sink.envs)
	}
}

func TestResponsesReferencesNeverReachUpstream(t *testing.T) {
	for _, input := range []string{
		`[{"type":"item_reference","id":"msg_private"}]`,
		`[{"id":"msg_private"}]`,
		`[{"role":"user","content":[{"type":"input_file","file_id":"file_private"}]}]`,
	} {
		for _, streaming := range []string{"false", "true"} {
			h := newHarness(t, Config{})
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"team-chat","input":`+input+`,"stream":`+streaming+`}`))
			r.Header.Set("Authorization", "Bearer "+fullKey)
			r.Header.Set("Content-Type", "application/json")
			w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), beforeDelivery: func() {}}
			h.gateway.inference(openai.FamilyResponses)(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"unsupported_stateful_reference"`) || h.mock.count("a") != 0 || h.mock.count("b") != 0 {
				t.Fatalf("stateful reference reached upstream: %d %s", w.Code, w.Body.String())
			}
			if len(h.sink.envs) != 1 || len(h.sink.envs[0].Attempts) != 0 || h.sink.envs[0].Outcome != "failure" {
				t.Fatalf("reference rejection telemetry: %+v", h.sink.envs)
			}
		}
	}
}
