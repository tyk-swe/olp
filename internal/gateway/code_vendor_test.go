package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
)

// newCodeVendorHarness serves the "coding" route through a coding-plan
// profile's connection instead of Codex's.
func newCodeVendorHarness(t *testing.T, profile string) (*harness, *codeTestLedger, *httptest.Server) {
	t.Helper()
	h, ledger, server := newCodeForwardHarness(t)
	h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile, Endpoint: h.upstream.URL + "/a"}}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
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
	if response, _ := codeVendorDo(t, server, http.MethodPost, "v1/responses", []byte(`{"model":"gpt-5.5","store":false}`), header); response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	if u := ledger.wait(t); *u.Total != 3 {
		t.Fatalf("usage %+v", u)
	}
	if response, _ := codeVendorDo(t, server, http.MethodPost, "v1/responses", []byte(`{"model":"gpt-5.5","previous_response_id":"resp_1"}`), header); response.StatusCode != 200 {
		t.Fatalf("continuation status %d", response.StatusCode)
	}
	ledger.wait(t)
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.inputs[1].PreviousResponse != "resp_1" || ledger.inputs[0].Operation.Identity.Conversation != "ses:5f1" || len(ledger.allowances) != 0 {
		t.Fatalf("inputs %+v allowances %v", ledger.inputs, ledger.allowances)
	}
}

func TestCodeMixedRoutesDispatchEachPathToAnAccountOfItsAdapter(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	connection := func(profile string) routing.Configuration {
		return routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile, Endpoint: h.upstream.URL + "/a"}
	}
	h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{
		"revision:p-codex": connection(codexauth.ProfileID),
		"revision:p-glm":   connection(codeplans.ZAIProfile),
		"revision:p-go":    connection(codeplans.OpenCodeGoProfile),
	}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
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
		model     string
		header    http.Header
		providers []string
		upstream  string
		codex     bool
	}{
		{"responses", "native-model", http.Header{"Authorization": {"Bearer " + fullKey}, "Session_id": {"root"}}, []string{"p-codex"}, "/a/responses", true},
		{"v1/messages", "minimax-m3", http.Header{"X-Api-Key": {fullKey}, "X-Claude-Code-Session-Id": {"s1"}}, []string{"p-glm", "p-go"}, "/a/anthropic/v1/messages", false},
		{"v1/messages", "glm-5.3", http.Header{"X-Api-Key": {fullKey}, "X-Claude-Code-Session-Id": {"s2"}}, []string{"p-glm"}, "/a/anthropic/v1/messages", false},
		{"v1/chat/completions", "glm-5.3", http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_a"}}, []string{"p-glm", "p-go"}, "/a/coding/paas/v4/chat/completions", false},
		{"v1/responses", "gpt-5.5", http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_b"}}, []string{"p-go"}, "/a/responses", false},
	} {
		response, body := codeVendorDo(t, server, http.MethodPost, test.path, []byte(`{"model":"`+test.model+`"}`), test.header)
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
	ledger.mu.Lock()
	inputs := len(ledger.inputs)
	ledger.mu.Unlock()
	response, body := codeVendorDo(t, server, http.MethodPost, "v1/responses", []byte(`{"model":"glm-5.3"}`), http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_c"}})
	ledger.mu.Lock()
	admitted := len(ledger.inputs) != inputs
	ledger.mu.Unlock()
	if response.StatusCode != 400 || !strings.Contains(string(body), "code_operation_unsupported") || admitted {
		t.Fatalf("model no adapter serves on the path: %d %s admitted %v", response.StatusCode, body, admitted)
	}
	response, _ = codeVendorDo(t, server, http.MethodPost, "v1/messages/count_tokens", []byte(`{}`), http.Header{"X-Api-Key": {fullKey}})
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if response.StatusCode != 404 || !slices.Equal(ledger.refusals, []string{"code_operation_unsupported"}) {
		t.Fatalf("probe on a route serving Messages: %d refusals %v", response.StatusCode, ledger.refusals)
	}
}

type codeHeaderAuthorizer http.Header

func (a codeHeaderAuthorizer) AuthorizeCode(context.Context, routing.Configuration, codemode.Account, codemode.Dispatch) (codemode.Authorization, error) {
	return codemode.Authorization{Principal: "principal", Headers: http.Header(a)}, nil
}

func TestCodeAuthorizationHeadersRefuseBeforeEitherTransportDispatches(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers http.Header
	}{
		{"missing", nil},
		{"wrong-credential", http.Header{"X-Api-Key": {"key"}}},
		{"undeclared", http.Header{"Authorization": {"Bearer key"}, "X-Extra": {"value"}}},
		{"multiple", http.Header{"Authorization": {"Bearer key", "Bearer other"}}},
		{"no-value", http.Header{"Authorization": nil}},
		{"empty", http.Header{"Authorization": {""}}},
		{"whitespace", http.Header{"Authorization": {" \t"}}},
		{"newline", http.Header{"Authorization": {"Bearer key\r\nX-Extra: injected"}}},
		{"nul", http.Header{"Authorization": {"Bearer key\x00"}}},
		{"delete", http.Header{"Authorization": {"Bearer key\x7f"}}},
		{"duplicate-name", http.Header{"Authorization": {"Bearer key"}, "authorization": {"Bearer other"}}},
		{"extra-multiple", http.Header{"Authorization": {"Bearer key"}, "Chatgpt-Account-Id": {"account", "other"}}},
	} {
		for _, transport := range []string{"http", "websocket"} {
			t.Run(test.name+"/"+transport, func(t *testing.T) {
				h, ledger, server := newCodeForwardHarness(t)
				h.gateway.CodeAuthorizer = codeHeaderAuthorizer(test.headers)
				var response *http.Response
				if transport == "http" {
					response = codeDo(t, server, []byte(`{"model":"native-model"}`), nil)
				} else {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					conn, rejected, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"invalid-authorization"}}})
					if conn != nil {
						conn.CloseNow()
					}
					if err == nil || rejected == nil {
						t.Fatalf("handshake accepted: %v", err)
					}
					response = rejected
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != 503 || !strings.Contains(string(body), "code_authorization_invalid") {
					t.Fatalf("%d %s", response.StatusCode, body)
				}
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				if len(ledger.marks) != 0 || h.mock.count("a") != 0 {
					t.Fatal("invalid authorization dispatched")
				}
				if transport == "http" && len(ledger.aborts) != 1 {
					t.Fatal("prepared admission was not aborted")
				}
			})
		}
	}
}

func TestCodePrincipalMismatchRefusesBothTransports(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			h.gateway.CodeAuthorizer = codeTestAuthorizer{principal: "another-principal"}
			var response *http.Response
			status, code := 409, "code_principal_mismatch"
			if transport == "http" {
				response = codeDo(t, server, []byte(`{"model":"native-model"}`), nil)
			} else {
				status, code = 503, "code_account_unavailable"
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				conn, rejected, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"principal-mismatch"}}})
				if conn != nil {
					conn.CloseNow()
				}
				if err == nil || rejected == nil {
					t.Fatalf("handshake accepted: %v", err)
				}
				response = rejected
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != status || !strings.Contains(string(body), code) {
				t.Fatalf("%d %s", response.StatusCode, body)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.marks) != 0 || h.mock.count("a") != 0 {
				t.Fatal("principal mismatch dispatched")
			}
		})
	}
}

func TestCodeSelectedEndpointOwnsResponseObservations(t *testing.T) {
	for _, test := range []struct {
		profile, path, model, response, contentType string
		allowance                                   bool
	}{
		{codexauth.ProfileID, "responses", "native-model", "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n", "text/event-stream", true},
		{codeplans.OpenCodeGoProfile, "v1/responses", "gpt-5.5", "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n", "text/event-stream", false},
		{codeplans.ZAIProfile, "v1/messages", "glm-5.3", `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`, "application/json", false},
		{codeplans.BigModelProfile, "v1/messages", "glm-5.3", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "text/event-stream", false},
		{codeplans.OpenCodeGoProfile, "v1/chat/completions", "glm-5.3", "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n", "text/event-stream", false},
	} {
		t.Run(test.profile+"/"+test.path, func(t *testing.T) {
			h, ledger, server := newCodeVendorHarness(t, test.profile)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("X-Codex-Primary-Used-Percent", "50")
				_, _ = io.WriteString(w, test.response)
			})
			header := http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"codex-observation"}, "X-Opencode-Session-Id": {"client-observation"}}
			response, raw := codeVendorDo(t, server, http.MethodPost, test.path, []byte(`{"model":"`+test.model+`"}`), header)
			if response.StatusCode != 200 || string(raw) != test.response {
				t.Fatalf("response changed: %d %s", response.StatusCode, raw)
			}
			if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
				t.Fatalf("usage %+v", usage)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if (len(ledger.allowances) != 0) != test.allowance {
				t.Fatalf("allowances: %+v", ledger.allowances)
			}
		})
	}
}

func TestCodeWebSocketRequiresDeclaredTransport(t *testing.T) {
	for _, test := range []struct{ profile, path string }{
		{codeplans.OpenCodeGoProfile, "v1/responses"},
		{codexauth.ProfileID, "responses/compact"},
	} {
		t.Run(test.profile, func(t *testing.T) {
			h, ledger, server := newCodeVendorHarness(t, test.profile)
			response, body := codeVendorDo(t, server, http.MethodGet, test.path, nil, http.Header{"Authorization": {"Bearer " + fullKey}, "Upgrade": {"websocket"}, "Thread-Id": {"unsupported"}})
			if response.StatusCode != 400 || !strings.Contains(string(body), "code_operation_unsupported") {
				t.Fatalf("%d %s", response.StatusCode, body)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.inputs) != 0 || h.mock.count("a") != 0 {
				t.Fatal("unsupported transport admitted")
			}
		})
	}
}
