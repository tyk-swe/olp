//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// hostedSearchFixture is a direct-OpenAI Responses upstream that answers the
// native web_search lifecycle. refusal serves a definitive in-band HTTP error
// (a terminal provider answer, never accepted work); failure replays the
// ambiguous transport cuts that must never be retried; malformed emits an
// undeclared hosted item.
type hostedSearchFixture struct {
	*strictProviderFixture
	mu        sync.Mutex
	calls     [][]byte
	failure   atomic.Int32
	refusal   atomic.Int32
	malformed atomic.Bool
}

const hostedSearchResult = `{"id":"resp_ws_1","object":"response","created_at":1,"model":"` + vendorModel + `","status":"completed","error":null,"incomplete_details":null,"output":[{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","queries":["olp strict contract"],"sources":[{"type":"url","url":"https://example.com/a"},{"type":"url","url":"https://example.org/b"}]}},{"id":"msg_ws_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"grounded answer","annotations":[{"type":"url_citation","url":"https://example.com/a","title":"Example A","start_index":0,"end_index":8}]}]}],"usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}`

const hostedSearchBadResult = `{"id":"resp_ws_bad","object":"response","created_at":1,"model":"` + vendorModel + `","status":"completed","error":null,"incomplete_details":null,"output":[{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","queries":["olp strict contract"]}},{"id":"fs_1","type":"file_search_call","status":"completed","queries":["secret"],"results":[]}],"usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}`

const hostedSearchCompleted = `{"type":"response.completed","sequence_number":5,"response":{"id":"resp_ws_1","object":"response","created_at":1,"model":"` + vendorModel + `","status":"completed","output":[{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","queries":["olp strict contract"],"sources":[{"type":"url","url":"https://example.com/a"}]}},{"id":"msg_ws_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"grounded answer","annotations":[{"type":"url_citation","url":"https://example.com/a","title":"Example A","start_index":0,"end_index":8}]}]}],"usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}}`

const hostedSearchBadCompleted = `{"type":"response.completed","sequence_number":5,"response":{"id":"resp_ws_bad","object":"response","created_at":1,"model":"` + vendorModel + `","status":"completed","output":[{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","queries":["olp strict contract"]}},{"id":"fs_1","type":"file_search_call","status":"completed","queries":["secret"],"results":[]}],"usage":{"input_tokens":12,"output_tokens":8,"total_tokens":20}}}`

const hostedSearchStream = "event: response.created\n" +
	`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_ws_1","object":"response","created_at":1,"model":"` + vendorModel + `","status":"in_progress","output":[]}}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":0,"sequence_number":1,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}` + "\n\n" +
	"event: response.web_search_call.in_progress\n" +
	`data: {"type":"response.web_search_call.in_progress","item_id":"ws_1","output_index":0,"sequence_number":2}` + "\n\n" +
	"event: response.web_search_call.searching\n" +
	`data: {"type":"response.web_search_call.searching","item_id":"ws_1","output_index":0,"sequence_number":3}` + "\n\n" +
	"event: response.web_search_call.completed\n" +
	`data: {"type":"response.web_search_call.completed","item_id":"ws_1","output_index":0,"sequence_number":4}` + "\n\n" +
	"event: response.completed\n" +
	"data: " + hostedSearchCompleted + "\n\n"

const hostedSearchBadStream = "event: response.web_search_call.in_progress\n" +
	`data: {"type":"response.web_search_call.in_progress","item_id":"ws_1","output_index":0,"sequence_number":1}` + "\n\n" +
	"event: response.completed\n" +
	"data: " + hostedSearchBadCompleted + "\n\n"

func newHostedSearchFixture(t *testing.T, profile string) *hostedSearchFixture {
	t.Helper()
	u := &hostedSearchFixture{strictProviderFixture: &strictProviderFixture{profile: profile}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		u.mu.Lock()
		u.calls = append(u.calls, raw)
		u.mu.Unlock()
		switch failure := u.failure.Load(); failure {
		case 1:
			// The request body was fully received: the outcome is ambiguous.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		case 2:
			// A committed partial response then a cut: doubly ambiguous.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":`)
			_ = http.NewResponseController(w).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		if status := u.refusal.Load(); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(status))
			_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"fixture definitive rejection"}}`)
			return
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		stream := string(body["stream"]) == "true"
		if !bytes.Contains(raw, []byte(`"web_search"`)) {
			writeResponsesFixture(w, vendorModel, vendorAnswer, stream)
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			if u.malformed.Load() {
				_, _ = io.WriteString(w, hostedSearchBadStream)
				return
			}
			_, _ = io.WriteString(w, hostedSearchStream)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if u.malformed.Load() {
			_, _ = io.WriteString(w, hostedSearchBadResult)
			return
		}
		_, _ = io.WriteString(w, hostedSearchResult)
	}))
	t.Cleanup(u.Server.Close)
	return u
}

func (u *hostedSearchFixture) captured() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.calls...)
}

// hostedKey issues an inference key authorized for provider-hosted tools.
func hostedKey(t *testing.T, h *accessHarness, owner *browser, slug string) string {
	t.Helper()
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "hosted tools", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_hosted_tools": true}, idem(uuid.NewString()), 201)
	return key["secret"].(string)
}

// Native web_search rides the OpenAI Responses lifecycle: it needs the key's
// hosted-tool permission and the direct-OpenAI profile's qualified lifecycle,
// and its output stays under the strict result contract.
func TestStrictWebSearchRequiresPermissionProfileAndContract(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newHostedSearchFixture(t, "openai-responses")
	slug, plain := publishStrictProvider(t, h, owner, f.strictProviderFixture, nil, nil, "strict")
	compatible := newHostedSearchFixture(t, "compatible-responses")
	compatibleSlug, _ := publishStrictProvider(t, h, owner, compatible.strictProviderFixture, nil, nil, "strict")
	hosted := hostedKey(t, h, owner, slug)
	hostedCompatible := hostedKey(t, h, owner, compatibleSlug)
	h.refresh()

	search := func(key, route string) (int, []byte) {
		t.Helper()
		body := `{"model":"` + route + `","input":"latest contract","store":false,"tools":[{"type":"web_search","search_context_size":"low"}]}`
		status, response, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
		return status, response
	}

	before := len(f.captured())
	if status, response := search(plain, slug); status != 400 || !bytes.Contains(response, []byte(`"code":"policy_conflict"`)) {
		t.Fatalf("hosted tool ran without permission: %d %s", status, response)
	}
	if len(f.captured()) != before {
		t.Fatal("an unauthorized hosted tool reached the provider")
	}
	compatibleBefore := len(compatible.captured())
	if status, response := search(hostedCompatible, compatibleSlug); status != 400 || !bytes.Contains(response, []byte(`"code":"state_carrier"`)) {
		t.Fatalf("unqualified profile admitted hosted work: %d %s", status, response)
	}
	if len(compatible.captured()) != compatibleBefore {
		t.Fatal("the compatible Responses profile dispatched hosted work")
	}

	for _, tc := range []struct {
		name, body, code string
	}{
		{"unknown member", `{"model":"` + slug + `","input":"q","store":false,"tools":[{"type":"web_search","mystery":true}]}`, "target_capability"},
		{"other hosted tool", `{"model":"` + slug + `","input":"q","store":false,"tools":[{"type":"file_search","vector_store_ids":["vs_1"]}]}`, "state_carrier"},
		{"hosted include without tool", `{"model":"` + slug + `","input":"q","store":false,"include":["web_search_call.action.sources"]}`, "state_carrier"},
		{"hosted include unknown", `{"model":"` + slug + `","input":"q","store":false,"tools":[{"type":"web_search"}],"include":["web_search_call.future"]}`, "target_capability"},
		{"implicit retention", `{"model":"` + slug + `","input":"q","tools":[{"type":"web_search"}]}`, "policy_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.captured())
			status, response, _ := h.gatewayRaw("POST", "/v1/responses", hosted, strings.NewReader(tc.body), map[string]string{"Content-Type": "application/json"})
			if status != 400 || !bytes.Contains(response, []byte(`"code":"`+tc.code+`"`)) || len(f.captured()) != before {
				t.Fatalf("%s dispatched or misclassified: %d %s", tc.name, status, response)
			}
		})
	}

	body := `{"model":"` + slug + `","input":"latest contract","store":false,"tools":[{"type":"web_search","search_context_size":"low"}],"include":["web_search_call.action.sources"]}`
	before = len(f.captured())
	status, response, _ := h.gatewayRaw("POST", "/v1/responses", hosted, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("admitted web search refused: %d %s", status, response)
	}
	for _, want := range []string{`"type":"web_search_call"`, `"url_citation"`, `"https://example.com/a"`, `"id":"resp_ws_1"`, `"model":"` + slug + `"`} {
		if !bytes.Contains(response, []byte(want)) {
			t.Fatalf("native web search output missing %s: %s", want, response)
		}
	}
	if len(f.captured()) != before+1 {
		t.Fatal("admitted web search was not dispatched exactly once")
	}
	calls := f.captured()
	requireProfileNetworkJSON(t, `{"model":"`+vendorModel+`","input":"latest contract","store":false,"tools":[{"type":"web_search","search_context_size":"low"}],"include":["web_search_call.action.sources"]}`, calls[len(calls)-1])

	// A provider that emits an undeclared hosted item breaks the contract; the
	// gateway refuses the result rather than delivering unbounded effects.
	f.malformed.Store(true)
	before = len(f.captured())
	status, response, _ = h.gatewayRaw("POST", "/v1/responses", hosted, strings.NewReader(`{"model":"`+slug+`","input":"latest contract","store":false,"tools":[{"type":"web_search"}]}`), map[string]string{"Content-Type": "application/json"})
	if status != 502 || !bytes.Contains(response, []byte(`"code":"fidelity_protocol_violation"`)) || bytes.Contains(response, []byte("file_search_call")) || len(f.captured()) != before+1 {
		t.Fatalf("undeclared hosted output delivered: %d %s", status, response)
	}
	f.malformed.Store(false)
}

// Streaming keeps the provider's own web_search lifecycle events; a terminal
// event carrying undeclared hosted work is withheld before it reaches the
// client. A strict request's first dispatch pins its serving identity: a
// sibling provider may never substitute for the pinned serving, so even a
// definitive pre-commit refusal is reported as itself instead of replaying,
// and an ambiguous cut is terminal on the same terms.
func TestStrictWebSearchStreamingAndRetryContract(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	first := newHostedSearchFixture(t, "openai-responses")
	second := newHostedSearchFixture(t, "openai-responses")
	slug, _ := publishStrictProvider(t, h, owner, first.strictProviderFixture, nil, nil, "strict")
	publishStrictProvider(t, h, owner, second.strictProviderFixture, nil, nil, "strict")
	input := fidelityDraft(slug, first.providerID)
	input["fidelity"] = map[string]any{"mode": "strict"}
	input["max_attempts"] = 2
	input["targets"] = []any{
		map[string]any{"provider_id": first.providerID, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000},
		map[string]any{"provider_id": second.providerID, "provider_model": vendorModel, "priority": 1, "weight": 1, "timeout_ms": 5000},
	}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", input, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := hostedKey(t, h, owner, slug)
	h.refresh()

	request := `{"model":"` + slug + `","input":"latest contract","store":false,"stream":true,"tools":[{"type":"web_search"}]}`

	// The provider's own web_search lifecycle arrives untouched: in-progress,
	// searching and completed frames, the hosted item and its citation.
	status, response, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
	if status != 200 {
		t.Fatalf("admitted hosted stream refused: %d %s", status, response)
	}
	for _, want := range []string{"event: response.web_search_call.in_progress", "event: response.web_search_call.searching", "event: response.web_search_call.completed", `"type":"web_search_call"`, `"url_citation"`, `"id":"resp_ws_1"`, "event: response.completed"} {
		if !bytes.Contains(response, []byte(want)) {
			t.Fatalf("hosted stream missing %s:\n%s", want, response)
		}
	}

	// A malformed terminal observation is never forwarded: the lifecycle
	// frames were admitted but the violating completion is withheld.
	first.malformed.Store(true)
	status, response, _ = h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
	if bytes.Contains(response, []byte("file_search_call")) || bytes.Contains(response, []byte("event: response.completed")) {
		t.Fatalf("undeclared hosted event reached the client: %d %s", status, response)
	}
	first.malformed.Store(false)

	// Once the provider may have accepted the web-search work, the attempt is
	// terminal: neither a cut connection nor a committed partial reply may
	// replay on the sibling target.
	unary := `{"model":"` + slug + `","input":"latest contract","store":false,"tools":[{"type":"web_search"}]}`
	for _, failure := range []int32{1, 2} {
		first.failure.Store(failure)
		beforeFirst, beforeSecond := len(first.captured()), len(second.captured())
		status, response, headers := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(unary), map[string]string{"Content-Type": "application/json"})
		if status != 502 || headers.Get("X-Should-Retry") != "false" || !bytes.Contains(response, []byte(`"code":"ambiguous_upstream_result"`)) || len(first.captured()) != beforeFirst+1 || len(second.captured()) != beforeSecond {
			t.Fatalf("ambiguous hosted work replayed: %d %s first=%d second=%d", status, response, len(first.captured()), len(second.captured()))
		}
	}
	first.failure.Store(0)

	// A definitive refusal the provider declared before accepting the work —
	// a rate limit the retry taxonomy would otherwise fail over — still
	// cannot cross strict serving identity: the pinned serving owns the
	// request, the sibling provider stays untouched, and the refusal reaches
	// the client as itself. The cooled credential keeps later requests off
	// this slot, so this leg stays last.
	first.refusal.Store(429)
	beforeFirst, beforeSecond := len(first.captured()), len(second.captured())
	status, response, _ = h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(request), map[string]string{"Content-Type": "application/json"})
	if status != 429 || !bytes.Contains(response, []byte(`"code":"upstream_rate_limit"`)) || len(first.captured()) != beforeFirst+1 || len(second.captured()) != beforeSecond {
		t.Fatalf("strict route substituted a sibling provider after refusal: %d %s first=%d second=%d", status, response, len(first.captured()), len(second.captured()))
	}
}
