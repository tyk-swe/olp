package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
)

// newCodeVendorHarness serves the "coding" route through a coding-plan
// profile's connection instead of Codex's.
func newCodeVendorHarness(t *testing.T, profile string) (*harness, *codeTestLedger, *httptest.Server) {
	t.Helper()
	h, ledger, server := newCodeForwardHarness(t)
	h.rt.release.Snapshot.CodeConnections = map[string]runtime.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile, Endpoint: h.upstream.URL + "/a"}}
	if err := h.rt.release.Snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	return h, ledger, server
}

func codeVendorDo(t *testing.T, server *httptest.Server, method, path string, body []byte, header http.Header) (*http.Response, []byte) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, server.URL+"/code/coding/"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = header.Clone()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, _ := io.ReadAll(response.Body)
	return response, got
}

func TestCodeMessagesForwardExactBytesWithTheAccountsCredential(t *testing.T) {
	for _, test := range []struct {
		profile, upstream, credential, value string
	}{
		{codeplans.ZAIProfile, "/a/anthropic/v1/messages", "Authorization", "Bearer upstream-only"},
		{codeplans.OpenCodeGoProfile, "/a/messages", "X-Api-Key", "upstream-only"},
	} {
		t.Run(test.profile, func(t *testing.T) {
			h, ledger, server := newCodeVendorHarness(t, test.profile)
			body := []byte(`{"model":"native-model","max_tokens":64,"system":[{"type":"text","text":"x-anthropic-billing-header: kept"}],"messages":[{"role":"user","content":"SECRET PROMPT"}],"stream":true}`)
			wire := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":4,\"output_tokens\":1}}}\n\n" +
				"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"SECRET ANSWER\"}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, body) || r.URL.Path != test.upstream || r.URL.RawQuery != "beta=true" {
					t.Errorf("request changed: %s?%s", r.URL.Path, r.URL.RawQuery)
				}
				if r.Header.Get(test.credential) != test.value || len(r.Header.Values("Authorization"))+len(r.Header.Values("X-Api-Key")) != 1 || r.Header.Get("Chatgpt-Account-Id") != "" {
					t.Errorf("upstream credentials: %v", r.Header)
				}
				if r.Header.Get("Anthropic-Beta") != "a-2026-01-01,b-2026-02-02" || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("X-Claude-Code-Session-Id") != "session-1" || r.Header.Get("X-Olp-Private") != "" {
					t.Errorf("client headers changed: %v", r.Header)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Codex-Primary-Used-Percent", "100")
				for i := 0; i < len(wire); i += 5 {
					_, _ = io.WriteString(w, wire[i:min(i+5, len(wire))])
					w.(http.Flusher).Flush()
				}
			})
			header := http.Header{"X-Api-Key": {fullKey}, "Anthropic-Beta": {"a-2026-01-01,b-2026-02-02"}, "Anthropic-Version": {"2023-06-01"}, "X-Claude-Code-Session-Id": {"session-1"}, "X-Olp-Private": {"secret"}}
			response, got := codeVendorDo(t, server, http.MethodPost, "v1/messages?beta=true", body, header)
			if response.StatusCode != 200 || string(got) != wire {
				t.Fatalf("response changed: %d %q", response.StatusCode, got)
			}
			u := ledger.wait(t)
			if *u.Input != 14 || *u.Cached != 4 || *u.Output != 6 || *u.Total != 20 {
				t.Fatalf("usage: %+v", u)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			in := ledger.inputs[0]
			if in.Operation.Name != "messages" || in.Operation.Identity.Conversation != "session-1" || in.PreviousResponse != "" || len(ledger.allowances) != 0 {
				t.Fatalf("admission %+v allowances %v", in, ledger.allowances)
			}
		})
	}
}

func TestCodeChatSettlesOnlyReportedUsage(t *testing.T) {
	for _, reported := range []bool{true, false} {
		h, ledger, server := newCodeVendorHarness(t, codeplans.ZAIProfile)
		wire := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n"
		if reported {
			wire += "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}\n\n"
		}
		wire += "data: [DONE]\n\n"
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/a/coding/paas/v4/chat/completions" || r.Header.Get("Authorization") != "Bearer upstream-only" || r.Header.Get("X-Opencode-Parent-Session-Id") != "ses_root" {
				t.Errorf("request: %s %v", r.URL.Path, r.Header)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, wire)
		})
		header := http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_child"}, "X-Opencode-Parent-Session-Id": {"ses_root"}}
		response, got := codeVendorDo(t, server, http.MethodPost, "v1/chat/completions", []byte(`{"model":"native-model","stream":true}`), header)
		if response.StatusCode != 200 || string(got) != wire {
			t.Fatalf("response: %d %q", response.StatusCode, got)
		}
		u := ledger.wait(t)
		if reported != (u.Total != nil) || reported && *u.Total != 12 {
			t.Fatalf("reported=%v usage=%+v", reported, u)
		}
		ledger.mu.Lock()
		identity := ledger.inputs[0].Operation.Identity
		outcomes := ledger.outcomes
		ledger.mu.Unlock()
		if identity.Conversation != "ses:5fchild" || identity.Parent != "ses:5froot" || outcomes[len(outcomes)-1].Kind != "completed" {
			t.Fatalf("identity %+v outcomes %+v", identity, outcomes)
		}
	}
}

func TestCodeOpenCodeGoResponsesUseClientIdentityAndReferences(t *testing.T) {
	h, ledger, server := newCodeVendorHarness(t, codeplans.OpenCodeGoProfile)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a/responses" || r.Header.Get("Authorization") != "Bearer upstream-only" {
			t.Errorf("request: %s %v", r.URL.Path, r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"codex.rate_limits\",\"rate_limits\":{\"primary\":{\"used_percent\":100}}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
	})
	header := http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_1"}}
	if response, _ := codeVendorDo(t, server, http.MethodPost, "v1/responses", []byte(`{"model":"native-model","store":false}`), header); response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	if u := ledger.wait(t); *u.Total != 3 {
		t.Fatalf("usage %+v", u)
	}
	if response, _ := codeVendorDo(t, server, http.MethodPost, "v1/responses", []byte(`{"model":"native-model","previous_response_id":"resp_1"}`), header); response.StatusCode != 200 {
		t.Fatalf("continuation status %d", response.StatusCode)
	}
	ledger.wait(t)
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.inputs[1].PreviousResponse != "resp_1" || ledger.inputs[0].Operation.Identity.Conversation != "ses:5f1" || len(ledger.allowances) != 0 {
		t.Fatalf("inputs %+v allowances %v", ledger.inputs, ledger.allowances)
	}
}

func TestCodeAdaptersRefuseOtherPathsBeforeDispatch(t *testing.T) {
	claude := http.Header{"Authorization": {"Bearer " + fullKey}, "X-Claude-Code-Session-Id": {"s1"}}
	for _, test := range []struct {
		name, profile, method, path string
		header                      http.Header
		status                      int
		code                        string
		recorded                    bool
	}{
		{"codex path on z.ai", codeplans.ZAIProfile, http.MethodPost, "responses", claude, 400, "code_operation_unsupported", true},
		{"responses on z.ai", codeplans.ZAIProfile, http.MethodPost, "v1/responses", claude, 400, "code_operation_unsupported", true},
		{"models", codeplans.ZAIProfile, http.MethodGet, "v1/models", claude, 400, "code_operation_unsupported", true},
		{"websocket on opencode go", codeplans.OpenCodeGoProfile, http.MethodGet, "responses", http.Header{"Authorization": {"Bearer " + fullKey}, "Upgrade": {"websocket"}, "Connection": {"Upgrade"}}, 400, "code_operation_unsupported", true},
		{"count tokens probe", codeplans.ZAIProfile, http.MethodPost, "v1/messages/count_tokens", claude, 404, "code_operation_unsupported", false},
		{"connectivity probe", codeplans.OpenCodeGoProfile, http.MethodHead, "api/hello", claude, 404, "", false},
		{"count tokens probe on codex", codexauth.ProfileID, http.MethodPost, "v1/messages/count_tokens", claude, 400, "code_operation_unsupported", true},
		{"no identity", codeplans.ZAIProfile, http.MethodPost, "v1/messages", http.Header{"X-Api-Key": {fullKey}}, 400, "code_identity_invalid", true},
		{"unknown adapter", "reference-grant-chat", http.MethodPost, "v1/messages", claude, 503, "code_adapter_unavailable", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeVendorHarness(t, test.profile)
			response, body := codeVendorDo(t, server, test.method, test.path, []byte(`{"model":"native-model"}`), test.header)
			if response.StatusCode != test.status || test.code != "" && !bytes.Contains(body, []byte(test.code)) || h.mock.count("a") != 0 {
				t.Fatalf("status %d body %s dispatched %d", response.StatusCode, body, h.mock.count("a"))
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.inputs) != 0 || (len(ledger.refusals) == 1) != test.recorded {
				t.Fatalf("admitted %d, refusals %v", len(ledger.inputs), ledger.refusals)
			}
		})
	}
}

func TestCodeMixedRoutesDispatchEachPathToAnAccountOfItsAdapter(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	connection := func(profile string) runtime.Configuration {
		return runtime.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile, Endpoint: h.upstream.URL + "/a"}
	}
	h.rt.release.Snapshot.CodeConnections = map[string]runtime.Configuration{
		"revision:p-codex": connection(codexauth.ProfileID),
		"revision:p-glm":   connection(codeplans.ZAIProfile),
		"revision:p-go":    connection(codeplans.OpenCodeGoProfile),
	}
	if err := h.rt.release.Snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	type upstream struct {
		path   string
		header http.Header
	}
	requests := make(chan upstream, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		requests <- upstream{r.URL.Path, r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	for _, test := range []struct {
		path      string
		header    http.Header
		providers []string
		upstream  string
		codex     bool
	}{
		{"responses", http.Header{"Authorization": {"Bearer " + fullKey}, "Session_id": {"root"}}, []string{"p-codex"}, "/a/responses", true},
		{"v1/messages", http.Header{"X-Api-Key": {fullKey}, "X-Claude-Code-Session-Id": {"s1"}}, []string{"p-glm", "p-go"}, "/a/anthropic/v1/messages", false},
		{"v1/chat/completions", http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_a"}}, []string{"p-glm", "p-go"}, "/a/coding/paas/v4/chat/completions", false},
		{"v1/responses", http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_b"}}, []string{"p-go"}, "/a/responses", false},
	} {
		response, body := codeVendorDo(t, server, http.MethodPost, test.path, []byte(`{"model":"native-model"}`), test.header)
		if response.StatusCode != 200 {
			t.Fatalf("%s: %d %s", test.path, response.StatusCode, body)
		}
		got := <-requests
		ledger.wait(t)
		ledger.mu.Lock()
		providers := ledger.inputs[len(ledger.inputs)-1].Providers
		ledger.mu.Unlock()
		if got.path != test.upstream || !slices.Equal(providers, test.providers) || (got.header.Get("Chatgpt-Account-Id") != "") != test.codex {
			t.Fatalf("%s: upstream %s providers %v headers %v", test.path, got.path, providers, got.header)
		}
	}
	response, _ := codeVendorDo(t, server, http.MethodPost, "v1/messages/count_tokens", []byte(`{}`), http.Header{"X-Api-Key": {fullKey}})
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if response.StatusCode != 404 || len(ledger.refusals) != 0 {
		t.Fatalf("probe on a route serving Messages: %d refusals %v", response.StatusCode, ledger.refusals)
	}
}

func TestCodeMessagesRefusalsUseTheAnthropicEnvelope(t *testing.T) {
	_, _, server := newCodeVendorHarness(t, codeplans.ZAIProfile)
	response, body := codeVendorDo(t, server, http.MethodPost, "v1/messages", []byte(`{"model":"bad model"}`), http.Header{"X-Api-Key": {fullKey}, "X-Claude-Code-Session-Id": {"s1"}})
	var envelope struct {
		Type  string
		Error struct{ Type, Code string }
	}
	if response.StatusCode != 400 || json.Unmarshal(body, &envelope) != nil || envelope.Type != "error" || envelope.Error.Type != "invalid_request_error" || envelope.Error.Code != "code_model_invalid" {
		t.Fatalf("envelope %d %s", response.StatusCode, body)
	}
	_, _, server = newCodeVendorHarness(t, codeplans.ZAIProfile)
	response, body = codeVendorDo(t, server, http.MethodPost, "v1/chat/completions", []byte(`{"model":"bad model"}`), http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"s"}})
	if response.StatusCode != 400 || !bytes.Contains(body, []byte(`"type":"code_mode_error"`)) {
		t.Fatalf("chat envelope %d %s", response.StatusCode, body)
	}
}

type codeHeaderAuthorizer http.Header

func (a codeHeaderAuthorizer) AuthorizeCode(context.Context, runtime.Configuration, codemode.Account, codemode.Dispatch) (codemode.Authorization, error) {
	return codemode.Authorization{Principal: "principal", Headers: http.Header(a)}, nil
}

func TestCodeAuthorizationCarriesOnlyTheAdaptersHeaders(t *testing.T) {
	for _, headers := range []http.Header{
		{"Authorization": {"Bearer a"}, "X-Api-Key": {"b"}},
		{"Authorization": {"Bearer a"}, "Chatgpt-Account-Id": {"c"}},
		{"X-Api-Key": {"b"}},
		{"Authorization": {"Bearer a", "Bearer b"}},
	} {
		h, _, server := newCodeVendorHarness(t, codeplans.ZAIProfile)
		h.gateway.CodeAuthorizer = codeHeaderAuthorizer(headers)
		response, body := codeVendorDo(t, server, http.MethodPost, "v1/messages", []byte(`{"model":"native-model"}`), http.Header{"X-Api-Key": {fullKey}, "X-Claude-Code-Session-Id": {"s1"}})
		if response.StatusCode != 503 || !strings.Contains(string(body), "code_authorization_invalid") || h.mock.count("a") != 0 {
			t.Fatalf("%v: %d %s", headers, response.StatusCode, body)
		}
	}
}
