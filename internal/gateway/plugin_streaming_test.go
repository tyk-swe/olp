package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// serveStreamingProfile moves the harness's first provider onto a Responses
// plugin profile whose upstream serves only streaming requests.
func (h *harness) serveStreamingProfile() {
	h.t.Helper()
	h.servePlugin(abi.Profile{ID: "acme-responses", Label: "Acme Responses", Dialect: "openai-responses", Hosting: abi.Hosting{
		Address: h.upstream.URL + "/a/v1", Headers: map[string]string{"Authorization": "Token {credential}"}, ForceStreaming: true,
	}})
}

// streamedItem is the output item the streaming-only upstream completes.
func streamedItem(text string) string {
	return `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + fmt.Sprintf("%q", text) + `,"annotations":[]}]}`
}

// streamedEvents is a Responses stream that delivers its output item only in
// response.output_item.done and leaves the terminal response's output empty.
func streamedEvents(text string) []string {
	return []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"` + modelA + `","output":[]}}`,
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}`,
		`{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"delta":` + fmt.Sprintf("%q", text) + `}`,
		`{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":` + streamedItem(text) + `}`,
		`{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","object":"response","status":"completed","model":"` + modelA + `","output":[],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}}`,
	}
}

// streamingOnly is an upstream that serves only streaming Responses
// requests: it refuses any other request, and streams events for the rest.
// When lost is set, the connection drops after the events.
func streamingOnly(events []string, lost bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Stream || r.Header.Get("Accept") != "text/event-stream" || r.URL.Path != "/a/v1/responses" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Stream must be set to true","type":"invalid_request_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		if lost {
			_ = http.NewResponseController(w).Flush()
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}
}

// responses sends a Responses request for the harness route with the given
// extra members, and returns the response with its decoded body.
func (h *harness) responses(fields string) (*http.Response, map[string]any) {
	h.t.Helper()
	resp := h.do(h.t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hi"`+fields+`}`), nil)
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		h.t.Fatalf("invalid JSON response: %v", err)
	}
	return resp, body
}

// A non-streaming caller of a profile that forces streaming gets the
// dialect's non-streaming result, aggregated from the upstream's stream, and
// the same usage a streaming caller is accounted.
func TestForcedStreamingAggregatesTheStreamForNonStreamingCallers(t *testing.T) {
	h := newHarness(t, Config{})
	h.serveStreamingProfile()
	h.mock.set("a", streamingOnly(streamedEvents(answerText), false))

	resp, response := h.responses(`,"stream":false`)
	if resp.StatusCode != http.StatusOK || response["object"] != "response" || response["model"] != routeSlug || response["status"] != "completed" ||
		!strings.Contains(fmt.Sprint(response["output"]), answerText) || response["usage"].(map[string]any)["total_tokens"] != float64(10) {
		t.Fatalf("non-streaming Responses %d %v", resp.StatusCode, response)
	}
	unary := h.sink.last(t)
	if unary.Mode != "unary" || unary.Usage == nil || unary.Usage.TotalTokens != 10 || len(unary.Attempts) != 1 || unary.Attempts[0].Class != classSuccess || unary.Attempts[0].Status != http.StatusOK {
		t.Fatalf("accounted %+v", unary)
	}

	stream := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hi","stream":true}`), nil)
	defer stream.Body.Close()
	events, _ := io.ReadAll(stream.Body)
	if stream.StatusCode != http.StatusOK || strings.Count(string(events), "event: response.") != 5 || !strings.Contains(string(events), `"delta":"`+answerText+`"`) {
		t.Fatalf("streaming Responses %d %s", stream.StatusCode, events)
	}
	streamed := h.sink.last(t)
	if streamed.Mode != "streaming" || !reflect.DeepEqual(streamed.Usage, unary.Usage) {
		t.Fatalf("a streaming caller was accounted %+v, a non-streaming one %+v", streamed.Usage, unary.Usage)
	}
	u, s := unary.Attempts[0], streamed.Attempts[0]
	if u.UsageObserved != s.UsageObserved || u.UsageComplete != s.UsageComplete || u.BillingUncertain != s.BillingUncertain || !reflect.DeepEqual(u.Usage, s.Usage) {
		t.Fatalf("attempt evidence differs: %+v and %+v", u, s)
	}
	if h.mock.count("a") != 2 || h.mock.count("b") != 0 {
		t.Fatalf("dispatched a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
}

// Aggregation keeps at most the response size limit, and a stream beyond it
// is a terminal failure that tells the caller to stream instead.
func TestForcedStreamAggregationIsBounded(t *testing.T) {
	h := newHarness(t, Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 512, MaxEventBytes: 4096})
	h.serveStreamingProfile()
	h.mock.set("a", streamingOnly(streamedEvents(strings.Repeat("y", 600)), false))

	resp, body := h.responses("")
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "upstream_response_too_large" || !strings.Contains(body["error"].(map[string]any)["message"].(string), "request a streaming response") {
		t.Fatalf("aggregated beyond the bound: %d %v", resp.StatusCode, body)
	}
	if env := h.sink.last(t); len(env.Attempts) != 1 || env.Attempts[0].Class != classProtocol || h.mock.count("b") != 0 {
		t.Fatalf("an oversized aggregate failed over: %+v", env.Attempts)
	}
	// A streaming caller receives the same stream event by event.
	stream := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hi","stream":true}`), nil)
	defer stream.Body.Close()
	if events, _ := io.ReadAll(stream.Body); stream.StatusCode != http.StatusOK || !strings.Contains(string(events), "event: response.completed") {
		t.Fatalf("streaming beyond the aggregation bound: %d %s", stream.StatusCode, events)
	}
}

// Once the upstream has accepted the forced stream, a failure in the middle of
// it leaves the upstream's work unaccounted, as for any accepted request, and
// yields no partial result.
func TestForcedStreamFailuresAfterAcceptance(t *testing.T) {
	events := streamedEvents(answerText)
	failed := `{"type":"response.failed","sequence_number":4,"response":{"id":"resp_1","object":"response","status":"failed","model":"` + modelA + `","output":[],"error":{"code":"server_error","message":"The model failed."},"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`
	for name, tc := range map[string]struct {
		events   []string
		lost     bool
		class    string
		observed bool
		failover bool
	}{
		"the stream ends early":      {events[:4], false, classProtocol, false, false},
		"the connection is lost":     {events[:4], true, classConnect, false, true},
		"the upstream fails in band": {append(events[:4:4], failed), false, classUpstreamServer, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.serveStreamingProfile()
			h.mock.set("a", streamingOnly(tc.events, tc.lost))
			h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"resp_2","object":"response","status":"completed","model":"`+modelB+`","output":[`+streamedItem(answerText)+`],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
			})
			resp, body := h.responses("")
			if tc.failover != (resp.StatusCode == http.StatusOK) {
				t.Fatalf("status %d %v", resp.StatusCode, body)
			}
			env := h.sink.last(t)
			if want := map[bool]int{false: 1, true: 2}[tc.failover]; len(env.Attempts) != want {
				t.Fatalf("attempts %+v", env.Attempts)
			}
			fact := env.Attempts[0]
			if fact.Status != http.StatusOK || fact.Class != tc.class || fact.Committed || fact.UsageObserved != tc.observed || fact.BillingUncertain == tc.observed {
				t.Fatalf("the accepted attempt was recorded as %+v", fact)
			}
		})
	}
}
