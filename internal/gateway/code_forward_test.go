package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

type codeTestRefusal struct {
	key, digest, code string
}

type codeTestLedger struct {
	refusalRecords []codeTestRefusal
	mu             sync.Mutex
	inputs         []resources.CodeAdmission
	marks          []string
	aborts         []string
	usages         []codemode.Usage
	health         codemode.HealthObservation
	allowance      codemode.Allowance
	refusals       []string
	refuse         string
	done           chan codemode.Usage
	references     map[string]bool
	outcomes       []codemode.Outcome
	offers         codeclient.Offers
	modelsErr      error
	pinTarget      string
}

func (l *codeTestLedger) Models(context.Context, codemode.Route, string) (codeclient.Offers, error) {
	return l.offers, l.modelsErr
}

func (l *codeTestLedger) ObserveOutcome(_ context.Context, _ string, outcome codemode.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.outcomes = append(l.outcomes, outcome)
	return nil
}

func (l *codeTestLedger) Admit(_ context.Context, in resources.CodeAdmission) (resources.CodePermit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inputs = append(l.inputs, in)
	if in.PreviousResponse != "" && !l.references[in.PreviousResponse] {
		return resources.CodePermit{}, codemode.Refuse(409, "code_parent_unresolved")
	}
	if l.refuse != "" {
		return resources.CodePermit{}, codemode.Refuse(409, l.refuse)
	}
	permit, err := codeTestPermit(in.APIKeyID, in.Providers)
	permit.Authority.EndUserDigest = in.EndUserDigest
	permit.InitialRecovery = in.InitialRecovery && in.Operation.Identity.Parent == "" && in.PreviousResponse == ""
	permit.Attempt = codemode.Attempt{ID: fmt.Sprint(len(l.inputs))}
	permit.Pin.TargetModel = l.pinTarget
	return permit, err
}

// codeTestPermit pins an account of the first provider serving the request.
func codeTestPermit(key string, providers []string) (resources.CodePermit, error) {
	if len(providers) == 0 {
		return resources.CodePermit{}, codemode.Refuse(503, "code_account_unavailable")
	}
	return resources.CodePermit{
		Authority: access.Authority{ID: key, Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}}},
		Account:   codemode.Account{ID: "account", ProviderID: providers[0], Principal: "principal"},
		Binding:   codemode.Binding{ID: "binding", AccountID: "account", Principal: "principal"},
		Pin:       codemode.Pin{AccountID: "account", Principal: "principal"},
	}, nil
}
func (l *codeTestLedger) MarkDispatched(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.marks = append(l.marks, id)
	return nil
}
func (l *codeTestLedger) ObserveReference(_ context.Context, _, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.references == nil {
		l.references = map[string]bool{}
	}
	l.references[id] = true
	return nil
}
func (l *codeTestLedger) BindConnection(_ context.Context, _ codemode.Route, key string, _ codemode.Identity, _ string, providers []string) (resources.CodePermit, error) {
	return codeTestPermit(key, providers)
}
func (l *codeTestLedger) Settle(_ context.Context, _ string, u codemode.Usage) error {
	l.mu.Lock()
	l.usages = append(l.usages, u)
	l.mu.Unlock()
	l.done <- u
	return nil
}
func (l *codeTestLedger) Abort(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.aborts = append(l.aborts, id)
	return nil
}
func (l *codeTestLedger) ObserveHealth(_ context.Context, _ string, status codemode.HealthObservation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.health = status
	return nil
}
func (l *codeTestLedger) ObserveAllowance(_ context.Context, _ string, a codemode.Allowance) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.allowance = l.allowance.Merge(a)
	return nil
}
func (l *codeTestLedger) RecordRefusal(_ context.Context, _ codemode.Route, key, digest string, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refusals = append(l.refusals, reason)
	l.refusalRecords = append(l.refusalRecords, codeTestRefusal{key: key, digest: digest, code: reason})
	return nil
}
func (l *codeTestLedger) wait(t *testing.T) codemode.Usage {
	t.Helper()
	select {
	case u := <-l.done:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("attempt not settled")
		return codemode.Usage{}
	}
}

type codeTestAuthorizer struct{ principal string }

func (a codeTestAuthorizer) AuthorizeCode(_ context.Context, _ routing.Configuration, _ codemode.Account, dispatch codemode.Dispatch) (codemode.Authorization, error) {
	vendor, _ := codeadapter.Lookup(dispatch.Adapter)
	endpoint, _ := vendor.Endpoint(dispatch.ClientPath)
	value := "upstream-only"
	if endpoint.CredentialHeader == "Authorization" {
		value = "Bearer " + value
	}
	headers := http.Header{endpoint.CredentialHeader: {value}}
	if endpoint.Observation == codeadapter.ObserveCodex {
		headers.Set("Chatgpt-Account-Id", "upstream-account")
	}
	return codemode.Authorization{Principal: a.principal, Headers: headers}, nil
}

type codeForwardSetup struct {
	Models      []string
	Policy      access.KeyPolicy
	Offers      codeclient.Offers
	ModelsError error
}

func newCodeForwardHarness(t *testing.T, setups ...codeForwardSetup) (*harness, *codeTestLedger, *httptest.Server) {
	t.Helper()
	h := newHarness(t, Config{})
	setup := func() codeForwardSetup {
		if len(setups) > 0 {
			return setups[0]
		}
		return codeForwardSetup{Models: []string{"native-model"}, Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}}}
	}()
	ledger := &codeTestLedger{done: make(chan codemode.Usage, 100), offers: setup.Offers, modelsErr: setup.ModelsError}
	publishedAt := time.Now()
	route := codemode.Route{ID: "route", Slug: "coding", ProjectID: "project", PoolID: "pool", RevisionID: "revision", Revision: 1, PublishedAt: &publishedAt, Enabled: true, Models: setup.Models}
	authority := access.Authority{ID: h.rt.keys[fullKey].ID, ProjectID: &route.ProjectID, Policy: setup.Policy}
	h.rt.keys[fullKey] = authority
	h.rt.release.Snapshot.CodeRoutes = map[string]codemode.Route{"coding": route}
	h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codexauth.ProfileID, Endpoint: h.upstream.URL + "/a"}}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	h.gateway.CodeLedger = ledger
	h.gateway.CodeAuthorizer = codeTestAuthorizer{principal: "principal"}
	mux := http.NewServeMux()
	h.gateway.RegisterCode(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return h, ledger, server
}

func codeDo(t *testing.T, server *httptest.Server, body []byte, extra http.Header) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = extra.Clone()
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Set("Authorization", "Bearer "+fullKey)
	r.Header.Set("Session_id", "root")
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestCodeModelDiscoveryUsesAuthorizedOffers(t *testing.T) {
	_, _, server := newCodeForwardHarness(t, codeForwardSetup{
		Models: []string{"private-code-v42", "unavailable-model"},
		Policy: access.KeyPolicy{Scopes: []string{"inference"}},
		Offers: codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: []string{"private-code-v42", "unpublished-model"}}},
	})
	for _, path := range []string{"models", "v1/models"} {
		t.Run(path, func(t *testing.T) {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/code/coding/"+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer "+fullKey)
			response, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body struct {
				Object string `json:"object"`
				Data   []struct {
					ID           string `json:"id"`
					Capabilities []struct {
						Protocol   string   `json:"protocol"`
						Path       string   `json:"path"`
						Transports []string `json:"transports"`
					} `json:"capabilities"`
				} `json:"data"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || body.Object != "list" || len(body.Data) != 1 || body.Data[0].ID != "private-code-v42" {
				t.Fatalf("discovery status %d body %+v", response.StatusCode, body)
			}
			if len(body.Data[0].Capabilities) == 0 || body.Data[0].Capabilities[0].Protocol != "responses" || body.Data[0].Capabilities[0].Path != "responses" {
				t.Fatalf("missing native protocol capabilities %+v", body)
			}

			for _, capability := range body.Data[0].Capabilities {
				expected := func() []string {
					if capability.Path == "responses" {
						return []string{"http", "websocket"}
					}
					return []string{"http"}
				}()
				if !slices.Equal(capability.Transports, expected) {
					t.Fatalf("transports must use public names, got %q", capability.Transports)
				}
			}
		})
	}
}

func TestCodeModelDiscoveryEnforcesAuthority(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       string
		liveError error
		status    int
	}{
		{name: "missing key", status: 401},
		{name: "read scope only", key: readKey, status: 403},
		{name: "other route", key: otherKey, status: 404},
		{name: "pool assignment revoked", key: fullKey, liveError: codemode.Refuse(403, "code_pool_denied"), status: 403},
		{name: "key revoked", key: fullKey, liveError: codemode.Refuse(403, "code_permission_denied"), status: 403},
		{name: "revision changed", key: fullKey, liveError: codemode.Refuse(409, "code_route_revision_changed"), status: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, server := newCodeForwardHarness(t, codeForwardSetup{
				Models: []string{"native-model"}, Policy: access.KeyPolicy{Scopes: []string{"inference"}},
				Offers: codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: []string{"native-model"}}}, ModelsError: tc.liveError,
			})
			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/code/coding/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.key != "" {
				r.Header.Set("Authorization", "Bearer "+tc.key)
			}
			response, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status || strings.Contains(string(body), "native-model") {
				t.Fatalf("unauthorized discovery %d %s", response.StatusCode, body)
			}
		})
	}
}

// The revision froze another provider's connection, not the account's.

func TestCodeNativeMessagesPreservePayloadForSelectedAccount(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.MessagesToCodex = enabled
			h.rt.release.Snapshot.CodeRoutes["coding"] = route
			configuration := routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codeplans.ZAIProfile, Endpoint: h.upstream.URL + "/a"}
			h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": configuration}
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			adapter, ok := codeadapter.ForConnection(configuration.Kind, configuration.AuthMode, configuration.ProfileID)
			if !ok {
				t.Fatal("selected account has no adapter")
			}
			endpoint, ok := adapter.Endpoint("v1/messages")
			if !ok {
				t.Fatal("selected account cannot serve Messages")
			}
			requestBody := []byte(`{ "model":"native-model", "messages":[{"role":"user","content":"hello"}], "max_tokens":8, "vendor_extension":{"keep":true} }`)
			responseBody := []byte(`{ "type":"message", "content":[{"type":"text","text":"native reply"}], "stop_reason":"end_turn", "usage":{"input_tokens":2,"output_tokens":1}, "vendor_extension":{"keep":true} }`)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, requestBody) {
					t.Error("native request bytes changed")
				}
				if r.URL.Path != "/a/"+endpoint.UpstreamPath || r.URL.RawQuery != "native_option=1" {
					t.Error("request did not use the selected account's endpoint")
				}
				if r.Header.Get(endpoint.CredentialHeader) != "Bearer upstream-only" || r.Header.Get("X-Api-Key") != "" {
					t.Error("request did not use the selected account's credential header")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(responseBody)
			})
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/messages?native_option=1", bytes.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Api-Key", fullKey)
			request.Header.Set("X-Claude-Code-Session-Id", "native-messages")
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, responseBody) {
				t.Fatalf("native response changed: status=%d body=%s error=%v", response.StatusCode, body, err)
			}
			if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
				t.Fatalf("selected account's response usage was lost: %+v", usage)
			}
			if h.mock.count("a") != 1 {
				t.Fatal("native request dispatched more than once")
			}
		})
	}
}

func TestCodeModelDiscoveryWithNativeEndUserSource(t *testing.T) {
	_, _, server := newCodeForwardHarness(t, codeForwardSetup{
		Models: []string{"native-model"}, Policy: access.KeyPolicy{Scopes: []string{"inference"}, EndUserSource: new("native")},
		Offers: codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: []string{"native-model"}}},
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/code/coding/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fullKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var catalog struct {
		Data []codemode.Model `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || len(catalog.Data) != 1 || catalog.Data[0].ID != "native-model" {
		t.Fatalf("discovery required a generation identity, status %d, catalog %+v", response.StatusCode, catalog)
	}
	generation := codeDo(t, server, []byte(`{"model":"native-model","input":[]}`), http.Header{"Content-Type": {"application/json"}})
	if generation.StatusCode != http.StatusBadRequest {
		t.Fatalf("generation accepted a missing native identity, status %d", generation.StatusCode)
	}
}

func TestCodeMessagesToCodexTextExchange(t *testing.T) {
	for _, query := range []string{"", "?beta=true"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprint(stream)+query, func(t *testing.T) {
				g, ledger, server := newCodeForwardHarness(t)
				route := g.rt.release.Snapshot.CodeRoutes["coding"]
				route.MessagesToCodex = true
				g.rt.release.Snapshot.CodeRoutes["coding"] = route
				g.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					var request struct {
						Model string `json:"model"`
						Input []struct {
							Role    string `json:"role"`
							Content []struct {
								Text string `json:"text"`
							} `json:"content"`
						} `json:"input"`
						Stream          bool    `json:"stream"`
						Store           *bool   `json:"store"`
						Instructions    *string `json:"instructions"`
						MaxOutputTokens int     `json:"max_output_tokens"`
					}
					if err := json.Unmarshal(body, &request); err != nil || request.Model != "native-model" || len(request.Input) != 2 || request.Input[0].Role != "developer" || request.Input[0].Content[0].Text != "Keep the answer short\nx-anthropic-billing-header: preserve" || request.Input[1].Content[0].Text != "Hello" || !request.Stream || request.MaxOutputTokens != 32 || request.Store == nil || *request.Store || request.Instructions == nil || *request.Instructions != "" {
						t.Errorf("translated request %s, error %v", body, err)
					}
					if r.URL.Path != "/a/responses" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer upstream-only" || r.Header.Get("Anthropic-Beta") != "" || r.Header.Get("X-Claude-Code-Session-Id") != "translated-text" {
						t.Errorf("unexpected target %s, headers %v", r.URL.Path, r.Header)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: "+`{"type":"response.created","response":{"id":"response-text","model":"native-model","status":"in_progress","output":[]}}`+"\n\n")
					fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello back"}`+"\n\n")
					fmt.Fprint(w, "data: "+`{"type":"response.completed","sequence_number":0,"response":{"id":"response-text","object":"response","model":"native-model","status":"completed","output":[{"id":"message-text","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello back","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`+"\n\n")
				})
				request, _ := http.NewRequest(http.MethodPost, server.URL+"/code/coding/v1/messages"+query, bytes.NewBufferString(fmt.Sprintf(`{"model":"native-model","system":"Keep the answer short\nx-anthropic-billing-header: preserve","messages":[{"role":"user","content":"Hello"}],"max_tokens":32,"stream":%t}`, stream)))
				request.Header.Set("Authorization", "Bearer "+fullKey)
				request.Header.Set("X-Claude-Code-Session-Id", "translated-text")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != 200 || !bytes.Contains(body, []byte(`"text":"Hello back"`)) || !bytes.Contains(body, []byte(`"model":"native-model"`)) || !bytes.Contains(body, []byte(`"output_tokens":2`)) {
					t.Fatalf("Messages exchange status %d body %s", response.StatusCode, body)
				}
				if stream && (!bytes.Contains(body, []byte("event: message_start")) || !bytes.Contains(body, []byte("event: message_stop")) || !bytes.Contains(body, []byte("event: content_block_delta"))) {
					t.Fatalf("incomplete Messages stream %s", body)
				}
				usage := ledger.wait(t)
				if usage.Total == nil || *usage.Total != 9 || len(ledger.marks) != 1 {
					t.Fatalf("usage=%+v, marks=%v", usage, ledger.marks)
				}

			})
		}
	}
}

func TestCodeMessagesToCodexCompletedMessages(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			g, ledger, server := newCodeForwardHarness(t)
			route := g.rt.release.Snapshot.CodeRoutes["coding"]
			route.MessagesToCodex = true
			g.rt.release.Snapshot.CodeRoutes["coding"] = route
			g.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range []string{
					`{"type":"response.created","response":{"id":"completed-messages","status":"in_progress","output":[]}}`,
					`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}`,
					`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello "}]}}`,
					`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"back"}]}}`,
					`{"type":"response.completed","response":{"id":"completed-messages","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`,
				} {
					fmt.Fprint(w, "data: "+event+"\n\n")
				}
			})
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"native-model","messages":[{"role":"user","content":"Hello"}],"max_tokens":32,"stream":%t}`, stream)))
			request.Header.Set("Authorization", "Bearer "+fullKey)
			request.Header.Set("X-Claude-Code-Session-Id", "completed-messages")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("completed messages status %d %s", response.StatusCode, body)
			}
			text, err := func() (string, error) {
				if stream {
					capture := protocols.NewCaptureStream(wire.Anthropic, 8192, true, false)
					if err := capture.Push(body); err != nil {
						return "", err
					}
					text, _, err := capture.Finish()
					return text, err
				}
				completion, err := protocols.Decode(wire.Anthropic, wire.Anthropic, body, "native-model", "")
				if err != nil {
					return "", err
				}
				return completion.OutputText, nil
			}()
			if err != nil || text != "Hello back" || !bytes.Contains(body, []byte(`"stop_reason":"end_turn"`)) {
				t.Fatalf("completed message text %q error %v body %s", text, err, body)
			}
			if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 9 || g.mock.count("a") != 1 {
				t.Fatalf("completed message accounting %+v", usage)
			}
		})
	}
}

func TestCodeMessagesToCodexRefusesUnsupportedSemantics(t *testing.T) {
	for _, test := range []struct{ name, extra, query, beta, messages string }{
		{name: "tool extension", extra: `,"tools":[{"name":"run","input_schema":{"type":"object"},"required_extension":true}]`},
		{name: "error-marked tool result", messages: `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-read","is_error":true,"content":"failed"}]}]`},
		{name: "media tool result", messages: `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-read","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}]}]`},
		{name: "adaptive numeric budget", extra: `,"thinking":{"type":"adaptive","budget_tokens":1024},"output_config":{"effort":"high"}`},
		{name: "unsupported effort", extra: `,"output_config":{"effort":"max"}`},
		{name: "adaptive without effort", extra: `,"thinking":{"type":"adaptive"}`},
		{name: "signed thinking history", messages: `[{"role":"assistant","content":[{"type":"thinking","thinking":"private-prompt","signature":"opaque-signature"}]},{"role":"user","content":"Continue"}]`},
		{name: "redacted thinking history", messages: `[{"role":"assistant","content":[{"type":"redacted_thinking","data":"private-prompt"}]},{"role":"user","content":"Continue"}]`},
		{name: "opaque compaction history", messages: `[{"role":"user","content":[{"type":"compaction","content":"private-prompt"}]}]`},
		{name: "thinking history", extra: `,"system":[{"type":"thinking","thinking":"private-prompt","signature":"opaque"}]`},
		{name: "unsupported tool choice", extra: `,"tool_choice":{"type":"auto","required_extension":true}`},
		{name: "thinking", extra: `,"thinking":{"type":"enabled","budget_tokens":1024}`},
		{name: "stream options", extra: `,"stream_options":{"required_extension":true}`},
		{name: "stop", extra: `,"stop_sequences":["stop"]`},
		{name: "unknown", extra: `,"required_extension":{"value":"private-prompt"}`},
		{name: "beta", beta: "required-protocol"},
		{name: "query", query: "?required_feature=true"},
		{name: "unsupported beta query", query: "?beta=required-feature"},
		{name: "duplicate beta query", query: "?beta=true&beta=true"},
		{name: "beta plus unknown query", query: "?beta=true&required_feature=true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			g, ledger, server := newCodeForwardHarness(t)
			route := g.rt.release.Snapshot.CodeRoutes["coding"]
			route.MessagesToCodex = true
			g.rt.release.Snapshot.CodeRoutes["coding"] = route
			messages := func() string {
				if test.messages != "" {
					return test.messages
				}
				return `[{"role":"user","content":"private-prompt"}]`
			}()
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/messages"+test.query, strings.NewReader(`{"model":"native-model","messages":`+messages+`,"max_tokens":32`+test.extra+`}`))
			request.Header.Set("Authorization", "Bearer "+fullKey)
			request.Header.Set("X-Claude-Code-Session-Id", "unsupported-text")
			if test.beta != "" {
				request.Header.Set("Anthropic-Beta", test.beta)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != 422 || !bytes.Contains(body, []byte("code_translation_unsupported_semantics")) || bytes.Contains(body, []byte("private-prompt")) {
				t.Fatalf("unsupported request status=%d body=%s", response.StatusCode, body)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.marks) != 0 {
				t.Fatalf("unsupported request dispatched: %v", ledger.marks)
			}
		})
	}
}

func TestCodeMessagesToCodexPreservesNativeModelIdentifiers(t *testing.T) {
	model := "Namespace/" + strings.Repeat("M", 182) + ":Variant"
	g, ledger, server := newCodeForwardHarness(t, codeForwardSetup{Models: []string{model}, Policy: access.KeyPolicy{Scopes: []string{"inference"}}})
	route := g.rt.release.Snapshot.CodeRoutes["coding"]
	route.MessagesToCodex = true
	g.rt.release.Snapshot.CodeRoutes["coding"] = route
	g.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != model {
			t.Errorf("native model changed, model %q, error %v", request.Model, err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"response-model","object":"response","model":%q,"status":"completed","output":[{"id":"message-model","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"native model reply"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`, model)
	})
	body := []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"max_tokens":8}`, model))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("X-Claude-Code-Session-Id", "native-model")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var message struct {
		Model   string `json:"model"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(response.Body).Decode(&message); err != nil || response.StatusCode != 200 || message.Model != model || len(message.Content) != 1 || message.Content[0].Text != "native model reply" {
		t.Fatalf("native model exchange status %d, message %+v, error %v", response.StatusCode, message, err)
	}
	usage := ledger.wait(t)
	if usage.Total == nil || *usage.Total != 3 {
		t.Fatalf("native model usage %+v", usage)
	}
	ordinary := g.do(t.Context(), http.MethodPost, "/anthropic/v1/messages", fullKey, body, nil)
	defer ordinary.Body.Close()
	if ordinary.StatusCode != 400 {
		t.Fatalf("ordinary route accepted a native model identifier, status %d", ordinary.StatusCode)
	}
}

func (l *codeTestLedger) ConfirmNonexecution(context.Context, string, int) error   { return nil }
func (l *codeTestLedger) ObserveCooldown(context.Context, string, time.Time) error { return nil }

func TestCodeMessagesToCodexToolExchange(t *testing.T) {
	for _, source := range []string{"fragments", "complete", "arguments-done"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprint(stream)+"/"+source, func(t *testing.T) {
				g, ledger, server := newCodeForwardHarness(t)
				route := g.rt.release.Snapshot.CodeRoutes["coding"]
				route.MessagesToCodex = true
				g.rt.release.Snapshot.CodeRoutes["coding"] = route
				g.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					var request struct {
						Input []struct {
							Type, CallID, Name, Arguments, Output string
						} `json:"input"`
						Tools []struct {
							Type, Name, Description string
							Parameters              json.RawMessage
						} `json:"tools"`
						ToolChoice struct{ Type, Name string } `json:"tool_choice"`
						Parallel   *bool                       `json:"parallel_tool_calls"`
					}
					body, _ := io.ReadAll(r.Body)
					if err := json.Unmarshal(body, &request); err != nil {
						t.Error(err)
					}
					if len(request.Tools) != 1 || request.Tools[0].Name != "read_file" || !bytes.Contains(request.Tools[0].Parameters, []byte(`"path"`)) || request.ToolChoice.Type != "function" || request.ToolChoice.Name != "read_file" || request.Parallel == nil || *request.Parallel {
						t.Errorf("tool controls lost %s", body)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: "+`{"type":"response.created","response":{"id":"response-tools","model":"native-model","status":"in_progress","output":[]}}`+"\n\n")
					if len(request.Input) > 1 {
						if !bytes.Contains(body, []byte(`"call_id":"call-read"`)) || !bytes.Contains(body, []byte(`"arguments":"{\"path\":\"main.go\"}"`)) || !bytes.Contains(body, []byte(`"output":"package main\n"`)) {
							t.Errorf("tool history or result lost %s", body)
						}
						if source == "fragments" {
							fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"File read"}`+"\n\n")
						}
						fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"response-tools","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"File read"}]}],"usage":{"input_tokens":13,"output_tokens":2,"total_tokens":15}}}`+"\n\n")
						return
					}
					if source == "complete" {
						fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"response-tools","status":"completed","output":[{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`+"\n\n")
						return
					}
					if source == "arguments-done" {
						for _, event := range []string{
							`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":""}}`,
							`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"main.go\"}"}`,
							`{"type":"response.completed","response":{"id":"response-tools","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`,
						} {
							fmt.Fprint(w, "data: "+event+"\n\n")
						}
						return
					}
					for _, event := range []string{
						`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":""}}`,
						`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`,
						`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"main.go\"}"}`,
						`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}}`,
						`{"type":"response.completed","response":{"id":"response-tools","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`,
					} {
						fmt.Fprint(w, "data: "+event+"\n\n")
					}
				})
				send := func(history string) []byte {
					t.Helper()
					body := fmt.Sprintf(`{"model":"native-model","messages":[{"role":"user","content":"Read main.go"}%s],"max_tokens":64,"stream":%t,"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],"tool_choice":{"type":"tool","name":"read_file","disable_parallel_tool_use":true}}`, history, stream)
					request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/messages", strings.NewReader(body))
					request.Header.Set("Authorization", "Bearer "+fullKey)
					request.Header.Set("X-Claude-Code-Session-Id", "tools")
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					reply, err := io.ReadAll(response.Body)
					if err != nil || response.StatusCode != http.StatusOK {
						t.Fatalf("status %d body %s error %v", response.StatusCode, reply, err)
					}
					return reply
				}
				first := send("")
				if stream {
					if _, err := protocols.Stream(wire.Anthropic, wire.Anthropic, bytes.NewReader(first), 8192, "native-model", true, func([]byte) error { return nil }); err != nil {
						t.Fatal(err)
					}
					for _, field := range []string{`"id":"call-read"`, `"name":"read_file"`, `"stop_reason":"tool_use"`, `"output_tokens":4`} {
						if !bytes.Contains(first, []byte(field)) {
							t.Fatalf("missing %s in %s", field, first)
						}
					}
					capture := protocols.NewCaptureStream(wire.Anthropic, 8192, false, true)
					if err := capture.Push(first); err != nil {
						t.Fatal(err)
					}
					_, calls, err := capture.Finish()
					if err != nil || len(calls) != 1 {
						t.Fatalf("streamed tool calls %s %v", first, err)
					}
					var call struct {
						Name      string
						Arguments struct{ Path string }
					}
					if err := json.Unmarshal(calls[0], &call); err != nil || call.Name != "read_file" || call.Arguments.Path != "main.go" {
						t.Fatalf("streamed tool arguments %s %v", first, err)
					}
					if source == "fragments" && (!bytes.Contains(first, []byte(`"partial_json":"{\"path\":"`)) || !bytes.Contains(first, []byte(`"partial_json":"\"main.go\"}"`))) {
						t.Fatalf("fragmented arguments lost %s", first)
					}
				} else {
					completion, err := protocols.Decode(wire.Anthropic, wire.Anthropic, first, "native-model", "")
					if err != nil || len(completion.ToolCalls) != 1 || completion.ToolCalls[0].ID != "call-read" || completion.ToolCalls[0].Name != "read_file" || completion.ToolCalls[0].Arguments != `{"path":"main.go"}` || completion.FinishReason != "tool_calls" {
						t.Fatalf("tool reply %s error %v", first, err)
					}
				}
				second := send(`,{"role":"assistant","content":[{"type":"tool_use","id":"call-read","name":"read_file","input":{"path":"main.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-read","is_error":false,"content":[{"type":"text","text":"package main\n"}]}]}`)
				if !bytes.Contains(second, []byte(`"text":"File read"`)) || !bytes.Contains(second, []byte(`"stop_reason":"end_turn"`)) {
					t.Fatalf("tool result reply %s", second)
				}
				for _, total := range []int64{11, 15} {
					if usage := ledger.wait(t); usage.Total == nil || *usage.Total != total {
						t.Fatalf("usage %+v, expected %d", usage, total)
					}
				}
				if g.mock.count("a") != 2 {
					t.Fatal("tool exchange replayed work")
				}
			})
		}
	}
}

func TestCodeMessagesToCodexReasoningControls(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high"} {
		t.Run(effort, func(t *testing.T) {
			g, ledger, server := newCodeForwardHarness(t)
			route := g.rt.release.Snapshot.CodeRoutes["coding"]
			route.MessagesToCodex = true
			g.rt.release.Snapshot.CodeRoutes["coding"] = route
			g.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				var body struct{ Reasoning struct{ Effort string } }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Reasoning.Effort != effort {
					t.Errorf("reasoning control %+v error %v", body, err)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"reasoning-control","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done"}]}],"usage":{"input_tokens":2,"output_tokens":4,"total_tokens":6,"output_tokens_details":{"reasoning_tokens":3}}}`)
			})
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"native-model","messages":[{"role":"user","content":"Think"}],"max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":%q}}`, effort)))
			request.Header.Set("Authorization", "Bearer "+fullKey)
			request.Header.Set("X-Claude-Code-Session-Id", "reasoning-control")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"output_tokens":4`)) {
				t.Fatalf("reasoning reply %d %s", response.StatusCode, body)
			}
			if usage := ledger.wait(t); usage.Reasoning == nil || *usage.Reasoning != 3 || usage.Total == nil || *usage.Total != 6 {
				t.Fatalf("reasoning usage %+v", usage)
			}
		})
	}
}

func TestCodeMessagesToCodexToolStreamOutcomes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, outcome := range []string{"truncated", "failed", "missing-usage", "max-tokens", "refusal", "mismatched-snapshot", "opaque-reasoning"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, outcome), func(t *testing.T) {
				g, ledger, server := newCodeForwardHarness(t)
				route := g.rt.release.Snapshot.CodeRoutes["coding"]
				route.MessagesToCodex = true
				g.rt.release.Snapshot.CodeRoutes["coding"] = route
				g.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, event := range []string{
						`{"type":"response.created","response":{"id":"tool-outcome","status":"in_progress","output":[]}}`,
						`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":""}}`,
						`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
					} {
						fmt.Fprint(w, "data: "+event+"\n\n")
					}
					switch outcome {
					case "truncated":
						return
					case "failed":
						fmt.Fprint(w, "data: "+`{"type":"response.failed","response":{"id":"tool-outcome","status":"failed","error":{"code":"server_error","message":"failed"},"output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`+"\n\n")
					case "missing-usage":
						fmt.Fprint(w, "data: "+`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{}"}}`+"\n\n")
						fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"tool-outcome","status":"completed","output":[]}}`+"\n\n")
					case "max-tokens", "refusal":
						reason := func() string {
							if outcome == "refusal" {
								return "content_filter"
							}
							return "max_output_tokens"
						}()
						fmt.Fprintf(w, "data: "+`{"type":"response.incomplete","response":{"id":"tool-outcome","status":"incomplete","incomplete_details":{"reason":%q},"output":[{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{}"}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`+"\n\n", reason)
					case "mismatched-snapshot":
						fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"tool-outcome","status":"completed","output":[{"type":"function_call","call_id":"different-call","name":"read_file","arguments":"{}"}]}}`+"\n\n")
					case "opaque-reasoning":
						fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"tool-outcome","status":"completed","output":[{"type":"reasoning","id":"reasoning-state","encrypted_content":"opaque"}]}}`+"\n\n")
					}
				})
				request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"native-model","messages":[{"role":"user","content":"Read a file"}],"max_tokens":64,"stream":%t}`, stream)))
				request.Header.Set("Authorization", "Bearer "+fullKey)
				request.Header.Set("X-Claude-Code-Session-Id", "tool-outcome")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				successful := outcome == "missing-usage" || outcome == "max-tokens" || outcome == "refusal"
				if successful {
					reason := func() string {
						switch outcome {
						case "max-tokens":
							return "max_tokens"
						case "refusal":
							return "refusal"
						default:
							return "tool_use"
						}
					}()
					if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"stop_reason":"`+reason+`"`)) {
						t.Fatalf("lost terminal reason %d %s", response.StatusCode, body)
					}
				} else if stream {
					if !bytes.Contains(body, []byte("event: error")) || bytes.Contains(body, []byte("event: message_stop")) {
						t.Fatalf("fabricated stream success %s", body)
					}
				} else if response.StatusCode != http.StatusBadGateway {
					t.Fatalf("fabricated unary success %d %s", response.StatusCode, body)
				}
				usage := ledger.wait(t)
				if outcome == "failed" || outcome == "max-tokens" || outcome == "refusal" {
					if usage.Total == nil || *usage.Total != 7 {
						t.Fatalf("authoritative terminal usage lost %+v", usage)
					}
				} else if usage.Total != nil || usage.Input != nil || usage.Output != nil {
					t.Fatalf("invented usage %+v", usage)
				}
				if g.mock.count("a") != 1 {
					t.Fatal("partial exchange replayed")
				}
			})
		}
	}
}

func TestCodeMessagesToCodexCancelledToolStream(t *testing.T) {
	g, ledger, server := newCodeForwardHarness(t)
	route := g.rt.release.Snapshot.CodeRoutes["coding"]
	route.MessagesToCodex = true
	g.rt.release.Snapshot.CodeRoutes["coding"] = route
	g.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"cancelled-tool","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`,
		} {
			fmt.Fprint(w, "data: "+event+"\n\n")
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/code/coding/v1/messages", strings.NewReader(`{"model":"native-model","messages":[{"role":"user","content":"Read a file"}],"max_tokens":64,"stream":true}`))
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("X-Claude-Code-Session-Id", "cancelled-tool")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "input_json_delta") {
			break
		}
	}
	cancel()
	remaining, _ := io.ReadAll(reader)
	if bytes.Contains(remaining, []byte("message_stop")) {
		t.Fatalf("cancelled tool reported success %s", remaining)
	}
	usage := ledger.wait(t)
	if usage.Total != nil || usage.Input != nil || usage.Output != nil {
		t.Fatalf("cancelled tool invented usage %+v", usage)
	}
	if g.mock.count("a") != 1 {
		t.Fatal("cancelled tool was replayed")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.outcomes) == 0 || (ledger.outcomes[len(ledger.outcomes)-1].Origin != "client" || ledger.outcomes[len(ledger.outcomes)-1].Kind != "canceled") {
		t.Fatalf("cancellation outcomes=%+v", ledger.outcomes)
	}
}

func TestCodeForwardingObservesRetryAfterWithoutChangingAllowance(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		status  int
		headers []string
		delay   *time.Duration
	}{
		{"seconds", 429, []string{"120"}, new(2 * time.Minute)},
		{"date", 503, []string{now.Add(3 * time.Minute).Format(http.TimeFormat)}, new(3 * time.Minute)},
		{"zero", 429, []string{"0"}, new(time.Duration(0))},
		{"past", 503, []string{now.Add(-time.Minute).Format(http.TimeFormat)}, new(time.Duration(0))},
		{"malformed", 429, []string{"soon"}, nil},
		{"negative", 503, []string{"-1"}, nil},
		{"missing", 429, nil, nil},
		{"ambiguous", 503, []string{"30", "120"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			h.gateway.now = func() time.Time { return now }
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				for _, value := range test.headers {
					w.Header().Add("Retry-After", value)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"error":"controlled rejection"}`)
			})
			response := codeDo(t, server, []byte(`{"model":"native-model","input":[]}`), nil)
			_, _ = io.Copy(io.Discard, response.Body)
			ledger.wait(t)
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if response.StatusCode != test.status || ledger.health.Status == "" {
				t.Fatalf("status %d observations %+v", response.StatusCode, ledger.health)
			}
			observed := ledger.health
			if !observed.ObservedAt.Equal(now) {
				t.Fatalf("observation time %s", observed.ObservedAt)
			}
			if test.delay == nil && observed.RetryAt != nil || test.delay != nil && (observed.RetryAt == nil || !observed.RetryAt.Equal(now.Add(*test.delay))) {
				t.Fatalf("retry at %v", observed.RetryAt)
			}
			if test.status == 429 && observed.Status != "quota_limited" || test.status == 503 && observed.Status != "unavailable" {
				t.Fatalf("health %s", observed.Status)
			}
			if ledger.allowance.RemainingTokens != nil || ledger.allowance.RemainingRequests != nil ||
				ledger.allowance.RemainingPercent != nil || len(ledger.allowance.Windows) != 0 || ledger.allowance.Credits != nil {
				t.Fatal("rejection invented allowance")
			}
			if h.mock.count("a") != 1 {
				t.Fatal("rejection replayed generation")
			}
		})
	}
}

func TestCodeForwardingRetainsExhaustionWithMalformedOrMissingReset(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, signal string
		reset        *time.Time
	}{
		{"future", fmt.Sprint(now.Add(time.Hour).Unix()), new(now.Add(time.Hour))},
		{"expired", fmt.Sprint(now.Add(-time.Hour).Unix()), new(now.Add(-time.Hour))},
		{"malformed", "soon", nil},
		{"missing", "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			h.gateway.now = func() time.Time { return now }
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Codex-Primary-Used-Percent", "100")
				if test.signal != "" {
					w.Header().Set("X-Codex-Primary-Reset-At", test.signal)
				}
				w.WriteHeader(429)
			})
			response := codeDo(t, server, []byte(`{"model":"native-model","input":[]}`), nil)
			_, _ = io.Copy(io.Discard, response.Body)
			ledger.wait(t)
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if response.StatusCode != 429 || ledger.allowance.RemainingPercent == nil {
				t.Fatalf("exhaustion not observed, status %d allowance %+v", response.StatusCode, ledger.allowance)
			}
			allowance := ledger.allowance
			if allowance.RemainingPercent == nil || *allowance.RemainingPercent != 0 || !allowance.ObservedAt.Equal(now) {
				t.Fatalf("exhaustion or observation time changed %+v", allowance)
			}
			if test.reset == nil && allowance.ResetsAt != nil || test.reset != nil && (allowance.ResetsAt == nil || !allowance.ResetsAt.Equal(*test.reset)) {
				t.Fatalf("reset signal inferred or lost %+v", allowance)
			}
		})
	}
}

func TestCodeContinuationRefusalsExplainRecovery(t *testing.T) {
	for _, reason := range []string{"code_parent_unresolved", "code_response_account_conflict"} {
		t.Run(reason, func(t *testing.T) {
			_, ledger, server := newCodeForwardHarness(t)
			ledger.refuse = reason
			response := codeDo(t, server, []byte(`{"model":"native-model","input":[]}`), nil)
			defer response.Body.Close()
			var body struct {
				Error struct{ Code, Message string } `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusConflict || body.Error.Code != reason || !strings.Contains(body.Error.Message, "full context") || !strings.Contains(body.Error.Message, "response reference") {
				t.Fatalf("continuation recovery status %d body %+v", response.StatusCode, body)
			}
		})
	}
}

func TestCodeExactAliasHTTPUsesNativeTargetAndPublicResponse(t *testing.T) {
	for _, test := range []struct {
		name                       string
		stream, translated, pinned bool
	}{
		{"unary", false, false, false}, {"stream", true, false, false}, {"translated", true, true, false}, {"republished-alias-retains-pin", false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.ModelRewriting = true
			route.Aliases = map[string]string{"team-code": "native-model"}
			if test.pinned {
				route.Models = []string{"native-model", "native-next"}
				route.Aliases = map[string]string{"team-code": "native-next"}
				ledger.pinTarget = "native-model"
			}
			route.MessagesToCodex = test.translated
			h.rt.release.Snapshot.CodeRoutes["coding"] = route
			h.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if !bytes.Contains(body, []byte(`"model":"native-model"`)) || !bytes.Contains(body, []byte(`"private-prompt"`)) {
					t.Errorf("upstream received wrong model or prompt %s", body)
				}
				if !test.translated && !bytes.Contains(body, []byte(`{ "model":"team-code", "number":1.2300 }`)) {
					t.Errorf("alias changed unknown request subtree %s", body)
				}
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: response.completed\nid: event-one\ndata: "+`{"type":"response.completed","response":{"id":"alias-response","model":"native-model","status":"completed","output":[{"id":"text-one","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"alias-response","model":"native-model","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3},"extension":{"model":"native-model"}}`)
			})
			body := []byte(`{"model":"team-code","input":"private-prompt","extension": { "model":"team-code", "number":1.2300 }}`)
			response := func() *http.Response {
				if !test.translated {
					return codeDo(t, server, body, nil)
				}
				request, _ := http.NewRequest(http.MethodPost, server.URL+"/code/coding/v1/messages", strings.NewReader(`{"model":"team-code","messages":[{"role":"user","content":"private-prompt"}],"max_tokens":32,"stream":true}`))
				request.Header.Set("Authorization", "Bearer "+fullKey)
				request.Header.Set("X-Claude-Code-Session-Id", "alias-text")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				return response
			}()
			defer response.Body.Close()
			output, _ := io.ReadAll(response.Body)
			if response.StatusCode != 200 || !bytes.Contains(output, []byte(`"model":"team-code"`)) {
				t.Fatalf("client alias response %d %s", response.StatusCode, output)
			}
			usage := ledger.wait(t)
			if usage.Total == nil || *usage.Total != 3 || len(ledger.inputs) != 1 || ledger.inputs[0].Operation.Model != route.Aliases["team-code"] || ledger.inputs[0].Operation.PublicModel() != "team-code" {
				t.Fatalf("alias accounting %+v inputs %+v", usage, ledger.inputs)
			}
		})
	}
}

func TestCodeExactAliasWebSocketRewritesOnlyModelFields(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.ModelRewriting = true
	route.Aliases = map[string]string{"team-code": "native-model"}
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	h.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		kind, request, err := conn.Read(t.Context())
		if err != nil || kind != websocket.MessageText || !bytes.Contains(request, []byte(`"model":"native-model"`)) || !bytes.Contains(request, []byte(`{ "model":"team-code", "number":1.2300 }`)) {
			t.Errorf("native alias dispatch %s %v", request, err)
			return
		}
		_ = conn.Write(t.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"alias-ws","model":"native-model","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3},"extension":{"model":"native-model"}}}`))
		_, _, _ = conn.Read(t.Context())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, strings.Replace(server.URL, "http://", "ws://", 1)+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"alias-ws"}, "X-OLP-Code-Model": {"team-code"}}})
	if err != nil {
		t.Fatalf("alias WebSocket handshake %v %v", response, err)
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"team-code","input":"private-prompt","extension": { "model":"team-code", "number":1.2300 }}`)); err != nil {
		t.Fatal(err)
	}
	_, body, err := conn.Read(ctx)
	if err != nil || !bytes.Contains(body, []byte(`"model":"team-code"`)) || !bytes.Contains(body, []byte(`"extension":{"model":"native-model"}`)) {
		t.Fatalf("alias WebSocket response %s %v", body, err)
	}
	select {
	case usage := <-ledger.done:
		if usage.Total == nil || *usage.Total != 3 {
			t.Fatalf("alias usage %+v", usage)
		}
	case <-ctx.Done():
		t.Fatal("alias usage missing")
	}
}

func TestCodeExactAliasesDiscoverOnlyAvailableNativeTargets(t *testing.T) {
	h, _, server := newCodeForwardHarness(t, codeForwardSetup{
		Models: []string{"native-model", "unavailable-model"},
		Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}},
		Offers: codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: []string{"native-model"}}},
	})
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.ModelRewriting = true
	route.Aliases = map[string]string{"team-code": "native-model", "unavailable-alias": "unavailable-model"}
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	for _, path := range []string{"models", "v1/models"} {
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+route.BasePath()+"/"+path, nil)
		request.Header.Set("Authorization", "Bearer "+fullKey)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var catalog struct {
			Data []codemode.Model `json:"data"`
		}
		err = json.NewDecoder(response.Body).Decode(&catalog)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || len(catalog.Data) != 2 || catalog.Data[0].ID != "native-model" || catalog.Data[1].ID != "team-code" || !reflect.DeepEqual(catalog.Data[0].Capabilities, catalog.Data[1].Capabilities) {
			t.Fatalf("alias discovery %+v %v", catalog, err)
		}
	}
}

func TestCodeExactAliasRefusesUnauthorizedOrUnavailableTargets(t *testing.T) {
	for _, test := range []struct {
		name, requested, target, refusal string
		rewriting                        bool
	}{
		{"disabled", "team-code", "native-model", "code_model_denied", false},
		{"unknown", "unknown-alias", "native-model", "code_model_denied", true},
		{"unpublished", "team-code", "unpublished-model", "code_model_denied", true},
		{"unavailable", "team-code", "native-model", "code_account_unavailable", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.Aliases = map[string]string{"team-code": test.target}
			route.ModelRewriting = test.rewriting
			h.rt.release.Snapshot.CodeRoutes["coding"] = route
			if test.name == "unavailable" {
				ledger.refuse = test.refusal
			}
			response := codeDo(t, server, []byte(`{"model":"`+test.requested+`","input":"hello"}`), nil)
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode < 400 || !bytes.Contains(body, []byte(test.refusal)) || len(ledger.marks) != 0 || h.mock.count("a") != 0 {
				t.Fatalf("invalid alias dispatched, status %d, body %s", response.StatusCode, body)
			}
		})
	}
}

func TestCodeExactAliasRecoveryKeepsSelectedNativeModel(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.InitialRecoveryAttempts = 1
	route.ModelRewriting = true
	route.Aliases = map[string]string{"team-code": "native-model"}
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	h.gateway.CodeAuthorizer = &codeRecoveryAuthorizer{}
	ledger.pinTarget = "native-model"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Model, Input string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "native-model" || request.Input != "hello" {
			t.Errorf("alias recovery changed exact target or prompt %+v, %v", request, err)
		}
		if h.mock.count("a") == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"type":"authentication_error","code":"invalid_api_key"}}`)
			return
		}
		io.WriteString(w, `{"id":"recovered","model":"native-model","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	})
	response := codeDo(t, server, []byte(`{"model":"team-code","input":"hello"}`), nil)
	defer response.Body.Close()
	var completion struct{ ID, Model string }
	if err := json.NewDecoder(response.Body).Decode(&completion); err != nil || response.StatusCode != http.StatusOK || completion.ID != "recovered" || completion.Model != "team-code" || h.mock.count("a") != 2 {
		t.Fatalf("alias recovery status=%d completion=%+v err=%v requests=%d", response.StatusCode, completion, err, h.mock.count("a"))
	}
	ledger.wait(t)
	ledger.wait(t)
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.inputs) != 2 || ledger.inputs[1].Recovery == nil {
		t.Fatalf("alias recovery did not use durable recovery admission %+v", ledger.inputs)
	}
	for _, admission := range ledger.inputs {
		if !admission.InitialRecovery || admission.Operation.Model != "native-model" || admission.Operation.PublicModel() != "team-code" {
			t.Fatalf("alias recovery lost requested or exact model %+v", admission)
		}
	}
}

func TestCodeExactAliasFiltersNativeProtocolBeforeAdmission(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t, codeForwardSetup{Models: []string{"gpt-fixture-model"}, Policy: access.KeyPolicy{Scopes: []string{"inference"}}})
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.MessagesToCodex = true
	route.ModelRewriting = true
	route.Aliases = map[string]string{"team-code": "gpt-fixture-model"}
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	h.rt.release.Snapshot.CodeConnections["revision:other"] = routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codeplans.OpenCodeGoProfile, Endpoint: h.upstream.URL + "/b"}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"eligible","model":"gpt-fixture-model","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
	})
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+route.BasePath()+"/v1/messages", strings.NewReader(`{"model":"team-code","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`))
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("X-Claude-Code-Session-Id", "mixed-alias")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"model":"team-code"`)) {
		t.Fatalf("eligible alias failed %d %s", response.StatusCode, body)
	}
	ledger.wait(t)
	if len(ledger.inputs) != 1 || !reflect.DeepEqual(ledger.inputs[0].Providers, []string{"provider"}) {
		t.Fatalf("protocol-ineligible accounts reached durable admission %+v", ledger.inputs)
	}
}

func TestCodeExactAliasPreservesLiteralJSONAndUnrelatedEnvelopes(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.ModelRewriting = true
	route.Aliases = map[string]string{"team-code": "native-model"}
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	requestBody := []byte("{\n  \"mo\\u0064el\" : \"team-code\", \"input\" : \"hello\", \"\\u0065xtension\":{\"model\":\"team-code\",\"number\":1.2300}\n}")
	upstreamRequest := []byte("{\n  \"mo\\u0064el\" : \"native-model\", \"input\" : \"hello\", \"\\u0065xtension\":{\"model\":\"team-code\",\"number\":1.2300}\n}")
	upstreamResponse := []byte("{\n \"model\" : \"native-model\", \"id\":\"literal\", \"status\":\"completed\", \"response\": { \"model\":\"native-model\" }, \"message\": { \"model\":\"native-model\" }, \"\\u0065xtension\":1.2300, \"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}\n}")
	clientResponse := []byte("{\n \"model\" : \"team-code\", \"id\":\"literal\", \"status\":\"completed\", \"response\": { \"model\":\"native-model\" }, \"message\": { \"model\":\"native-model\" }, \"\\u0065xtension\":1.2300, \"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}\n}")
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, upstreamRequest) {
			t.Errorf("untouched request bytes changed %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstreamResponse)
	})
	response := codeDo(t, server, requestBody, nil)
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, clientResponse) {
		t.Fatalf("untouched response bytes changed %d %q", response.StatusCode, body)
	}
	if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
		t.Fatalf("native usage lost %+v", usage)
	}
}

func TestCodeExactAliasPreservesRawSSEFraming(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.ModelRewriting = true
	route.Aliases = map[string]string{"team-code": "native-model"}
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	wire := "\xef\xbb\xbf: before\r\nid: event-one\r\nretry: 0010\r\nx-vendor: untouched\r\nevent: response.completed\r\ndata: { \"type\":\"response.completed\",\r\n: interleaved\r\ndata:\t\"response\": { \"id\":\"raw-stream\", \"model\" : \"native-model\", \"status\":\"completed\", \"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}, \"message\":{\"model\":\"native-model\"}, \"extension\":1.2300 }\r\n\r\n: final\n"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	})
	response := codeDo(t, server, []byte(`{"model":"team-code","input":"hello","stream":true}`), nil)
	body, err := io.ReadAll(response.Body)
	want := strings.Replace(wire, `"model" : "native-model"`, `"model" : "team-code"`, 1)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("SSE framing changed %d %q, %v", response.StatusCode, body, err)
	}
	if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
		t.Fatalf("raw SSE usage lost %+v", usage)
	}
}

func TestCodeExactAliasFinalizesUsageAtCleanEOF(t *testing.T) {
	for _, test := range []struct{ name, path, request, stream string }{
		{"messages", "v1/messages", `{"model":"team-code","messages":[{"role":"user","content":"hello"}],"max_tokens":8,"stream":true}`, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"native-model\",\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"},
		{"chat", "v1/chat/completions", `{"model":"team-code","messages":[{"role":"user","content":"hello"}],"max_tokens":8,"stream":true}`, "data: {\"model\":\"native-model\",\"choices\":[{\"delta\":{\"content\":\"reply\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"model\":\"native-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.ModelRewriting = true
			route.Aliases = map[string]string{"team-code": "native-model"}
			h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
			h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codeplans.ZAIProfile, Endpoint: h.upstream.URL + "/a"}}
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.stream)
			})
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/"+test.path, strings.NewReader(test.request))
			request.Header.Set("Authorization", "Bearer "+fullKey)
			if test.name == "messages" {
				request.Header.Set("X-Claude-Code-Session-Id", "alias-eof")
			} else {
				request.Header.Set("X-Opencode-Session-Id", "alias-eof")
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"model":"team-code"`) {
				t.Fatalf("alias EOF response %d %s, %v", response.StatusCode, body, err)
			}
			if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
				t.Fatalf("EOF usage not finalized %+v", usage)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if !slices.ContainsFunc(ledger.outcomes, func(outcome codemode.Outcome) bool { return outcome.Kind == "completed" }) {
				t.Fatalf("clean EOF outcome %+v", ledger.outcomes)
			}
		})
	}
}

func TestCodeExactAliasBoundsDeliveryAndRetainsObservedUsage(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, failure := range []string{"write", "flush"} {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, failure), func(t *testing.T) {
				h, ledger, _ := newCodeForwardHarness(t)
				route := h.rt.release.Snapshot.CodeRoutes["coding"]
				route.ModelRewriting = true
				route.Aliases = map[string]string{"team-code": "native-model"}
				h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"delivery\",\"model\":\"native-model\",\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"delivery","model":"native-model","status":"completed","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
				})
				mux := http.NewServeMux()
				h.gateway.RegisterCode(mux)
				request := httptest.NewRequest(http.MethodPost, "/code/coding/responses", strings.NewReader(`{"model":"team-code","input":"hello"}`))
				request.Header.Set("Authorization", "Bearer "+fullKey)
				request.Header.Set("Session_id", "alias-delivery")
				writer := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder()}
				if failure == "write" {
					writer.writeErr = errors.New("fixture write failed")
				} else {
					writer.flushErr = errors.New("fixture flush failed")
				}
				writer.beforeDelivery = func() {
					if remaining := time.Until(writer.deadline); remaining <= 0 || remaining > responseWriteTimeout {
						t.Fatalf("unbounded alias delivery %s", remaining)
					}
				}
				mux.ServeHTTP(writer, request)
				if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
					t.Fatalf("terminal usage lost on %s failure %+v", failure, usage)
				}
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				if !slices.ContainsFunc(ledger.outcomes, func(outcome codemode.Outcome) bool { return outcome.Origin == "client" && outcome.Kind == "canceled" }) {
					t.Fatalf("delivery failure outcome %+v", ledger.outcomes)
				}
			})
		}
	}
}

func TestCodeExactAliasSlowReaderTerminatesWithinWriteDeadline(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			h, ledger, _ := newCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.ModelRewriting = true
			route.Aliases = map[string]string{"team-code": "native-model"}
			h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"type":"response.completed","response":`)
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = fmt.Fprintf(w, `{"id":"slow-reader","model":"native-model","status":"completed","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3},"extension":"%s"}`, strings.Repeat("x", 768<<10))
				if streaming {
					_, _ = io.WriteString(w, "}\n\n")
				}
			})
			mux := http.NewServeMux()
			h.gateway.RegisterCode(mux)
			finished := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				mux.ServeHTTP(codeWriteDeadlineWriter{socketDeadlineWriter{ResponseWriter: w, t: t}}, r)
			}))
			server.Config.ConnState = func(connection net.Conn, state http.ConnState) {
				if state == http.StateNew {
					if tcp, ok := connection.(*net.TCPConn); ok {
						_ = tcp.SetWriteBuffer(1024)
					}
				}
			}
			server.Start()
			defer server.Close()
			connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			body := `{"model":"team-code","input":"hello"}`
			_, err = fmt.Fprintf(connection, "POST /code/coding/responses HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nSession_id: slow-alias\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", server.Listener.Addr().String(), fullKey, len(body), body)
			if err != nil {
				t.Fatal(err)
			}
			_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
			reader := bufio.NewReader(connection)
			status, readErr := reader.ReadString('\n')
			if readErr != nil || !strings.Contains(status, "200") {
				t.Fatalf("alias slow response %q, %v", status, readErr)
			}
			for {
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					t.Fatal(readErr)
				}
				if line == "\r\n" {
					break
				}
			}
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("alias write did not stop for a nonreading client")
			}
			if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
				t.Fatalf("slow delivery lost native terminal usage %+v", usage)
			}
		})
	}
}

type codeWriteDeadlineWriter struct{ socketDeadlineWriter }

func (writer codeWriteDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	return http.NewResponseController(writer.ResponseWriter).SetReadDeadline(deadline)
}
