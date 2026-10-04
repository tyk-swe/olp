package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/runtime"
)

// anthropicStrictHarness serves provider "a" as a native Anthropic upstream
// behind a strict route, as Claude Code reaches it.
func anthropicStrictHarness(t *testing.T) *harness {
	t.Helper()
	return strictHarness(t, func(snapshot *runtime.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name != "a" {
				continue
			}
			provider.Kind, provider.VendorID, provider.ProfileID = "anthropic", "anthropic", "anthropic-messages"
			provider.Capabilities = []runtime.Capability{
				{Model: modelA, Operation: "generation", Surface: "anthropic", Mode: "unary"},
				{Model: modelA, Operation: "generation", Surface: "anthropic", Mode: "streaming"},
				{Model: modelA, Operation: "token_count", Surface: "anthropic", Mode: "unary"},
			}
			snapshot.Providers[id] = provider
		}
		route := snapshot.Routes[routeSlug]
		route.Operations = append(route.Operations, "token_count")
		snapshot.Routes[routeSlug] = route
	})
}

// upstreamRequest is what an upstream received. Beta is the Anthropic-Beta
// header, and BetaLines every line of it, which tells an absent header from an
// empty one.
type upstreamRequest struct {
	Query     string
	Beta      string
	BetaLines []string
	Body      json.RawMessage
}

type upstreamLog struct {
	mu       sync.Mutex
	requests []upstreamRequest
}

func (l *upstreamLog) last(t *testing.T) upstreamRequest {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.requests) == 0 {
		t.Fatal("the upstream received nothing")
	}
	return l.requests[len(l.requests)-1]
}

// recordUpstream makes provider "a" answer with reply and records what each
// request carried before reply sees it, body included.
func recordUpstream(h *harness, reply http.HandlerFunc) *upstreamLog {
	log := &upstreamLog{}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		log.mu.Lock()
		log.requests = append(log.requests, upstreamRequest{Query: r.URL.RawQuery, Beta: r.Header.Get("Anthropic-Beta"), BetaLines: r.Header.Values("Anthropic-Beta"), Body: body})
		log.mu.Unlock()
		reply(w, r)
	})
	return log
}

// recordAnthropic answers the upstream's Messages and count_tokens calls and
// records what each carried.
func recordAnthropic(h *harness) *upstreamLog {
	return recordUpstream(h, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/count_tokens"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"input_tokens":7}`)
		case strings.Contains(string(body), `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"model-a\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"+
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"+
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":3}}`)
		}
	})
}

const (
	messagesPath = "/anthropic/v1/messages"
	countPath    = "/anthropic/v1/messages/count_tokens"
	// betas is a header the way Claude Code sends it, and midSystem a request
	// that needs it: the mid-conversation-system beta admits role "system".
	betas     = "claude-code-20250219,mid-conversation-system-2026-04-07,context-management-2025-06-27"
	midSystem = `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"note","cache_control":{"type":"ephemeral"}}]}],"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]}}`
)

// The Anthropic SDKs call the beta namespace with ?beta=true. The query selects
// no API behavior, so a strict route consumes it and the upstream never sees
// it, while the paired beta header and body still arrive unchanged.
func TestStrictAnthropicRoutesAcceptTheBetaNamespaceQuery(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		check            func(*testing.T, upstreamRequest)
	}{
		{"unary", messagesPath, midSystem, nil},
		{"streaming", messagesPath, strings.Replace(midSystem, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1), nil},
		{"count_tokens", countPath, `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"},{"role":"system","content":"note"}]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := anthropicStrictHarness(t)
			log := recordAnthropic(h)
			resp := h.do(t.Context(), http.MethodPost, tc.path+"?beta=true", fullKey, []byte(tc.body), map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": betas})
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			got := log.last(t)
			if got.Query != "" {
				t.Fatalf("the upstream received the query %q", got.Query)
			}
			if got.Beta != betas {
				t.Fatalf("the upstream received Anthropic-Beta %q, want %q", got.Beta, betas)
			}
			var sent struct {
				Messages          []struct{ Role string }
				ContextManagement json.RawMessage `json:"context_management"`
			}
			if err := json.Unmarshal(got.Body, &sent); err != nil {
				t.Fatal(err)
			}
			if len(sent.Messages) != 2 || sent.Messages[1].Role != "system" {
				t.Fatalf("the system message did not arrive in place: %s", got.Body)
			}
			if tc.name != "count_tokens" && !strings.Contains(string(got.Body), `"cache_control":{"type":"ephemeral"}`) {
				t.Fatalf("the cache marker did not arrive: %s", got.Body)
			}
		})
	}
}

// Only the SDK's marker is consumed. Every other caller query setting still
// has to match a setting the profile declares.
func TestStrictAnthropicRoutesRefuseOtherQuerySettings(t *testing.T) {
	for _, query := range []string{"beta=false", "beta=", "beta=true&beta=true", "beta=true&extra=1", "extra=1", "Beta=true"} {
		for _, path := range []string{messagesPath, countPath} {
			t.Run(path+"?"+query, func(t *testing.T) {
				h := anthropicStrictHarness(t)
				recordAnthropic(h)
				body := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
				resp := h.do(t.Context(), http.MethodPost, path+"?"+query, fullKey, []byte(body), map[string]string{"Anthropic-Version": "2023-06-01"})
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "target_capability") || h.mock.count("a") != 0 {
					t.Fatalf("status=%d body=%s dispatches=%d", resp.StatusCode, raw, h.mock.count("a"))
				}
			})
		}
	}
}
