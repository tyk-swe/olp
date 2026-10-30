package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/claudeauth"
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
	paidOperation           codemode.Operation
	paidRefuse              string
	refusalRecords          []codeTestRefusal
	mu                      sync.Mutex
	inputs                  []resources.CodeAdmission
	marks                   []string
	aborts                  []string
	usages                  []codemode.Usage
	health                  codemode.HealthObservation
	allowance               codemode.Allowance
	refusals                []string
	refuse                  string
	done                    chan codemode.Usage
	references              map[string]bool
	outcomes                []codemode.Outcome
	offers                  codeclient.Offers
	modelsErr               error
	pinTarget               string
	recoveryTarget          string
	recoveryUnavailable     bool
	subscriptionUnavailable bool
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
	if l.subscriptionUnavailable || in.Recovery != nil && l.recoveryUnavailable {
		return resources.CodePermit{}, codemode.Refuse(503, "code_account_unavailable")
	}
	if in.PreviousResponse != "" && !l.references[in.PreviousResponse] {
		return resources.CodePermit{}, codemode.Refuse(409, "code_parent_unresolved")
	}
	if l.refuse != "" {
		return resources.CodePermit{}, codemode.Refuse(409, l.refuse)
	}
	target := func() string {
		if in.Recovery != nil && l.recoveryTarget != "" {
			return l.recoveryTarget
		}
		return l.pinTarget
	}()
	providers := func() []string {
		if target != "" && target != in.Operation.Model {
			if in.ModelProviders != nil {
				return in.ModelProviders[target]
			}
			if in.ModelSource != nil {
				return in.ModelSource.CodeModelProviders(in.Route, in.ModelPath, target, in.ModelTransport)
			}
		}
		return in.Providers
	}()
	permit, err := codeTestPermit(in.APIKeyID, providers)
	permit.Authority.EndUserDigest = in.EndUserDigest
	permit.InitialRecovery = in.InitialRecovery && in.Operation.Identity.Parent == "" && in.PreviousResponse == ""
	permit.Attempt = codemode.Attempt{ID: fmt.Sprint(len(l.inputs))}
	permit.Pin.TargetModel = target
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
	if dispatch.Adapter == codemode.AdapterClaude {
		headers.Set("Anthropic-Beta", claudeauth.OAuthBeta)
	}
	if endpoint.Observation == codeadapter.ObserveCodex {
		headers.Set("Chatgpt-Account-Id", "upstream-account")
	}
	return codemode.Authorization{Principal: a.principal, Headers: headers}, nil
}

type codeForwardSetup struct {
	Models                  []string
	Policy                  access.KeyPolicy
	Offers                  codeclient.Offers
	ModelsError             error
	InitialRecoveryAttempts int
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
	route := codemode.Route{ID: "route", Slug: "coding", ProjectID: "project", PoolID: "pool", RevisionID: "revision", Revision: 1, PublishedAt: &publishedAt, Enabled: true, Models: setup.Models, InitialRecoveryAttempts: setup.InitialRecoveryAttempts}
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

func TestCodeCodexIdentityPreservesNativeExchange(t *testing.T) {
	for _, test := range []struct {
		name, profile, path, model string
		enabled                    bool
		originators                []string
		want                       []string
	}{
		{"disabled", codexauth.ProfileID, "responses", "native-model", false, nil, nil},
		{"enabled", codexauth.ProfileID, "responses", "native-model", true, nil, []string{"codex_cli_rs"}},
		{"blank", codexauth.ProfileID, "responses", "native-model", true, []string{""}, []string{"codex_cli_rs"}},
		{"mixed-blank-caller", codexauth.ProfileID, "responses", "native-model", true, []string{"", "caller-extension"}, []string{"", "caller-extension"}},
		{"caller", codexauth.ProfileID, "responses", "native-model", true, []string{"caller-client", "caller-extension"}, []string{"caller-client", "caller-extension"}},
		{"other-adapter", codeplans.OpenCodeGoProfile, "v1/responses", "gpt-fixture", true, nil, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t, codeForwardSetup{Models: []string{test.model}, Policy: access.KeyPolicy{Scopes: []string{"inference"}}})
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.CodexCLIIdentity, route.MessagesToCodex = test.enabled, true
			h.rt.release.Snapshot.CodeRoutes["coding"] = route
			h.rt.release.Snapshot.CodeConnections["revision:provider"] = routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: test.profile, Endpoint: h.upstream.URL + "/a"}
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			requestBody := []byte(`{ "model":"` + test.model + `", "instructions":"Preserve all instructions", "input":[{"type":"function_call","call_id":"call-1","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"source"}], "reasoning":{"effort":"high"}, "extension":{"unknown":true} }`)
			responseBody := []byte(`{ "id":"identity-response", "status":"completed", "output":[], "usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}, "extension":{"unknown":true} }`)
			h.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, requestBody) || !slices.Equal(r.Header.Values("Originator"), test.want) || r.Header.Get("User-Agent") != "caller-agent" || r.Header.Get("X-Caller-Extension") != "preserve" || r.Header.Get("Authorization") != "Bearer upstream-only" {
					t.Errorf("identity policy changed native content or headers, body %s originator %v error %v", body, r.Header.Values("Originator"), err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(responseBody)
			})
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/"+test.path, bytes.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			request.Header = http.Header{"Authorization": {"Bearer " + fullKey}, "X-Olp-Code-Affinity": {"identity-session"}, "Originator": test.originators, "User-Agent": {"caller-agent"}, "X-Caller-Extension": {"preserve"}}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, responseBody) {
				t.Fatalf("native response changed, status %d body %s error %v", response.StatusCode, body, err)
			}
			usage := ledger.wait(t)
			if usage.Total == nil || *usage.Total != 3 {
				t.Fatalf("usage lost %+v", usage)
			}
		})
	}
}

func TestCodeCodexIdentityPreservesWebSocketExchange(t *testing.T) {
	for _, test := range []struct {
		name, originator, want string
		enabled                bool
	}{
		{"disabled", "", "", false},
		{"enabled", "", "codex_cli_rs", true},
		{"caller", "caller-client", "caller-client", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.CodexCLIIdentity = test.enabled
			h.rt.release.Snapshot.CodeRoutes["coding"] = route
			requestBody := []byte(`{ "type":"response.create", "model":"native-model", "instructions":"Keep instructions", "input":"private-prompt", "reasoning":{"effort":"high"}, "extension":{"number":1.2300} }`)
			responseBody := []byte(`{ "type":"response.completed", "response":{"id":"identity-ws","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3},"extension":{"number":1.2300}} }`)
			h.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Originator") != test.want || r.Header.Get("User-Agent") != "caller-agent" || r.Header.Get("Authorization") != "Bearer upstream-only" {
					t.Error("WebSocket identity policy changed explicit caller or authorization headers")
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				kind, body, err := conn.Read(t.Context())
				if err != nil || kind != websocket.MessageText || !bytes.Equal(body, requestBody) {
					t.Errorf("WebSocket request changed, body %s error %v", body, err)
					return
				}
				_ = conn.Write(t.Context(), websocket.MessageText, responseBody)
				_, _, _ = conn.Read(t.Context())
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"identity-ws"}, "Originator": {test.originator}, "User-Agent": {"caller-agent"}}})
			if err != nil {
				t.Fatalf("identity WebSocket handshake %v %v", response, err)
			}
			defer conn.CloseNow()
			if err := conn.Write(ctx, websocket.MessageText, requestBody); err != nil {
				t.Fatal(err)
			}
			_, body, err := conn.Read(ctx)
			if err != nil || !bytes.Equal(body, responseBody) {
				t.Fatalf("WebSocket response changed, body %s error %v", body, err)
			}
			usage := ledger.wait(t)
			if usage.Total == nil || *usage.Total != 3 {
				t.Fatalf("WebSocket usage lost %+v", usage)
			}
		})
	}
}

func TestCodeGenericResponsesPreserveNativeExchange(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	requestBody := []byte(`{ "model":"native-model", "input":"Explain this function", "vendor_extension":{"keep":true} }`)
	responseBody := []byte(`{ "id":"generic-response", "status":"completed", "output":[], "usage":{"total_tokens":3,"input_tokens":2,"output_tokens":1,"input_tokens_details":{"cached_tokens":1}}, "vendor_extension":{"keep":true} }`)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, requestBody) {
			t.Error("generic request bytes changed")
		}
		if r.Header.Get("Authorization") != "Bearer upstream-only" {
			t.Error("account credential missing")
		}
		for _, name := range []string{"Thread-Id", "Session-Id", "Session_id", "X-Claude-Code-Session-Id", "X-Opencode-Session-Id", "X-Olp-Code-Affinity"} {
			if len(r.Header.Values(name)) != 0 {
				t.Errorf("invented or leaked identity header %s", name)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	})
	for range 2 {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", bytes.NewReader(requestBody))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+fullKey)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, responseBody) {
			t.Fatalf("generic exchange failed, status %d body %s error %v", response.StatusCode, body, err)
		}
		if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
			t.Fatalf("generic usage lost %+v", usage)
		}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.inputs) != 2 || ledger.inputs[0].Operation.Identity.Conversation == ledger.inputs[1].Operation.Identity.Conversation {
		t.Fatal("stateless requests acquired shared affinity")
	}
	if h.mock.count("a") != 2 {
		t.Fatal("generic request replayed")
	}
}

func TestCodeGenericAffinityAndOwnedContinuation(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	responseBody := `{"id":"generic-owned","status":"completed","output":[],"usage":{"total_tokens":3,"input_tokens":2,"output_tokens":1}}`
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Olp-Code-Affinity") != "" || r.Header.Get("Thread-Id") != "" || r.Header.Get("Session_id") != "" {
			t.Error("generic affinity escaped into upstream identity")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	})
	for _, turn := range []struct {
		body     string
		affinity string
		status   int
	}{
		{`{"model":"native-model","input":"first"}`, "conversation", http.StatusOK},
		{`{"model":"native-model","input":"continue","previous_response_id":"generic-owned"}`, "conversation", http.StatusOK},
		{`{"model":"native-model","input":"continue","previous_response_id":"generic-owned"}`, "", http.StatusConflict},
		{`{"model":"native-model","input":"continue","previous_response_id":"unknown"}`, "conversation", http.StatusConflict},
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", strings.NewReader(turn.body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+fullKey)
		if turn.affinity != "" {
			request.Header.Set("X-Olp-Code-Affinity", turn.affinity)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != turn.status {
			t.Fatalf("unexpected continuation status %d body %s", response.StatusCode, body)
		}
		if turn.status == http.StatusOK {
			ledger.wait(t)
		}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.inputs[0].Operation.Identity != ledger.inputs[1].Operation.Identity {
		t.Fatal("explicit generic affinity did not survive a continuation")
	}
	if h.mock.count("a") != 2 {
		t.Fatal("unowned or unaffined continuation dispatched")
	}
}

func TestCodeGenericStateRefusesBeforeDispatch(t *testing.T) {
	for _, state := range []struct {
		name, body string
		headers    http.Header
	}{
		{"turn-state", `{"input":"hello"}`, http.Header{"X-Codex-Turn-State": {"opaque"}}},
		{"reasoning", `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, nil},
		{"compaction", `{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, nil},
		{"image-reference", `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"unowned"}]}]}`, nil},
		{"file-reference", `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"unowned"}]}]}`, nil},
		{"server-tool", `{"input":"hello","tools":[{"type":"file_search","vector_store_ids":["unowned"]}]}`, nil},
		{"stored-prompt", `{"input":"hello","prompt":{"id":"pmpt-private"}}`, nil},
		{"omitted-type-reference", `{"input":[{"id":"stored-item"}]}`, nil},
		{"null-type-reference", `{"input":[{"type":null,"id":"stored-item"}]}`, nil},
	} {
		for _, affinity := range []string{"", "generic-state"} {
			t.Run(state.name+"/"+affinity, func(t *testing.T) {
				h, ledger, server := newCodeForwardHarness(t)
				body := []byte(`{"model":"native-model",` + strings.TrimPrefix(state.body, "{"))
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				request.Header = state.headers.Clone()
				if request.Header == nil {
					request.Header = http.Header{}
				}
				request.Header.Set("Authorization", "Bearer "+fullKey)
				if affinity != "" {
					request.Header.Set("X-Olp-Code-Affinity", affinity)
				}
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				got, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusConflict || !bytes.Contains(got, []byte("code_parent_unresolved")) {
					t.Fatalf("unsupported generic state accepted, status %d body %s", response.StatusCode, got)
				}
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				if h.mock.count("a") != 0 || len(ledger.inputs) != 0 || len(ledger.usages) != 0 {
					t.Fatal("unsupported state reached accounting or dispatch")
				}
			})
		}
	}
}

func TestCodeGenericNativeProtocolsPreserveToolHistory(t *testing.T) {
	for _, protocol := range []struct {
		name, path, profile, model, request, response string
	}{
		{"messages", "v1/messages", codeplans.ZAIProfile, "native-model",
			`{"model":"native-model","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"read","input":{"signature":"plain argument"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"source text"}]}],"max_tokens":8,"client_metadata":{"thread_id":"application-thread"},"input":{"type":"native_extension"},"vendor_extension":{"keep":true}}`,
			`{"type":"message","content":[{"type":"text","text":"native reply"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`},
		{"chat", "v1/chat/completions", codeplans.ZAIProfile, "native-model",
			`{"model":"native-model","messages":[{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read","arguments":"{\"signature\":\"plain argument\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"source text"}],"client_metadata":{"thread_id":"application-thread"},"vendor_extension":{"keep":true}}`,
			`{"choices":[{"message":{"role":"assistant","content":"native reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`},
		{"responses", "v1/responses", codeplans.OpenCodeGoProfile, "gpt-fixture",
			`{"model":"gpt-fixture","input":[{"type":"function_call","id":"inline-call","call_id":"call-1","name":"read","arguments":"{\"signature\":\"plain argument\"}"},{"type":"function_call_output","call_id":"call-1","output":"source text"}],"vendor_extension":{"keep":true}}`,
			`{"id":"generic-native","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`},
	} {
		for _, identity := range []struct {
			name    string
			headers http.Header
		}{
			{"generic", http.Header{}},
			{"affinity", http.Header{"X-Olp-Code-Affinity": {"native-history"}}},
			{"recognized", http.Header{"X-Claude-Code-Session-Id": {"native-session"}}},
		} {
			t.Run(protocol.name+"/"+identity.name, func(t *testing.T) {
				h, ledger, server := newCodeForwardHarness(t, codeForwardSetup{Models: []string{protocol.model}, Policy: access.KeyPolicy{Scopes: []string{"inference"}}})
				h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: protocol.profile, Endpoint: h.upstream.URL + "/a"}}
				if err := h.rt.release.Snapshot.Compile(); err != nil {
					t.Fatal(err)
				}
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != protocol.request {
						t.Error("generic native tool history changed")
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, protocol.response)
				})
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/"+protocol.path, strings.NewReader(protocol.request))
				if err != nil {
					t.Fatal(err)
				}
				request.Header = identity.headers.Clone()
				request.Header.Set("Authorization", "Bearer "+fullKey)
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusOK || string(body) != protocol.response {
					t.Fatalf("generic native tool exchange refused, status %d body %s", response.StatusCode, body)
				}
				if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
					t.Fatalf("generic native usage lost %+v", usage)
				}
			})
		}
	}
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
			route.MessagesToCodex, route.ChatToCodex = enabled, enabled
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
			responseBody := []byte(`{ "type":"message", "content":[{"type":"text","text":"native reply"}], "stop_reason":"end_turn", "usage":{"input_tokens":2,"output_tokens":1,"input_tokens_details":{"cached_tokens":1}}, "vendor_extension":{"keep":true} }`)
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
				if err := g.rt.release.Snapshot.Compile(); err != nil {
					t.Fatal(err)
				}
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
					if r.URL.Path != "/a/responses" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer upstream-only" || r.Header.Get("Anthropic-Beta") != "" || r.Header.Get("Originator") != "" || r.Header.Get("X-Claude-Code-Session-Id") != "translated-text" {
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
			if err := g.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
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
			if err := g.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
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
	if err := g.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
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
				route.CodexCLIIdentity = true
				g.rt.release.Snapshot.CodeRoutes["coding"] = route
				if err := g.rt.release.Snapshot.Compile(); err != nil {
					t.Fatal(err)
				}
				g.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Originator") != "codex_cli_rs" {
						t.Error("separate identity policy did not reach translated tool request")
					}
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
			if err := g.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
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
				if err := g.rt.release.Snapshot.Compile(); err != nil {
					t.Fatal(err)
				}
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
	for _, input := range []struct {
		path, header, delta string
		chat                bool
	}{
		{"v1/messages", "X-Claude-Code-Session-Id", "input_json_delta", false},
		{"v1/chat/completions", "X-Opencode-Session-Id", "tool_calls", true},
	} {
		t.Run(input.path, func(t *testing.T) {
			g, ledger, server := newCodeForwardHarness(t)
			route := g.rt.release.Snapshot.CodeRoutes["coding"]
			route.MessagesToCodex, route.ChatToCodex = true, input.chat
			g.rt.release.Snapshot.CodeRoutes["coding"] = route
			if err := g.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
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
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/code/coding/"+input.path, strings.NewReader(`{"model":"native-model","messages":[{"role":"user","content":"Read a file"}],"max_tokens":64,"stream":true}`))
			request.Header.Set("Authorization", "Bearer "+fullKey)
			request.Header.Set(input.header, "cancelled-tool")
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
				if strings.Contains(line, input.delta) {
					break
				}
			}
			cancel()
			remaining, _ := io.ReadAll(reader)
			if bytes.Contains(remaining, []byte("message_stop")) || bytes.Contains(remaining, []byte("[DONE]")) {
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
		})
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
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
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

func (l *codeTestLedger) AdmitPaid(_ context.Context, in resources.PaidCodeAdmission) (resources.PaidCodePermit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paidOperation = in.Operation
	if l.paidRefuse != "" {
		return resources.PaidCodePermit{}, codemode.Refuse(403, l.paidRefuse)
	}
	if l.refuse != "" {
		return resources.PaidCodePermit{}, codemode.Refuse(403, l.refuse)
	}
	return resources.PaidCodePermit{Authority: in.Authority, Attempt: codemode.Attempt{ID: uuid.NewString()}, BindingID: uuid.NewString()}, nil
}

func TestCodePaidTargetPreservesNativeRequestAndAttributesMeteredUsage(t *testing.T) {
	h, ledger, server := newPaidCodeForwardHarness(t)
	route := h.rt.Release().Snapshot.CodeRoutes["coding"]
	provider := h.rt.Release().Snapshot.CodePaidConnections[route.RevisionID+":"+route.PaidTargets[modelA].ProviderID]
	requestBody := []byte(`{ "model":"model-a", "input":"Explain this function", "max_output_tokens":20, "vendor_extension":{"keep":true} }`)
	responseBody := []byte(`{ "id":"paid-response", "status":"completed", "output":[], "usage":{"total_tokens":3,"input_tokens":2,"output_tokens":1,"input_tokens_details":{"cached_tokens":1}}, "vendor_extension":{"keep":true} }`)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "fixture=opaque%2Fquery&fixture=second&future=unknown" {
			t.Errorf("native query changed %q", r.URL.RawQuery)
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, requestBody) || r.Header.Get("Authorization") != "Bearer "+secretA {
			t.Error("paid target changed native request or authorization")
		}
		_, _ = w.Write(responseBody)
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses?fixture=opaque%2Fquery&fixture=second&future=unknown", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("Thread-Id", "paid-native-query")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !bytes.Equal(body, responseBody) {
		t.Fatalf("paid exchange %d %s", response.StatusCode, body)
	}
	ledger.wait(t)
	env := h.sink.last(t)
	if env.KeyID != h.keyID || env.Route != route.Slug || len(env.Attempts) != 1 || env.Attempts[0].ProviderID != provider.ID || env.Attempts[0].VendorID != provider.VendorID || env.Attempts[0].SlotID != h.slotA || env.Attempts[0].Price == nil || !env.Attempts[0].UsageComplete || env.Attempts[0].Usage.InputTokens != 2 || env.Attempts[0].Usage.OutputTokens != 1 {
		t.Fatalf("paid usage attribution %+v", env)
	}
	emitter := usage.NewEmitter(1)
	defer emitter.Close()
	(&AccountingSink{Emitter: emitter, Log: quiet}).Terminal(env)
	if state := emitter.Snapshot(); state.Accepted != 1 || state.Lost() != 0 {
		t.Fatalf("paid accounting was not accepted by the shared pipeline %+v", state)
	}
	emitter.Close()
	emitter.RunWriter(t.Context(), accountingStreamFunc(func(_ context.Context, args ...string) (any, error) {
		event, err := usage.Decode([]byte(args[4]))
		if err != nil {
			t.Fatal(err)
		}
		if len(event.Attempts) != 1 || event.Attempts[0].Usage == nil || !event.Attempts[0].Usage.Complete || event.Attempts[0].Routing == nil || event.Attempts[0].Routing.PricingRevisionID == nil || event.APIKeyID != h.keyID {
			t.Fatalf("paid monetary accounting lost ownership or measured cost %+v", event)
		}
		return "1-0", nil
	}), "metadata", quiet)
}

func TestCodePaidNativeIdentifiersAndAliasesPreserveExactTarget(t *testing.T) {
	model := "Family/Model:" + strings.Repeat("x", 187)
	for _, scenario := range []struct {
		name, public string
		alias        bool
	}{{"native", model, false}, {"alias", "Team/Assistant:Stable", true}} {
		t.Run(scenario.name, func(t *testing.T) {
			h, ledger, server := newPaidCodeForwardHarness(t)
			snapshot := h.rt.Release().Snapshot.Clone()
			route := snapshot.CodeRoutes["coding"]
			route.Models = []string{model}
			route.PaidTargets = map[string]codemode.PaidTarget{model: route.PaidTargets[modelA]}
			route.ModelRewriting = scenario.alias
			if scenario.alias {
				route.Aliases = map[string]string{scenario.public: model}
			}
			snapshot.CodeRoutes[route.Slug] = route
			providerKey := route.RevisionID + ":" + route.PaidTargets[model].ProviderID
			provider := snapshot.CodePaidConnections[providerKey]
			provider.Capabilities = []routing.Capability{{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"}}
			snapshot.CodePaidConnections[providerKey] = provider
			release, err := runtime.NewRelease(uuid.NewString(), 12, snapshot, map[string][]byte{h.credA: []byte(secretA)})
			if err != nil {
				t.Fatal(err)
			}
			h.rt.release = release
			h.rt.inputs.Prices[0].Model = model
			requestBody := fmt.Sprintf(`{ "model":%q, "input":"keep native input", "max_output_tokens":20, "vendor_extension":{"keep":true} }`, scenario.public)
			responseBody := `{"id":"paid-exact-native","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != strings.Replace(requestBody, scenario.public, model, 1) {
					t.Errorf("native identifier changed request %s", body)
				}
				_, _ = io.WriteString(w, responseBody)
			})
			response := codeDo(t, server, []byte(requestBody), nil)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != 200 || string(body) != responseBody {
				t.Fatalf("valid native model refused %d %s", response.StatusCode, body)
			}
			ledger.wait(t)
			fact := h.sink.last(t).Attempts[0]
			if fact.UpstreamModel != model || fact.Price.Model != model {
				t.Fatalf("paid exact model lost pricing identity %+v", fact)
			}
		})
	}
}

func TestCodeModelPoolRecoveryPreservesNativeRequestedName(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t, codeForwardSetup{Models: []string{"gpt-fixture-main", "gpt-fixture-alternate"}, InitialRecoveryAttempts: 1, Policy: access.KeyPolicy{Scopes: []string{"inference"}}})
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.ModelRewriting = true
	route.ModelPools = map[string][]string{"gpt-fixture-main": {"gpt-fixture-main", "gpt-fixture-alternate"}}
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	ledger.pinTarget, ledger.recoveryTarget = "gpt-fixture-main", "gpt-fixture-alternate"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Model, Input string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Input != "hello" {
			t.Errorf("model recovery changed prompt %+v, %v", request, err)
		}
		if request.Model == "gpt-fixture-main" {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
			return
		}
		if request.Model != "gpt-fixture-alternate" {
			t.Errorf("unexpected selected native model %q", request.Model)
		}
		io.WriteString(w, `{"id":"recovered","model":"gpt-fixture-alternate","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	})
	response := codeDo(t, server, []byte(`{"model":"gpt-fixture-main","input":"hello"}`), nil)
	defer response.Body.Close()
	var completion struct{ ID, Model string }
	if err := json.NewDecoder(response.Body).Decode(&completion); err != nil || response.StatusCode != http.StatusOK || completion.ID != "recovered" || completion.Model != "gpt-fixture-main" {
		t.Fatalf("model recovery lost public name status=%d completion=%+v, %v", response.StatusCode, completion, err)
	}
	ledger.wait(t)
	ledger.wait(t)
}

func TestCodeModelPoolRemovalKeepsRecoveredNativePinAndProvider(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t, codeForwardSetup{Models: []string{"minimax-fixture-main", "gpt-fixture-selected"}, Policy: access.KeyPolicy{Scopes: []string{"inference"}}})
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.ModelRewriting, route.MessagesToCodex = true, true
	route.ModelPools = map[string][]string{"minimax-fixture-main": {"minimax-fixture-main", "gpt-fixture-selected"}}
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	h.rt.release.Snapshot.CodeConnections["revision:other"] = routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codeplans.OpenCodeGoProfile, Endpoint: h.upstream.URL + "/b"}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	ledger.pinTarget = "gpt-fixture-selected"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != ledger.pinTarget {
			t.Errorf("continuation changed selected target %+v, %v", request, err)
		}
		io.WriteString(w, `{"id":"selected","model":"gpt-fixture-selected","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	})
	for _, pools := range []map[string][]string{route.ModelPools, nil} {
		route.ModelPools = pools
		h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+route.BasePath()+"/v1/messages", strings.NewReader(`{"model":"minimax-fixture-main","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+fullKey)
		request.Header.Set("X-Claude-Code-Session-Id", "recovered-native")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var completion struct{ ID, Model string }
		decodeErr := json.NewDecoder(response.Body).Decode(&completion)
		response.Body.Close()
		if decodeErr != nil || response.StatusCode != http.StatusOK || completion.ID != "selected" || completion.Model != "minimax-fixture-main" {
			t.Fatalf("pool removal changed recovered provider or public name pools=%v status=%d completion=%+v, %v", pools, response.StatusCode, completion, decodeErr)
		}
		ledger.wait(t)
	}
}

func newPaidCodeForwardHarness(t *testing.T) (*harness, *codeTestLedger, *httptest.Server) {
	t.Helper()
	h, ledger, server := newCodeForwardHarness(t, codeForwardSetup{Models: []string{modelA}, Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}}})
	snapshot := h.rt.Release().Snapshot.Clone()
	route := snapshot.CodeRoutes["coding"]
	route.ID = uuid.NewString()
	provider := snapshot.Providers[snapshot.Routes[routeSlug].Targets[0].ProviderID]
	provider.ProjectID = &route.ProjectID
	provider.Kind = "openai"
	provider.VendorID = "openai"
	provider.ProfileID = "openai-responses"
	provider.ProfileRevision = connectors.ProfileRevision
	route.PaidParticipation = true
	route.PaidTargets = map[string]codemode.PaidTarget{modelA: {ProviderID: provider.ID, SlotID: h.slotA}}
	snapshot.CodeRoutes["coding"] = route
	snapshot.CodePaidConnections = map[string]routing.Provider{route.RevisionID + ":" + provider.ID: provider}
	release, err := runtime.NewRelease(uuid.NewString(), 8, snapshot, map[string][]byte{h.credA: []byte(secretA)})
	if err != nil {
		t.Fatal(err)
	}
	h.rt.release = release
	authority := h.rt.keys[fullKey]
	authority.LookupID = "paid_key_lookup"
	h.rt.keys[fullKey] = authority
	h.rt.inputs = &usage.RoutingInputs{RefreshedAt: time.Now(), Prices: []usage.RoutingPrice{{RevisionID: uuid.NewString(), EffectiveAt: time.Now().Add(-time.Hour), Price: usage.Price{ProviderKind: provider.Kind, VendorID: new(provider.VendorID), Model: modelA, Operation: "generation", Currency: "USD", InputPerMillion: new("1"), OutputPerMillion: new("2")}}}}

	return h, ledger, server
}

type paidBudgetRefusal struct{}

func (paidBudgetRefusal) Do(context.Context, ...string) (any, error) {
	return []any{int64(1), int64(0), "daily_cost", int64(1000), int64(20_000), int64(24_000)}, nil
}

func TestCodePaidTargetRefusesBeforeUpstreamDispatch(t *testing.T) {
	for _, scenario := range []struct {
		name, code string
		status     int
		setup      func(*testing.T, *harness, *codeTestLedger)
	}{
		{"missing pricing", "code_paid_pricing_unavailable", 503, func(_ *testing.T, h *harness, _ *codeTestLedger) { h.rt.inputs = nil }},
		{"live access denied", "code_permission_denied", 403, func(_ *testing.T, _ *harness, l *codeTestLedger) { l.refuse = "code_permission_denied" }},
		{"credential revoked", "code_paid_target_denied", 403, func(_ *testing.T, h *harness, _ *codeTestLedger) { h.rt.revoked = map[string]bool{h.credA: true} }},
		{"slot restricted", "code_paid_target_denied", 403, func(_ *testing.T, h *harness, _ *codeTestLedger) {
			for key, p := range h.rt.release.Snapshot.CodePaidConnections {
				p.Slots[0].AllowedAPIKeys = []string{"other-key"}
				h.rt.release.Snapshot.CodePaidConnections[key] = p
			}
		}},
		{"paid disabled", "code_paid_participation_disabled", 403, func(_ *testing.T, h *harness, _ *codeTestLedger) {
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.PaidParticipation = false
			h.rt.release.Snapshot.CodeRoutes["coding"] = route
		}},
		{"exhausted spend", "code_budget_exhausted", 429, func(t *testing.T, h *harness, _ *codeTestLedger) {
			authority := h.rt.keys[fullKey]
			authority.Policy.DailyCostLimit = new("0.01")
			authority.Policy.CostBasis = string(limits.BasisCost)
			h.rt.keys[fullKey] = authority
			limiter, err := limits.New(paidBudgetRefusal{}, "olp:paid-refusal")
			if err != nil {
				t.Fatal(err)
			}
			h.gateway.Admission = NewAdmission(limiter, nil, h.gateway.log)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, ledger, server := newPaidCodeForwardHarness(t)
			scenario.setup(t, h, ledger)
			var dispatched atomic.Bool
			handler := func(w http.ResponseWriter, _ *http.Request) { dispatched.Store(true); w.WriteHeader(502) }
			h.mock.set("a", handler)
			h.mock.set("b", handler)
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello","max_output_tokens":20}`), nil)
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != scenario.status || !strings.Contains(string(body), scenario.code) {
				t.Fatalf("refusal %d %s", response.StatusCode, body)
			}
			if dispatched.Load() {
				t.Fatal("refused paid request reached upstream")
			}
		})
	}
}

func TestCodePaidUnknownUsageRemainsUncertain(t *testing.T) {
	h, ledger, server := newPaidCodeForwardHarness(t)
	native := `{"id":"response-without-usage","object":"response","status":"completed","output":[]}`
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, native)
	})
	response := codeDo(t, server, []byte(`{"model":"`+modelA+`","input":"private prompt"}`), nil)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || string(body) != native {
		t.Fatalf("native exchange changed %d %s %v", response.StatusCode, body, err)
	}
	h.sink.await(t, 1)
	fact := h.sink.last(t).Attempts[0]
	if !fact.BillingUncertain || fact.UsageComplete || fact.UsageObserved || fact.Usage != nil {
		t.Fatalf("unknown usage became zero or known %+v", fact)
	}
	settled := ledger.wait(t)
	if settled.Total != nil {
		t.Fatalf("unreported coding tokens became known %+v", settled)
	}
	if !reflect.DeepEqual(ledger.allowance, codemode.Allowance{}) {
		t.Fatal("paid usage became subscription allowance")
	}
}

func TestCodePaidNativeCacheWriteUsageIsPreserved(t *testing.T) {
	h, _, server := newPaidCodeForwardHarness(t)
	snapshot := h.rt.Release().Snapshot.Clone()
	route := snapshot.CodeRoutes["coding"]
	id := route.RevisionID + ":" + route.PaidTargets[modelA].ProviderID
	provider := snapshot.CodePaidConnections[id]
	provider.ProfileID = "openai-chat"
	snapshot.CodePaidConnections[id] = provider
	release, err := runtime.NewRelease(uuid.NewString(), 9, snapshot, map[string][]byte{h.credA: []byte(secretA)})
	if err != nil {
		t.Fatal(err)
	}
	h.rt.release = release
	h.rt.inputs.Prices[0].CacheWriteInputPerMillion = new("3")
	native := `{"id":"chat-paid","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":1,"total_tokens":7,"prompt_tokens_details":{"cache_write_tokens":4}}}`
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, native) })
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(`{"model":"model-a","messages":[{"role":"user","content":"hello"}],"max_completion_tokens":20}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("X-Olp-Code-Affinity", "cache-write")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != native {
		t.Fatalf("native paid response changed %d %s %v", response.StatusCode, body, err)
	}
	fact := h.sink.await(t, 1)[0].Attempts[0]
	if !fact.UsageComplete || fact.BillingUncertain || fact.Usage.CacheWriteInputTokens == nil || *fact.Usage.CacheWriteInputTokens != 4 {
		t.Fatalf("metered cache-write evidence was lost %+v", fact)
	}
}

func TestCodePaidStreamPreservesNativeBytesAndUsage(t *testing.T) {
	for _, encoding := range []string{"", "identity", "gzip"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			h, _, server := newPaidCodeForwardHarness(t)
			native := "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"paid-stream","object":"response","model":"model-a","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}` + "\n\n"
			payload := func() []byte {
				if encoding != "gzip" {
					return []byte(native)
				}
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write([]byte(native)); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				return compressed.Bytes()
			}()
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if encoding != "" {
					w.Header().Set("Content-Encoding", encoding)
				}
				_, _ = w.Write(payload)
			})
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello","stream":true}`), http.Header{"Accept-Encoding": {"gzip"}})
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != 200 || !bytes.Equal(body, payload) || response.Header.Get("Content-Encoding") != encoding {
				t.Fatalf("native paid stream changed %d %q %v", response.StatusCode, body, err)
			}
			fact := h.sink.await(t, 1)[0].Attempts[0]
			if !fact.UsageComplete || fact.BillingUncertain || fact.Usage == nil || fact.Usage.TotalTokens != 3 {
				t.Fatalf("paid stream usage was not attributed %+v", fact)
			}
		})
	}
}

func TestCodePaidHTTPStatusDeterminesAccountingOutcome(t *testing.T) {
	for _, scenario := range []struct {
		status  int
		outcome string
	}{{200, "success"}, {299, "success"}, {300, "failure"}} {
		t.Run(fmt.Sprint(scenario.status), func(t *testing.T) {
			h, _, server := newPaidCodeForwardHarness(t)
			const native = `{"id":"paid-status","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(scenario.status)
				_, _ = io.WriteString(w, native)
			})
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello"}`), nil)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != scenario.status || string(body) != native {
				t.Fatalf("paid status response changed %d %s %v", response.StatusCode, body, err)
			}
			envelope := h.sink.await(t, 1)[0]
			if envelope.Outcome != scenario.outcome || envelope.Attempts[0].Class != scenario.outcome || envelope.Attempts[0].Status != response.StatusCode {
				t.Fatalf("paid HTTP status lost accounting outcome %+v", envelope)
			}
		})
	}
}

func TestCodePaidProviderErrorRedactsAppliedCredential(t *testing.T) {
	for _, scenario := range []struct {
		shape  string
		status int
	}{{"message", 400}, {"escaped message", 401}, {"member", 400}, {"escaped member", 401}, {"text", 400}} {
		t.Run(scenario.shape, func(t *testing.T) {
			h, _, server := newPaidCodeForwardHarness(t)
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(scenario.status)
				credential := func() string {
					if strings.Contains(scenario.shape, "escaped") {
						return strings.ReplaceAll(secretA, "s", `\u0073`)
					}
					return secretA
				}()
				if strings.Contains(scenario.shape, "member") {
					_, _ = io.WriteString(w, `{"error":{"`+credential+`":"failed"}}`)
					return
				}
				if scenario.shape == "text" {
					_, _ = io.WriteString(w, "rejected Bearer "+credential)
					return
				}
				_, _ = io.WriteString(w, `{"error":{"message":"rejected Bearer `+credential+`","code":"`+credential+`"}}`)
			})
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello"}`), nil)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || strings.Contains(string(body), secretA) {
				t.Fatalf("paid error exposed applied credentials %q %v", body, err)
			}
			if strings.Contains(scenario.shape, "member") && response.StatusCode != 502 {
				t.Fatalf("credential member was forwarded %d %s", response.StatusCode, body)
			}
			if !strings.Contains(scenario.shape, "member") && (response.StatusCode != scenario.status || !strings.Contains(string(body), "[REDACTED]")) {
				t.Fatalf("native error was not safely retained %d %s", response.StatusCode, body)
			}
		})
	}
}

func TestCodePaidCompactionRefusesBeforeDispatch(t *testing.T) {
	h, _, server := newPaidCodeForwardHarness(t)
	var dispatched atomic.Bool
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		dispatched.Store(true)
		_, _ = io.WriteString(w, `{"id":"wrong-generation"}`)
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses/compact", strings.NewReader(`{"model":"model-a","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("Session_id", "compact")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 400 || !strings.Contains(string(body), "code_paid_operation_unsupported") || dispatched.Load() {
		t.Fatalf("paid compaction reached generation %d %s dispatched %v", response.StatusCode, body, dispatched.Load())
	}
}

func TestCodePaidHostingAdaptationRefusesBeforeDispatch(t *testing.T) {
	h, _, server := newPaidCodeForwardHarness(t)
	snapshot := h.rt.Release().Snapshot.Clone()
	route := snapshot.CodeRoutes["coding"]
	key := route.RevisionID + ":" + route.PaidTargets[modelA].ProviderID
	provider := snapshot.CodePaidConnections[key]
	provider.Kind = "vertex_ai"
	provider.ProfileID = "vertex-anthropic"
	provider.CloudProject = "fixture-project"
	provider.CloudRegion = "us-central1"
	provider.Capabilities = []routing.Capability{{Model: modelA, Operation: "generation", Surface: "anthropic", Mode: "unary"}}
	snapshot.CodePaidConnections[key] = provider
	release, releaseErr := runtime.NewRelease(uuid.NewString(), 10, snapshot, map[string][]byte{h.credA: []byte(secretA)})
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}
	h.rt.release = release
	h.rt.inputs.Prices[0].ProviderKind = provider.Kind
	var dispatched atomic.Bool
	h.mock.set("a", func(http.ResponseWriter, *http.Request) { dispatched.Store(true) })
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/messages", strings.NewReader(`{"model":"model-a","messages":[{"role":"user","content":"hello"}],"max_tokens":20}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("X-Olp-Code-Affinity", "hosting")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 400 || !strings.Contains(string(body), "code_paid_operation_unsupported") || dispatched.Load() {
		t.Fatalf("unsupported hosting advertised raw native forwarding %d %s", response.StatusCode, body)
	}
}

func TestCodePaidAliasContinuationUsesPinnedTargetBeforePricing(t *testing.T) {
	h, ledger, server := newPaidCodeForwardHarness(t)
	snapshot := h.rt.Release().Snapshot.Clone()
	route := snapshot.CodeRoutes["coding"]
	route.Models = []string{modelA, modelB}
	route.ModelRewriting = true
	route.Aliases = map[string]string{"assistant": modelB}
	route.PaidTargets = map[string]codemode.PaidTarget{modelA: route.PaidTargets[modelA], modelB: route.PaidTargets[modelA]}
	snapshot.CodeRoutes["coding"] = route
	providerKey := route.RevisionID + ":" + route.PaidTargets[modelA].ProviderID
	provider := snapshot.CodePaidConnections[providerKey]
	provider.Capabilities = slices.Concat(provider.Capabilities, []routing.Capability{{Model: modelB, Operation: "generation", Surface: "openai", Mode: "unary"}})
	snapshot.CodePaidConnections[providerKey] = provider
	release, err := runtime.NewRelease(uuid.NewString(), 11, snapshot, map[string][]byte{h.credA: []byte(secretA)})
	if err != nil {
		t.Fatal(err)
	}
	h.rt.release = release
	ledger.pinTarget = modelA
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"model-a"`) {
			t.Errorf("pinned alias changed upstream target %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"paid-alias","object":"response","model":"model-a","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
	})
	response := codeDo(t, server, []byte(`{"model":"assistant","input":"hello"}`), nil)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("paid alias selected current alias before its pin %d %s", response.StatusCode, body)
	}
	fact := h.sink.await(t, 1)[0].Attempts[0]
	if fact.UpstreamModel != modelA || fact.Price.Model != modelA {
		t.Fatalf("paid alias pricing diverged from its selected target %+v", fact)
	}
}

func TestCodeAliasRepointAcrossPaidAndSubscriptionRetainsPinnedTarget(t *testing.T) {
	for _, scenario := range []struct {
		name, pinned, repointed, credential string
	}{{"paid", modelA, modelB, "Bearer " + secretA}, {"subscription", modelB, modelA, "Bearer upstream-only"}} {
		t.Run(scenario.name, func(t *testing.T) {
			h, ledger, server := newPaidCodeForwardHarness(t)
			snapshot := h.rt.Release().Snapshot.Clone()
			route := snapshot.CodeRoutes["coding"]
			route.Models = []string{modelA, modelB}
			route.ModelRewriting = true
			route.Aliases = map[string]string{"assistant": scenario.repointed}
			snapshot.CodeRoutes[route.Slug] = route
			release, err := runtime.NewRelease(uuid.NewString(), 13, snapshot, map[string][]byte{h.credA: []byte(secretA)})
			if err != nil {
				t.Fatal(err)
			}
			h.rt.release = release
			ledger.pinTarget = scenario.pinned
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), `"model":"`+scenario.pinned+`"`) || r.Header.Get("Authorization") != scenario.credential {
					t.Errorf("alias repoint changed target or owner %s", body)
				}
				_, _ = fmt.Fprintf(w, `{"id":"alias-owner","model":%q,"status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`, scenario.pinned)
			})
			response := codeDo(t, server, []byte(`{"model":"assistant","input":"continue"}`), nil)
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != 200 || !strings.Contains(string(body), `"model":"assistant"`) {
				t.Fatalf("alias repoint stranded existing owner %d %s", response.StatusCode, body)
			}
			ledger.wait(t)
		})
	}
}

func (l *codeTestLedger) ResolvePaidModel(_ context.Context, in resources.CodeAdmission, _ access.Authority) (codemode.Operation, error) {
	pins := func() []codemode.Pin {
		if l.pinTarget == "" {
			return nil
		}
		return []codemode.Pin{{Model: in.Operation.PublicModel(), TargetModel: l.pinTarget}}
	}()
	return in.Route.ResolvePinnedModel(in.Operation, pins)
}

func TestCodePaidSuccessfulDeliveryRetainsLargeAndEncodedNativeBytes(t *testing.T) {
	for _, kind := range []string{"large unary", "large stream", "encoded stream"} {
		t.Run(kind, func(t *testing.T) {
			h, ledger, server := newPaidCodeForwardHarness(t)
			payload := func() []byte {
				if kind == "large unary" {
					return []byte(`{"output":"` + strings.Repeat("x", codewire.MaxBody+1) + `"}`)
				}
				if kind == "large stream" {
					return []byte("data: " + `{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", codewire.MaxBody+1) + `"}` + "\n\n")
				}
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write([]byte("data: " + `{"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n")); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				return compressed.Bytes()
			}()
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				if kind != "large unary" {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				if kind == "encoded stream" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				_, _ = w.Write(payload)
			})
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello"}`), http.Header{"Accept-Encoding": {"gzip"}})
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != 200 || !bytes.Equal(body, payload) {
				h.sink.await(t, 1)
				ledger.mu.Lock()
				outcomes := slices.Clone(ledger.outcomes)
				ledger.mu.Unlock()
				t.Fatalf("successful paid native delivery changed %d bytes %d/%d %v outcomes %+v", response.StatusCode, len(body), len(payload), err, outcomes)
			}
		})
	}
}

func TestCodePaidNativeErrorStreamRedactsCredential(t *testing.T) {
	for _, kind := range []string{"error", "response.failed", "response.incomplete"} {
		t.Run(kind, func(t *testing.T) {
			h, _, server := newPaidCodeForwardHarness(t)
			native := fmt.Sprintf("id: paid-error\r\nevent: %s\r\ndata: {\"type\":%q,\"response\":{\"error\":{\"message\":\"Bearer %s\"}},\"extension\":1.2300}\r\n\r\n", kind, kind, secretA)
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, native)
			})
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello","stream":true}`), nil)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != 200 || string(body) != strings.ReplaceAll(native, secretA, "[REDACTED]") {
				t.Fatalf("native paid error frame was not safely retained %d %s %v", response.StatusCode, body, err)
			}
		})
	}
}

func TestClaudeSubscriptionPreservesNativeToolRoundTrip(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: claudeauth.ProfileID, Endpoint: h.upstream.URL + "/a"}}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	first := `{"model":"native-model","system":"x-anthropic-billing-header: preserve","metadata":{"user_id":"native-user"},"messages":[{"role":"user","content":"Read main.go"}],"tools":[{"name":"read_file","input_schema":{"type":"object"}}],"max_tokens":64}`
	second := `{"model":"native-model","system":"x-anthropic-billing-header: preserve","metadata":{"user_id":"native-user"},"messages":[{"role":"user","content":"Read main.go"},{"role":"assistant","content":[{"type":"tool_use","id":"read-1","name":"read_file","input":{"path":"main.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"read-1","content":"package main"}]}],"tools":[{"name":"read_file","input_schema":{"type":"object"}}],"max_tokens":64}`
	for _, turn := range []struct{ request, response string }{
		{first, `{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"read-1","name":"read_file","input":{"path":"main.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":4}}`},
		{second, `{"id":"msg-2","type":"message","role":"assistant","content":[{"type":"text","text":"The file declares package main"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":6}}`},
	} {
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != turn.request {
				t.Error("native prompt, identity or tool history was rewritten")
			}
			if r.URL.Path != "/a/v1/messages" || r.Header.Get("Anthropic-Version") != "fixture-version" || !slices.Contains(strings.Split(r.Header.Get("Anthropic-Beta"), ","), "fixture-beta") || !slices.Contains(strings.Split(r.Header.Get("Anthropic-Beta"), ","), claudeauth.OAuthBeta) {
				t.Error("native Messages capability headers or endpoint changed")
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, turn.response)
		})
		request, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/code/coding/v1/messages", strings.NewReader(turn.request))
		if err != nil {
			t.Fatal(err)
		}
		request.Header = http.Header{"Authorization": {"Bearer " + fullKey}, "Content-Type": {"application/json"}, "X-Claude-Code-Session-Id": {"claude-tool-session"}, "Anthropic-Version": {"fixture-version"}, "Anthropic-Beta": {"fixture-beta"}}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || string(body) != turn.response {
			t.Fatalf("native tool turn changed, status %d error %v", response.StatusCode, err)
		}
		if usage := ledger.wait(t); usage.Total == nil || *usage.Total == 0 {
			t.Fatal("native tool turn usage was lost")
		}
	}
}

func TestCodeAntigravityToolRoundTripAndAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h, ledger, server := newCodeAntigravityHarness(t, true)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Project string `json:"project"`
					Model   string `json:"model"`
					Request struct {
						System struct {
							Parts []struct {
								Text string `json:"text"`
							} `json:"parts"`
						} `json:"systemInstruction"`
						Contents []struct {
							Parts []json.RawMessage `json:"parts"`
						} `json:"contents"`
					} `json:"request"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Project != "verified-project" || body.Model != "gemini-fixture" || len(body.Request.System.Parts) != 1 || body.Request.System.Parts[0].Text != "Keep this instruction" || r.Header.Get("User-Agent") != "fixture-client" || r.Header.Get("Authorization") != "Bearer upstream-only" {
					t.Error("envelope lost verified project, credentials, prompt or client identity")
				}
				reply := `{"response":{"responseId":"fixture-response","candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call-read","name":"read_file","args":{"path":"file.txt"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}}`
				if len(body.Request.Contents) > 1 {
					last := body.Request.Contents[len(body.Request.Contents)-1]
					if len(last.Parts) != 1 || !bytes.Contains(last.Parts[0], []byte(`"functionResponse"`)) || !bytes.Contains(last.Parts[0], []byte(`"name":"read_file"`)) || !bytes.Contains(last.Parts[0], []byte(`"file contents"`)) {
						t.Error("tool result lost its call binding or content")
					}
					reply = `{"response":{"responseId":"fixture-final","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"Read complete"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3,"totalTokenCount":12}}}`
				}
				if stream {
					if r.URL.Path != "/a/v1internal:streamGenerateContent" || r.URL.RawQuery != "alt=sse" {
						t.Error("stream did not select the declared SSE transport")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: "+reply+"\n\n")
					return
				}
				if r.URL.Path != "/a/v1internal:generateContent" || r.URL.RawQuery != "" {
					t.Error("unary did not select the declared transport")
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, reply)
			})
			for _, continuation := range []bool{false, true} {
				messages := `[{"role":"system","content":"Keep this instruction"},{"role":"user","content":"Read a file"}]`
				if continuation {
					messages = `[{"role":"system","content":"Keep this instruction"},{"role":"user","content":"Read a file"},{"role":"assistant","content":null,"tool_calls":[{"id":"call-read","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"file.txt\"}"}}]},{"role":"tool","tool_call_id":"call-read","content":"file contents"}]`
				}
				body := fmt.Sprintf(`{"model":"gemini-fixture","messages":%s,"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}],"stream":%t,"max_tokens":64}`, messages, stream)
				request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer "+fullKey)
				request.Header.Set("X-Opencode-Session-Id", "antigravity-roundtrip")
				request.Header.Set("User-Agent", "fixture-client")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				result, _ := io.ReadAll(response.Body)
				expected := "read_file"
				total := int64(7)
				if continuation {
					expected = "Read complete"
					total = 12
				}
				if response.StatusCode != http.StatusOK || !bytes.Contains(result, []byte(expected)) || !bytes.Contains(result, []byte(`"choices"`)) || stream && !bytes.Contains(result, []byte("[DONE]")) {
					t.Fatalf("tool exchange failed, status %d body %s", response.StatusCode, result)
				}
				if usage := ledger.wait(t); usage.Total == nil || *usage.Total != total {
					t.Fatalf("lost authoritative Gemini usage %+v", usage)
				}
			}
		})
	}
}

type codeCapacityLedger struct {
	CodeLedger
	occupied chan struct{}
}

func (l *codeCapacityLedger) Admit(ctx context.Context, input resources.CodeAdmission) (resources.CodePermit, error) {
	select {
	case l.occupied <- struct{}{}:
		permit, err := l.CodeLedger.Admit(ctx, input)
		if err != nil {
			<-l.occupied
		}
		return permit, err
	default:
		return resources.CodePermit{}, codemode.Refuse(429, "code_capacity_exceeded")
	}
}

func (l *codeCapacityLedger) Abort(ctx context.Context, id string) error {
	if err := l.CodeLedger.Abort(ctx, id); err != nil {
		return err
	}
	<-l.occupied
	return nil
}

func (l *codeCapacityLedger) Settle(ctx context.Context, id string, usage codemode.Usage) error {
	if err := l.CodeLedger.Settle(ctx, id, usage); err != nil {
		return err
	}
	<-l.occupied
	return nil
}

type codeAntigravityAuthorizer struct{}

func (codeAntigravityAuthorizer) AuthorizeCode(context.Context, routing.Configuration, codemode.Account, codemode.Dispatch) (codemode.Authorization, error) {
	return codemode.Authorization{Principal: "principal", ProjectID: "verified-project", Headers: http.Header{"Authorization": {"Bearer upstream-only"}}}, nil
}

func TestCodeAntigravityRefusesUnrepresentableInputBeforeDispatch(t *testing.T) {
	for _, input := range []string{
		`{"model":"gemini-fixture","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image"}}]}]}`,
		`{"model":"gemini-fixture","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"high"}`,
		`{"model":"gemini-fixture","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call-read","type":"function","function":{"name":"read_file","arguments":"{}","thoughtSignature":"opaque"}}]}]}`,
	} {
		h, ledger, server := newCodeAntigravityHarness(t, true)
		h.gateway.CodeLedger = &codeCapacityLedger{CodeLedger: ledger, occupied: make(chan struct{}, 1)}
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			t.Error("unrepresentable input was dispatched")
			w.WriteHeader(500)
		})
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(input))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		request.Header.Set("X-Opencode-Session-Id", "unsupported-state")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, _ := io.ReadAll(response.Body)
		if response.StatusCode != 422 || !bytes.Contains(result, []byte("code_translation_unsupported_semantics")) {
			t.Fatalf("unsupported input accepted, status %d body %s", response.StatusCode, result)
		}
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"admitted after refusal"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}`)
		})
		followup, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(`{"model":"gemini-fixture","messages":[{"role":"user","content":"hello"}]}`))
		followup.Header = request.Header.Clone()
		accepted, err := server.Client().Do(followup)
		if err != nil {
			t.Fatal(err)
		}
		defer accepted.Body.Close()
		acceptedBody, _ := io.ReadAll(accepted.Body)
		if accepted.StatusCode != 200 || !bytes.Contains(acceptedBody, []byte("admitted after refusal")) {
			t.Fatalf("refused input kept capacity reserved, status %d body %s", accepted.StatusCode, acceptedBody)
		}

	}
}

func TestCodeAntigravityRefusesSignedOutputInsteadOfDroppingState(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h, ledger, server := newCodeAntigravityHarness(t, true)
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			body := `{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call-read","name":"read_file","args":{}},"thoughtSignature":"opaque"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}}`
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: "+body+"\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		})
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"gemini-fixture","messages":[{"role":"user","content":"Read a file"}],"stream":%t}`, stream)))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		request.Header.Set("X-Opencode-Session-Id", "signed-output")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != 502 || bytes.Contains(body, []byte("read_file")) || !bytes.Contains(body, []byte("code_upstream_unsupported_state")) {
			t.Fatalf("signed function call lost state, status %d body %s", response.StatusCode, body)
		}
		if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 7 {
			t.Fatalf("dispatched signed result lost authoritative usage %+v", usage)
		}
	}
}

func newCodeAntigravityHarness(t *testing.T, enabled bool) (*harness, *codeTestLedger, *httptest.Server) {
	t.Helper()
	h, ledger, server := newCodeForwardHarness(t, codeForwardSetup{Models: []string{"gemini-fixture"}, Policy: access.KeyPolicy{Scopes: []string{"inference"}}})
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.ChatToAntigravity = enabled
	h.rt.release.Snapshot.CodeRoutes["coding"] = route
	h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: "antigravity-subscription", Endpoint: h.upstream.URL + "/a"}}
	h.gateway.CodeAuthorizer = codeAntigravityAuthorizer{}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	return h, ledger, server
}

func TestCodeAntigravityDisabledPolicyDoesNotDispatch(t *testing.T) {
	h, _, server := newCodeAntigravityHarness(t, false)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		t.Error("disabled translation dispatched")
		w.WriteHeader(500)
	})
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(`{"model":"gemini-fixture","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("X-Opencode-Session-Id", "disabled-translation")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 400 {
		t.Fatalf("disabled translation accepted status %d", response.StatusCode)
	}
}

func TestCodeAntigravityCancellationStopsUpstreamAndLeavesUnknownUsage(t *testing.T) {
	h, ledger, server := newCodeAntigravityHarness(t, true)
	stopped := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		defer close(stopped)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"partial text"}]}}]}}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(`{"model":"gemini-fixture","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("X-Opencode-Session-Id", "cancel-antigravity")
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
		if strings.Contains(line, "partial text") {
			break
		}
	}
	cancel()
	remaining, _ := io.ReadAll(reader)
	if bytes.Contains(remaining, []byte("[DONE]")) {
		t.Fatal("cancelled stream fabricated completion")
	}
	if usage := ledger.wait(t); usage.Total != nil || usage.Input != nil || usage.Output != nil {
		t.Fatalf("cancelled stream fabricated usage %+v", usage)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled stream kept upstream work alive")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.outcomes) == 0 || ledger.outcomes[len(ledger.outcomes)-1].Origin != "client" || ledger.outcomes[len(ledger.outcomes)-1].Kind != "canceled" {
		t.Fatalf("cancellation lost client outcome %+v", ledger.outcomes)
	}
}

func TestCodeNativeChatPreservesBytesWithAntigravityPolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		h, ledger, server := newCodeForwardHarness(t)
		route := h.rt.release.Snapshot.CodeRoutes["coding"]
		route.ChatToAntigravity, route.ChatToCodex = enabled, enabled
		h.rt.release.Snapshot.CodeRoutes["coding"] = route
		h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codeplans.ZAIProfile, Endpoint: h.upstream.URL + "/a"}}
		if err := h.rt.release.Snapshot.Compile(); err != nil {
			t.Fatal(err)
		}
		body := []byte(`{ "model":"native-model", "messages":[{"role":"user","content":"hello"}], "tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}], "tool_choice":{"type":"function","function":{"name":"read_file","required_state":"opaque"}}, "vendor_extension":{"keep":true} }`)
		reply := []byte(`{ "choices":[{"message":{"role":"assistant","content":"native reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3},"vendor_extension":{"keep":true} }`)
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			received, _ := io.ReadAll(r.Body)
			if !bytes.Equal(received, body) || r.URL.RawQuery != "native_option=1" {
				t.Error("native request bytes or query changed")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(reply)
		})
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions?native_option=1", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		request.Header.Set("X-Opencode-Session-Id", "native-chat")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		result, _ := io.ReadAll(response.Body)
		if response.StatusCode != 200 || !bytes.Equal(result, reply) {
			t.Fatalf("native response changed status %d body %s", response.StatusCode, result)
		}
		if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
			t.Fatalf("native usage lost %+v", usage)
		}
	}
}

func TestCodePaidFallbackRecoversSameModelWithoutModelPool(t *testing.T) {
	h, ledger, server := newPaidCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.PaidFallbackTargets, route.PaidTargets = route.PaidTargets, nil
	route.InitialRecoveryAttempts = 1
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	ledger.recoveryUnavailable = true
	var subscription, paid atomic.Bool
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer upstream-only" {
			subscription.Store(true)
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
			return
		}
		paid.Store(true)
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, []byte(`{"model":"model-a","input":"hello","max_output_tokens":20}`)) {
			t.Error("same-model fallback changed input")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"paid-recovery","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
	})
	response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello","max_output_tokens":20}`), nil)
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(body), "paid-recovery") || !subscription.Load() || !paid.Load() {
		t.Fatalf("fallback %d %s subscription=%v paid=%v", response.StatusCode, body, subscription.Load(), paid.Load())
	}
	if u := ledger.wait(t); u.Total == nil || *u.Total != 0 {
		t.Fatalf("rejected subscription consumption %+v", u)
	}
	if u := ledger.wait(t); u.Total == nil || *u.Total != 3 {
		t.Fatalf("paid fallback consumption %+v", u)
	}
	env := h.sink.last(t)
	if env.KeyID != h.keyID || env.Attempts[0].UpstreamModel != modelA || !env.Attempts[0].UsageComplete || env.Attempts[0].ProviderID != route.PaidFallbackTargets[modelA].ProviderID {
		t.Fatalf("fallback attribution %+v", env)
	}
}

func (l *codeTestLedger) ResolvePaidFallback(_ context.Context, in resources.CodeAdmission, _ access.Authority) (*codemode.Operation, error) {
	if in.Recovery == nil && !in.InitialRecovery {
		return nil, nil
	}
	pins := func() []codemode.Pin {
		if l.pinTarget == "" {
			return nil
		}
		return []codemode.Pin{{Model: in.Operation.PublicModel(), TargetModel: l.pinTarget}}
	}()
	return in.Route.PaidFallbackModel(in.Operation, pins)
}

func TestCodePaidFallbackAdmitsNewPortableWorkBeforeAnySubscriptionDispatch(t *testing.T) {
	h, ledger, server := newPaidCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.PaidFallbackTargets, route.PaidTargets = route.PaidTargets, nil
	route.InitialRecoveryAttempts = 1
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	ledger.subscriptionUnavailable = true
	var subscription, paid atomic.Bool
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer upstream-only" {
			subscription.Store(true)
			w.WriteHeader(500)
			return
		}
		paid.Store(true)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"new-paid","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
	})
	response := codeDo(t, server, []byte(`{"model":"model-a","input":"complete input","max_output_tokens":20}`), nil)
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(body), "new-paid") || subscription.Load() || !paid.Load() {
		t.Fatalf("initial fallback %d %s subscription=%v paid=%v", response.StatusCode, body, subscription.Load(), paid.Load())
	}
	if u := ledger.wait(t); u.Total == nil || *u.Total != 3 {
		t.Fatalf("initial fallback usage %+v", u)
	}
}

func newPaidFallbackHarness(t *testing.T) (*harness, *codeTestLedger, *httptest.Server) {
	t.Helper()
	h, ledger, server := newPaidCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.PaidFallbackTargets, route.PaidTargets = route.PaidTargets, nil
	route.InitialRecoveryAttempts = 1
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	ledger.subscriptionUnavailable = true
	return h, ledger, server
}

func TestCodePaidFallbackRefusesBeforePaidDispatch(t *testing.T) {
	for _, scenario := range []struct {
		name, code string
		setup      func(*testing.T, *harness, *codeTestLedger)
	}{
		{"fallback disabled", "code_account_unavailable", func(_ *testing.T, h *harness, _ *codeTestLedger) {
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.PaidFallbackTargets = nil
			h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
		}},
		{"recovery disabled", "code_account_unavailable", func(_ *testing.T, h *harness, _ *codeTestLedger) {
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.InitialRecoveryAttempts = 0
			h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
		}},
		{"paid participation disabled", "code_paid_participation_disabled", func(_ *testing.T, h *harness, _ *codeTestLedger) {
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.PaidParticipation = false
			h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
		}},
		{"paid authority denied", "code_pool_denied", func(_ *testing.T, _ *harness, l *codeTestLedger) { l.paidRefuse = "code_pool_denied" }},
		{"revoked credential", "code_paid_target_denied", func(_ *testing.T, h *harness, _ *codeTestLedger) { h.rt.revoked = map[string]bool{h.credA: true} }},
		{"missing pricing", "code_paid_pricing_unavailable", func(_ *testing.T, h *harness, _ *codeTestLedger) { h.rt.inputs = nil }},
		{"spend exhausted", "code_budget_exhausted", func(t *testing.T, h *harness, _ *codeTestLedger) {
			authority := h.rt.keys[fullKey]
			authority.Policy.DailyCostLimit = new("0.01")
			authority.Policy.CostBasis = string(limits.BasisCost)
			h.rt.keys[fullKey] = authority
			limiter, err := limits.New(paidBudgetRefusal{}, "olp:fallback-refusal")
			if err != nil {
				t.Fatal(err)
			}
			h.gateway.Admission = NewAdmission(limiter, nil, h.gateway.log)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, ledger, server := newPaidFallbackHarness(t)
			scenario.setup(t, h, ledger)
			var dispatched atomic.Bool
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) { dispatched.Store(true); w.WriteHeader(502) })
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello","max_output_tokens":20}`), nil)
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode < 400 || !strings.Contains(string(body), scenario.code) || dispatched.Load() {
				t.Fatalf("paid refusal %d %s dispatched=%v", response.StatusCode, body, dispatched.Load())
			}
		})
	}
}

func TestCodePaidFallbackDoesNotReplayAcceptedAmbiguousOrOpaqueWork(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		status  int
		body    string
		headers http.Header
	}{
		{"accepted", 200, `{"id":"accepted","status":"completed","output":[],"usage":{"total_tokens":1}}`, nil},
		{"ambiguous failure", 500, `{"error":{"type":"usage_limit_reached"}}`, nil},
		{"unproven rejection", 429, `{"error":{"type":"server_error"}}`, nil},
		{"opaque continuation", 429, `{"error":{"type":"usage_limit_reached"}}`, http.Header{"X-Codex-Turn-State": []string{"opaque"}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, ledger, server := newPaidFallbackHarness(t)
			ledger.subscriptionUnavailable = false
			ledger.recoveryUnavailable = true
			var paid atomic.Bool
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer upstream-only" {
					paid.Store(true)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(scenario.status)
				io.WriteString(w, scenario.body)
			})
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello","max_output_tokens":20}`), scenario.headers)
			body, _ := io.ReadAll(response.Body)
			if scenario.name == "opaque continuation" {
				if response.StatusCode != 409 || !strings.Contains(string(body), "code_recovery_unsafe") || paid.Load() {
					t.Fatalf("opaque replay %d %s paid=%v", response.StatusCode, body, paid.Load())
				}
				return
			}
			if response.StatusCode != scenario.status || string(body) != scenario.body || paid.Load() {
				t.Fatalf("unsafe replay %d %s paid=%v", response.StatusCode, body, paid.Load())
			}
		})
	}
}

func TestCodePaidFallbackUnknownMoneyRetainsUncertainAccounting(t *testing.T) {
	h, ledger, server := newPaidFallbackHarness(t)
	native := `{"id":"unmetered","status":"completed","output":[]}`
	h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, native)
	})
	response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello","max_output_tokens":20}`), nil)
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || string(body) != native {
		t.Fatalf("paid delivery %d %s", response.StatusCode, body)
	}
	if usage := ledger.wait(t); usage.Total != nil {
		t.Fatalf("unknown paid usage became known %+v", usage)
	}
	fact := h.sink.last(t).Attempts[0]
	if !fact.BillingUncertain || fact.UsageComplete || fact.Usage != nil {
		t.Fatalf("paid uncertainty lost %+v", fact)
	}
}

func TestCodePaidFallbackSubstitutionRequiresExplicitModelPool(t *testing.T) {
	for _, pool := range []bool{false, true} {
		t.Run(fmt.Sprint(pool), func(t *testing.T) {
			h, ledger, server := newPaidFallbackHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.Models = []string{modelA, modelB}
			route.PaidFallbackTargets = map[string]codemode.PaidTarget{modelB: route.PaidFallbackTargets[modelA]}
			if pool {
				route.ModelRewriting = true
				route.ModelPools = map[string][]string{modelA: {modelA, modelB}}
			}
			h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
			for key, provider := range h.rt.release.Snapshot.CodePaidConnections {
				provider.Capabilities = append(provider.Capabilities, routing.Capability{Model: modelB, Operation: "generation", Surface: "openai", Mode: "unary"})
				h.rt.release.Snapshot.CodePaidConnections[key] = provider
			}
			price := h.rt.inputs.Prices[0]
			price.Price.Model = modelB
			h.rt.inputs.Prices = append(h.rt.inputs.Prices, price)
			var paid atomic.Bool
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				paid.Store(true)
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), `"model":"model-b"`) || !strings.Contains(string(body), `"input":"hello"`) {
					t.Errorf("substitution changed input or ignored target %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"substituted","model":"model-b","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
			})
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello","max_output_tokens":20}`), nil)
			body, _ := io.ReadAll(response.Body)
			if !pool {
				if response.StatusCode != 503 || paid.Load() {
					t.Fatalf("implicit substitution %d %s paid=%v", response.StatusCode, body, paid.Load())
				}
				return
			}
			if response.StatusCode != 200 || !paid.Load() {
				t.Fatalf("explicit substitution refused %d %s", response.StatusCode, body)
			}
			ledger.wait(t)
			fact := h.sink.last(t)
			if ledger.paidOperation.PublicModel() != modelA || fact.Attempts[0].UpstreamModel != modelB {
				t.Fatalf("substitution attribution %+v", fact)
			}
		})
	}
}

func TestCodeChatToCodexToolContinuation(t *testing.T) {
	for _, input := range []struct{ stream, usage bool }{{false, false}, {true, true}, {true, false}} {
		t.Run(fmt.Sprint(input), func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.ChatToCodex, route.ModelRewriting = true, true
			route.Aliases = map[string]string{"team-model": "native-model"}
			h.rt.release.Snapshot.CodeRoutes["coding"] = route
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var request struct {
					Model        string
					Stream       bool
					Store        *bool
					Instructions *string
					Reasoning    struct{ Effort string }
					ToolChoice   struct{ Type, Name string } `json:"tool_choice"`
					Input        []struct {
						Type, Role, CallID, Arguments, Output string
						Content                               []struct{ Text string }
					}
				}
				if err := json.Unmarshal(body, &request); err != nil || request.Model != "native-model" || !request.Stream || request.Store == nil || *request.Store || request.Instructions == nil || *request.Instructions != "" || request.Reasoning.Effort != "high" || request.ToolChoice.Type != "function" || request.ToolChoice.Name != "read_file" || request.Input[0].Role != "developer" || request.Input[0].Content[0].Text != "Preserve my instructions" {
					t.Errorf("translated request %s, %v", body, err)
				}
				if r.URL.Path != "/a/responses" || r.Header.Get("Originator") != "" || r.Header.Get("X-Opencode-Session-Id") != "chat-tools" || r.Header.Get("Authorization") != "Bearer upstream-only" {
					t.Errorf("target %s headers %v", r.URL, r.Header)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: "+`{"type":"response.created","response":{"id":"chat-tool","status":"in_progress","output":[]}}`+"\n\n")
				if bytes.Contains(body, []byte(`"function_call_output"`)) {
					if !bytes.Contains(body, []byte(`"call_id":"call-read"`)) || !bytes.Contains(body, []byte(`"arguments":"{\"path\":\"main.go\"}"`)) || !bytes.Contains(body, []byte(`"output":"package main"`)) {
						t.Errorf("tool history lost %s", body)
					}
					fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"chat-tool","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"File read"}]}],"usage":{"input_tokens":13,"output_tokens":2,"total_tokens":15}}}`+"\n\n")
					return
				}
				for _, event := range []string{
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":""}}`,
					`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`,
					`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"main.go\"}"}`,
					`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}}`,
					`{"type":"response.completed","response":{"id":"chat-tool","status":"completed","output":[{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`,
				} {
					fmt.Fprint(w, "data: "+event+"\n\n")
				}
			})
			for _, history := range []struct {
				body  string
				total int64
			}{
				{``, 11},
				{`,{"role":"assistant","content":null,"tool_calls":[{"id":"call-read","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},{"role":"tool","tool_call_id":"call-read","content":"package main"}`, 15},
			} {
				request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"team-model","messages":[{"role":"system","content":"Preserve my instructions"},{"role":"user","content":"Read main.go"}%s],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"tool_choice":{"type":"function","function":{"name":"read_file"}},"reasoning_effort":"high","stream":%t,"stream_options":{"include_usage":%t}}`, history.body, input.stream, input.usage)))
				request.Header.Set("Authorization", "Bearer "+fullKey)
				request.Header.Set("X-Opencode-Session-Id", "chat-tools")
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 200 || !bytes.Contains(body, []byte(`"model":"team-model"`)) || bytes.Contains(body, []byte(`"total_tokens":`)) != (!input.stream || input.usage) || input.stream && !bytes.Contains(body, []byte("[DONE]")) {
					t.Fatalf("Chat exchange %d %s", response.StatusCode, body)
				}
				if history.body == "" && (!bytes.Contains(body, []byte(`"tool_calls"`)) || !bytes.Contains(body, []byte(`"name":"read_file"`))) || history.body != "" && !bytes.Contains(body, []byte("File read")) {
					t.Fatalf("lost tool exchange %s", body)
				}
				if usage := ledger.wait(t); usage.Total == nil || *usage.Total != history.total {
					t.Fatalf("native usage %+v", usage)
				}
			}
		})
	}
}

func TestCodeChatToCodexRefusesUnsupportedRequiredState(t *testing.T) {
	for _, test := range []struct {
		name, body, query string
		headers           http.Header
	}{
		{"signed reasoning", `{"role":"assistant","content":"Earlier","reasoning_content":"signed"}`, "", nil},
		{"message name", `{"role":"user","name":"owner","content":"Hello"}`, "", nil},
		{"media", `{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://image.example/required"}}]}`, "", nil},
		{"role", `{"role":"custom","content":"Hello"}`, "", nil},
		{"query", `{"role":"user","content":"Hello"}`, "?required=true", nil},
		{"turn state", `{"role":"user","content":"Hello"}`, "", http.Header{"X-Codex-Turn-State": {"required"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _, server := newCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.ChatToCodex = true
			h.rt.release.Snapshot.CodeRoutes["coding"] = route
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions"+test.query, strings.NewReader(`{"model":"native-model","messages":[`+test.body+`],"stream":true}`))
			request.Header = test.headers.Clone()
			if request.Header == nil {
				request.Header = make(http.Header)
			}
			request.Header.Set("Authorization", "Bearer "+fullKey)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode < 400 || response.StatusCode >= 500 || h.mock.count("a") != 0 {
				t.Fatalf("required state dispatched, status %d", response.StatusCode)
			}
		})
	}
	for _, field := range []string{`"stream_options":{"include_usage":true,"required_state":true}`, `"reasoning_effort":"numeric"`, `"previous_response_id":"owned"`, `"tools":[{"type":"function","function":{"name":"read_file","strict":true,"parameters":{"type":"object"}}}]`} {
		h, _, server := newCodeForwardHarness(t)
		route := h.rt.release.Snapshot.CodeRoutes["coding"]
		route.ChatToCodex = true
		h.rt.release.Snapshot.CodeRoutes["coding"] = route
		if err := h.rt.release.Snapshot.Compile(); err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(`{"model":"native-model","messages":[{"role":"user","content":"Hello"}],`+field+`}`))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode < 400 || response.StatusCode >= 500 || h.mock.count("a") != 0 {
			t.Fatalf("required field dispatched, status %d, %s", response.StatusCode, field)
		}
	}
	t.Run("required nested tool choice", func(t *testing.T) {
		h, _, server := newCodeForwardHarness(t)
		route := h.rt.release.Snapshot.CodeRoutes["coding"]
		route.ChatToCodex = true
		h.rt.release.Snapshot.CodeRoutes["coding"] = route
		if err := h.rt.release.Snapshot.Compile(); err != nil {
			t.Fatal(err)
		}
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(`{"model":"native-model","messages":[{"role":"user","content":"Read main.go"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"read_file","required_state":"opaque"}}}`))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnprocessableEntity || h.mock.count("a") != 0 {
			t.Fatalf("required tool choice dispatched, status %d", response.StatusCode)
		}
	})
}

func TestCodeChatToCodexPreservesNativeUsageOnProjectionFailure(t *testing.T) {
	for _, known := range []bool{false, true} {
		h, ledger, server := newCodeForwardHarness(t)
		route := h.rt.release.Snapshot.CodeRoutes["coding"]
		route.ChatToCodex = true
		h.rt.release.Snapshot.CodeRoutes["coding"] = route
		if err := h.rt.release.Snapshot.Compile(); err != nil {
			t.Fatal(err)
		}
		h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			if known {
				fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"required-state","status":"completed","output":[{"type":"reasoning","encrypted_content":"required"}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
				return
			}
			fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"unknown-usage","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}]}}`+"\n\n")
		})
		request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(`{"model":"native-model","messages":[{"role":"user","content":"Hello"}]}`))
		request.Header.Set("Authorization", "Bearer "+fullKey)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		usage := ledger.wait(t)
		if known && (response.StatusCode != 502 || usage.Total == nil || *usage.Total != 3 || !bytes.Contains(body, []byte("code_upstream_invalid"))) || !known && (response.StatusCode != 200 || usage.Total != nil) {
			t.Fatalf("usage/projection mismatch known=%t status=%d usage=%+v body=%s", known, response.StatusCode, usage, body)
		}
	}
}

func TestCodeChatToCodexPortableRecoveryUsesExactModelPool(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t, codeForwardSetup{Models: []string{"gpt-fixture-main", "gpt-fixture-alternate"}, InitialRecoveryAttempts: 1, Policy: access.KeyPolicy{Scopes: []string{"inference"}}})
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.ChatToCodex, route.ModelRewriting = true, true
	route.ModelPools = map[string][]string{"gpt-fixture-main": {"gpt-fixture-main", "gpt-fixture-alternate"}}
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	ledger.pinTarget, ledger.recoveryTarget = "gpt-fixture-main", "gpt-fixture-alternate"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string
			Input []struct{ Content []struct{ Text string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Input) != 1 || request.Input[0].Content[0].Text != "Preserve this prompt" {
			t.Errorf("model recovery changed prompt %+v, %v", request, err)
		}
		if request.Model == "gpt-fixture-main" {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
			return
		}
		if request.Model != "gpt-fixture-alternate" {
			t.Errorf("unexpected selected native model %q", request.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"recovered-chat","model":"gpt-fixture-alternate","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Recovered"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	})
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(`{"model":"gpt-fixture-main","messages":[{"role":"user","content":"Preserve this prompt"}]}`))
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("X-Opencode-Session-Id", "portable-recovery")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !bytes.Contains(body, []byte(`"model":"gpt-fixture-main"`)) || !bytes.Contains(body, []byte("Recovered")) || h.mock.count("a") != 2 {
		t.Fatalf("Chat recovery %d %s", response.StatusCode, body)
	}
	ledger.wait(t)
	if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 2 {
		t.Fatalf("recovered native usage %+v", usage)
	}
}

func TestCodeChatToCodexPaidFallbackPreservesNativeChatContract(t *testing.T) {
	h, ledger, server := newPaidCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.PaidFallbackTargets, route.PaidTargets = route.PaidTargets, nil
	route.ChatToCodex, route.InitialRecoveryAttempts = true, 1
	ledger.recoveryUnavailable = true
	h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	providerKey := route.RevisionID + ":" + route.PaidFallbackTargets[modelA].ProviderID
	provider := h.rt.release.Snapshot.CodePaidConnections[providerKey]
	provider.ProfileID = "openai-chat"
	h.rt.release.Snapshot.CodePaidConnections[providerKey] = provider
	const body = `{"model":"model-a","messages":[{"role":"user","content":"hello"}],"max_completion_tokens":20}`
	const reply = `{ "id":"paid-chat", "model":"model-a", "choices":[{"index":0,"message":{"role":"assistant","content":"Paid reply"},"finish_reason":"stop"}], "usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}, "extension":{"keep":true} }`
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer upstream-only" {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
			return
		}
		received, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer "+secretA || !bytes.Equal(received, []byte(body)) {
			t.Errorf("paid fallback changed native Chat request %s", received)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	})
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+fullKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	output, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !bytes.Equal(output, []byte(reply)) || h.mock.count("a") != 2 {
		t.Fatalf("paid Chat fallback %d %s", response.StatusCode, output)
	}
	ledger.wait(t)
	if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
		t.Fatalf("paid Chat native usage %+v", usage)
	}
	envelope := h.sink.last(t)
	if len(envelope.Attempts) != 1 || envelope.Attempts[0].ProviderID != provider.ID || envelope.Attempts[0].SlotID != h.slotA || !envelope.Attempts[0].UsageComplete || envelope.Attempts[0].Price == nil {
		t.Fatalf("paid Chat accounting %+v", envelope)
	}
}

func TestCodePaidOnlyRoutePreservesMeteredDelivery(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			h, ledger, server := newPaidCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			route.InitialRecoveryAttempts = 1
			if fallback {
				route.PaidFallbackTargets = route.PaidTargets
				route.PaidTargets = nil
				ledger.subscriptionUnavailable = true
			} else {
				route.Models = append(route.Models, modelB)
				route.PaidFallbackTargets = map[string]codemode.PaidTarget{modelB: route.PaidTargets[modelA]}
			}
			h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
			h.rt.release.Snapshot.CodeConnections = nil
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			const reply = `{"id":"paid-only","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+secretA {
					t.Error("paid-only route lost its selected credential")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, reply)
			})
			response := codeDo(t, server, []byte(`{"model":"model-a","input":"hello","max_output_tokens":8}`), nil)
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != reply {
				t.Fatalf("paid-only delivery %d %s, %v", response.StatusCode, body, err)
			}
			if measured := ledger.wait(t); measured.Total == nil || *measured.Total != 3 {
				t.Fatalf("paid-only usage lost %+v", measured)
			}
			fact := h.sink.last(t).Attempts[0]
			if !fact.UsageComplete || fact.BillingUncertain || fact.Usage == nil || fact.Usage.InputTokens != 2 || fact.Usage.OutputTokens != 1 {
				t.Fatalf("paid-only accounting lost %+v", fact)
			}
		})
	}
}

func TestCodeTranslatedRedirectCannotBecomeCompletion(t *testing.T) {
	for _, antigravity := range []bool{false, true} {
		t.Run(fmt.Sprintf("antigravity=%t", antigravity), func(t *testing.T) {
			h, ledger, server := func() (*harness, *codeTestLedger, *httptest.Server) {
				if antigravity {
					return newCodeAntigravityHarness(t, true)
				}
				h, ledger, server := newCodeForwardHarness(t)
				route := h.rt.release.Snapshot.CodeRoutes["coding"]
				route.ChatToCodex = true
				h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
				if err := h.rt.release.Snapshot.Compile(); err != nil {
					t.Fatal(err)
				}
				return h, ledger, server
			}()
			model, reply := func() (string, string) {
				if antigravity {
					return "gemini-fixture", `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"unaccepted output"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}`
				}
				return "native-model", `{"id":"redirect","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`
			}()
			h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusMultipleChoices)
				_, _ = io.WriteString(w, reply)
			})
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+fullKey)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusMultipleChoices || !bytes.Contains(body, []byte("code_upstream_rejected")) {
				t.Fatalf("redirect became a completion %d %s, %v", response.StatusCode, body, err)
			}
			if measured := ledger.wait(t); measured.Total != nil || measured.Input != nil || measured.Output != nil {
				t.Fatalf("redirect became billable usage %+v", measured)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if ledger.health.Status == "healthy" || slices.ContainsFunc(ledger.outcomes, func(outcome codemode.Outcome) bool { return outcome.Kind == "completed" }) {
				t.Fatalf("redirect became successful health %+v outcomes %+v", ledger.health, ledger.outcomes)
			}
		})
	}
}

func TestCodeAntigravityTruncationRetainsUsageWithoutSuccess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, truncated := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/truncated=%t", stream, truncated), func(t *testing.T) {
				h, ledger, server := newCodeAntigravityHarness(t, true)
				reason, finish, outcome := func() (string, string, string) {
					if truncated {
						return "MAX_TOKENS", "length", "incomplete"
					}
					return "STOP", "stop", "completed"
				}()
				h.mock.set("a", func(w http.ResponseWriter, _ *http.Request) {
					body := fmt.Sprintf(`{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"partial answer"}]},"finishReason":%q}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}`, reason)
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprint(w, "data: "+body+"\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, body)
				})
				request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"gemini-fixture","messages":[{"role":"user","content":"hello"}],"stream":%t}`, stream)))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+fullKey)
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"finish_reason":"`+finish+`"`)) || !bytes.Contains(body, []byte("partial answer")) {
					t.Fatalf("truncation changed delivery %d %s, %v", response.StatusCode, body, err)
				}
				if measured := ledger.wait(t); measured.Total == nil || *measured.Total != 3 || measured.Input == nil || *measured.Input != 2 || measured.Output == nil || *measured.Output != 1 {
					t.Fatalf("truncated result lost measured usage %+v", measured)
				}
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				if !slices.ContainsFunc(ledger.outcomes, func(observed codemode.Outcome) bool { return observed.Origin == "upstream" && observed.Kind == outcome }) || (ledger.health.Status == "healthy") == truncated {
					t.Fatalf("completion evidence disagrees with truncation, health %+v outcomes %+v", ledger.health, ledger.outcomes)
				}
			})
		}
	}
}

func TestCodeUnaryResponseLimitPreservesDeliveryAndUsage(t *testing.T) {
	for _, scenario := range []string{"paid", "paid rejection", "alias", "translation", "antigravity"} {
		t.Run(scenario, func(t *testing.T) {
			h, ledger, server := func() (*harness, *codeTestLedger, *httptest.Server) {
				switch scenario {
				case "paid", "paid rejection":
					return newPaidCodeForwardHarness(t)
				case "antigravity":
					return newCodeAntigravityHarness(t, true)
				default:
					return newCodeForwardHarness(t)
				}
			}()
			path, requestBody, upstreamBody, expected := func() (string, string, string, string) {
				switch scenario {
				case "paid":
					return "/responses", `{"model":"model-a","input":"hello","max_output_tokens":8}`, `{"id":"bounded","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`, "bounded"
				case "paid rejection":
					return "/responses", `{"model":"model-a","input":"hello","max_output_tokens":8}`, `{"error":{"message":"permission denied secret-a"}}`, "permission denied"
				case "antigravity":
					return "/v1/chat/completions", `{"model":"gemini-fixture","messages":[{"role":"user","content":"hello"}]}`, `{"response":{"responseId":"bounded","candidates":[{"content":{"role":"model","parts":[{"text":"bounded answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}`, "bounded answer"
				case "translation":
					route := h.rt.release.Snapshot.CodeRoutes["coding"]
					route.ChatToCodex = true
					h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
					return "/v1/chat/completions", `{"model":"native-model","messages":[{"role":"user","content":"hello"}]}`, `{"id":"bounded","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"bounded answer"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`, "bounded answer"
				default:
					route := h.rt.release.Snapshot.CodeRoutes["coding"]
					route.ModelRewriting = true
					route.Aliases = map[string]string{"public-model": "native-model"}
					h.rt.release.Snapshot.CodeRoutes[route.Slug] = route
					return "/responses", `{"model":"public-model","input":"hello"}`, `{"id":"bounded","model":"native-model","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`, `"model":"public-model"`
				}
			}()
			if err := h.rt.release.Snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			padded := upstreamBody[:len(upstreamBody)-1] + strings.Repeat(" ", codewire.MaxBody-len(upstreamBody)) + "}"
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", fmt.Sprint(len(padded)))
				if scenario == "paid rejection" {
					w.WriteHeader(http.StatusBadRequest)
				}
				if _, err := io.WriteString(w, padded); err != nil {
					t.Errorf("fixture write failed %v", err)
				}
			})
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding"+path, strings.NewReader(requestBody))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+fullKey)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			status := func() int {
				if scenario == "paid rejection" {
					return http.StatusBadRequest
				}
				return http.StatusOK
			}()
			if err != nil || response.StatusCode != status || !json.Valid(body) || !bytes.Contains(body, []byte(expected)) || scenario == "paid rejection" && bytes.Contains(body, []byte(secretA)) {
				t.Fatalf("bounded response lost delivery, status %d bytes %d prefix %s, %v", response.StatusCode, len(body), body[:min(200, len(body))], err)
			}
			measured := ledger.wait(t)
			if scenario == "paid rejection" {
				if measured.Total != nil || measured.Input != nil || measured.Output != nil {
					t.Fatalf("rejection fabricated usage %+v", measured)
				}
				return
			}
			if measured.Total == nil || *measured.Total != 3 || measured.Input == nil || *measured.Input != 2 || measured.Output == nil || *measured.Output != 1 {
				t.Fatalf("bounded response lost usage %+v", measured)
			}
			if scenario == "paid" {
				fact := h.sink.last(t).Attempts[0]
				if !fact.UsageComplete || fact.BillingUncertain || fact.Usage == nil || fact.Usage.InputTokens != 2 || fact.Usage.OutputTokens != 1 {
					t.Fatalf("bounded response lost paid accounting %+v", fact)
				}
			}
		})
	}
}
