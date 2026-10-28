package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	testProject  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50701"
	testServerID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50702"
	testServerRv = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50703"
	testToolset  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50704"
	testRevision = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50705"
	testOtherTS  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50706"
	testOtherRv  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50707"
	testCredID   = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50708"
	testKeyID    = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50709"
	testGenID    = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50710"
)

type timeline struct {
	mu     sync.Mutex
	events []string
}

func (t *timeline) add(event string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, event)
}

func (t *timeline) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string{}, t.events...)
}

func (t *timeline) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = nil
}

var testToolSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)

func testEgress() *egress.Policy {
	return &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
}

type fakeRuntime struct {
	authority      *access.Authority
	otherAuthority *access.Authority
	release        *runtime.Release
	failAuthzOn    int
	authzCalls     atomic.Int32
}

func (f *fakeRuntime) Authenticate(secret string) (*access.Authority, error) {
	if secret == "other-key" && f.otherAuthority != nil {
		return f.otherAuthority, nil
	}
	if secret != "test-key" || f.authority == nil {
		return nil, errors.New("invalid key")
	}
	return f.authority, nil
}

func (f *fakeRuntime) Release() *runtime.Release { return f.release }

func (f *fakeRuntime) AuthorizeAgentResource(a access.Authority, scope, id, revision, project string) error {
	if int(f.authzCalls.Add(1)) == f.failAuthzOn {
		return access.Forbidden()
	}
	if !a.AllowsResource(scope, id, project, time.Now()) {
		return access.Forbidden()
	}
	live := map[string]string{testToolset: testRevision, testOtherTS: testOtherRv}
	if live[id] != revision || project != testProject {
		return access.Forbidden()
	}
	return nil
}

type fakeLedger struct {
	tl         *timeline
	reserveErr error
	settleErr  error
	evidence   []AttemptEvidence
	reserved   []ReserveInput
}

func (f *fakeLedger) Reserve(_ context.Context, in ReserveInput) (*Reservation, error) {
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	f.tl.add("reserve")
	f.reserved = append(f.reserved, in)
	return &Reservation{DispatchID: testGenID, ClaimID: testGenID, RunID: testGenID, StepID: testGenID, AttemptID: testRevision}, nil
}

func (f *fakeLedger) MarkDispatched(context.Context, *Reservation) error {
	f.tl.add("dispatched")
	return nil
}

func (f *fakeLedger) ReleaseUndispatched(context.Context, *Reservation) error {
	f.tl.add("released")
	return nil
}

func (f *fakeLedger) MarkUnknown(_ context.Context, _ *Reservation, evidence AttemptEvidence) error {
	f.tl.add("unknown")
	f.evidence = append(f.evidence, evidence)
	return nil
}

func (f *fakeLedger) Settle(_ context.Context, _ *Reservation, evidence AttemptEvidence) error {
	if f.settleErr != nil {
		return f.settleErr
	}
	f.tl.add("settled")
	f.evidence = append(f.evidence, evidence)
	return nil
}

type fixture struct {
	server      *httptest.Server
	calls       atomic.Int32
	lists       atomic.Int32
	mode        string
	description atomic.Value
	notify      atomic.Bool
}

func (f *fixture) setDescription(description string) { f.description.Store(description) }

func (f *fixture) catalogDescription() string {
	if value, ok := f.description.Load().(string); ok && value != "" {
		return value
	}
	return "Look up"
}

func newFixture(t *testing.T, tl *timeline, mode string) *fixture {
	t.Helper()
	fx := &fixture{mode: mode}
	fx.description.Store("Look up")
	fx.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			tl.add("initialize")
			if fx.mode == "dial_fail" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Mcp-Session-Id", "fixture-session")
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			fx.lists.Add(1)
			tl.add("list")
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "description": fx.catalogDescription(), "inputSchema": testToolSchema}}}
		case "tools/call":
			if r.Header.Get("Authorization") != "Bearer "+"static-token" {
				t.Errorf("upstream did not receive the registered static credential: %q", r.Header.Get("Authorization"))
			}
			fx.calls.Add(1)
			tl.add("upstream")
			if fx.mode == "drop" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &params)
			result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "answer for " + params.Name}},
				"isError": fx.mode == "tool_error",
			}
			if fx.notify.Load() {
				note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed"})
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: message\ndata: " + string(note) + "\n\n"))
				_, _ = w.Write([]byte("event: message\ndata: " + string(body) + "\n\n"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(fx.server.Close)
	return fx
}

func testSnapshot(t *testing.T, endpoint string) *routing.Snapshot {
	t.Helper()
	digest := strings.Repeat("a", 64)
	description := "Look up"
	schema := testToolSchema
	if catalog, err := mcpservers.Certify(t.Context(), testEgress(), endpoint, []byte("static-token")); err == nil && len(catalog.Tools) == 1 {
		digest = catalog.Digest
		description = catalog.Tools[0].Description
		schema = catalog.Tools[0].InputSchema
	}
	cred := testCredID
	toolset := func(id, revision string) agentcontract.Revision {
		tool := agentcontract.Tool{ServerID: testServerID, RevisionID: testServerRv, CatalogDigest: digest, Name: "lookup", Alias: agentcontract.ToolAlias(testServerID, "lookup")}
		draft := agentcontract.Draft{ProjectID: testProject, Name: "tools", Toolset: &agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}
		return agentcontract.Revision{ID: revision, PublicationID: id, ProjectID: testProject, Kind: "toolset", Digest: agentcontract.DraftDigest(draft), Definition: draft, Current: true}
	}
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: testGenID},
		Providers:  map[string]routing.Provider{}, Routes: map[string]routing.Route{},
		MCPServers:        []routing.MCPServer{{ID: testServerID, ProjectID: testProject, RevisionID: testServerRv, Endpoint: endpoint, Protocol: "2025-11-25", Digest: digest, CredentialID: &cred, Tools: []routing.MCPTool{{Name: "lookup", Description: description, InputSchema: schema}}}},
		AgentPublications: []agentcontract.Revision{toolset(testToolset, testRevision), toolset(testOtherTS, testOtherRv)},
	}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func testAuthority(scopes []string, toolsets []string) *access.Authority {
	project := testProject
	return &access.Authority{ID: testKeyID, InstallationID: "test-installation", ProjectID: &project, Policy: access.KeyPolicy{Scopes: scopes, AllowedToolsets: toolsets}}
}

func testAuthKey() *secrets.AuthKey {
	return secrets.NewAuthKey(bytes.Repeat([]byte{9}, 32), "test-installation")
}

func newTestServer(t *testing.T, authority *access.Authority, mode string) (*Server, *fixture, *fakeLedger, *timeline, *fakeRuntime) {
	t.Helper()
	tl := &timeline{}
	fx := newFixture(t, tl, mode)
	rt := &fakeRuntime{authority: authority, release: &runtime.Release{ID: testGenID, Snapshot: testSnapshot(t, fx.server.URL)}}
	ledger := &fakeLedger{tl: tl}
	server := &Server{
		Runtime: rt, Ledger: ledger, Auth: testAuthKey(),
		Egress: testEgress(),
		Credentials: func(_ context.Context, id string) ([]byte, error) {
			if id != testCredID {
				return nil, errors.New("unknown credential")
			}
			return []byte("static-token"), nil
		},
	}
	sessions, err := NewSessionStore(newFakeSessionCommander(func() time.Time { return server.now() }), "test:mcp")
	if err != nil {
		t.Fatal(err)
	}
	server.Sessions = sessions
	tl.reset()
	return server, fx, ledger, tl, rt
}

func request(t *testing.T, server *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	return requestFrom(t, server, method, path, body, "", "")
}

func requestFrom(t *testing.T, server *Server, method, path, body, remoteAddr, forwardedFor string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	server.Register(mux)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not JSON: %s", rec.Body.String())
		}
	}
	return rec.Code, out
}

func alias() string { return agentcontract.ToolAlias(testServerID, "lookup") }

func TestListToolsReturnsOnlyAllowedToolsetsUnderAliases(t *testing.T) {
	server, _, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	status, body := request(t, server, http.MethodGet, "/v1/mcp/tools", "")
	if status != http.StatusOK {
		t.Fatalf("list status %d: %v", status, body)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("list returned tools outside the allowlist: %v", body)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != alias() || tool["toolset_id"] != testToolset || tool["description"] != "Look up" {
		t.Fatalf("listed tool is not the pinned alias: %v", tool)
	}
	if _, ok := tool["input_schema"].(map[string]any); !ok {
		t.Fatalf("listed tool lost its pinned schema: %v", tool)
	}

	status, _ = request(t, server, http.MethodGet, "/v1/mcp/tools?toolset_id="+testOtherTS, "")
	if status != http.StatusNotFound {
		t.Fatalf("unallowed toolset listed with status %d", status)
	}
	status, _ = request(t, server, http.MethodGet, "/v1/mcp/tools?toolset_id=0192cf87-d4ab-7f2e-a8b1-c2d3e4f50799", "")
	if status != http.StatusNotFound {
		t.Fatalf("foreign toolset did not behave like a missing one: %d", status)
	}
}

func TestListToolsEmptyAllowlistReturnsNothing(t *testing.T) {
	server, fx, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, nil), "")
	status, body := request(t, server, http.MethodGet, "/v1/mcp/tools", "")
	tools, _ := body["tools"].([]any)
	if status != http.StatusOK || len(tools) != 0 || fx.calls.Load() != 0 {
		t.Fatalf("empty allowlist listed tools: %d %v", status, body)
	}
	server, _, _, _, _ = newTestServer(t, testAuthority([]string{"inference"}, []string{testToolset}), "")
	status, _ = request(t, server, http.MethodGet, "/v1/mcp/tools", "")
	if status != http.StatusForbidden {
		t.Fatalf("missing tools scope listed tools: %d", status)
	}
}

func TestCallToolReservesDispatchesAndSettlesExactlyOnce(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"secret-argument"}}`)
	if status != http.StatusOK {
		t.Fatalf("call status %d: %v", status, body)
	}
	content, _ := body["content"].([]any)
	if len(content) != 1 || fx.calls.Load() != 1 {
		t.Fatalf("call did not reach the upstream exactly once: calls=%d body=%v", fx.calls.Load(), body)
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,list,dispatched,upstream,settled" {
		t.Fatalf("debit did not fence dispatch and settle after it: %s", got)
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusSucceeded {
		t.Fatalf("settlement evidence missing: %+v", ledger.evidence)
	}
	bare := sha256.Sum256([]byte(`{"query":"secret-argument"}`))
	if ledger.reserved[0].InputDigest == hex.EncodeToString(bare[:]) {
		t.Fatal("input digest is a bare hash of the raw arguments")
	}
	if want := hex.EncodeToString(testAuthKey().Digest(secrets.ToolInputDigest, `{"query":"secret-argument"}`)); ledger.reserved[0].InputDigest != want {
		t.Fatalf("input digest is not the keyed purpose-tagged digest: %q", ledger.reserved[0].InputDigest)
	}
	evidence := ledger.evidence[0]
	for _, digest := range []string{ledger.reserved[0].InputDigest, evidence.ResultDigest} {
		if len(digest) != 64 || strings.Contains(digest, "secret-argument") || strings.Contains(digest, "answer") {
			t.Fatalf("attempt record carries content instead of a digest: %q", digest)
		}
	}
	if ledger.reserved[0].ToolAlias != alias() || body["attempt_id"] == nil || body["dispatch_id"] == nil {
		t.Fatalf("call lost its attempt identity: reserved=%+v body=%v", ledger.reserved, body)
	}
}

func TestCallToolInvalidArgumentsRefuseBeforeAnyReservation(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":3}}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid arguments status %d: %v", status, body)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 || len(tl.snapshot()) != 0 {
		t.Fatalf("invalid arguments reserved or dispatched: calls=%d events=%v", fx.calls.Load(), tl.snapshot())
	}
}

func TestCallToolForeignToolsetAndUnknownAliasNeverDispatch(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	for _, body := range []string{
		`{"toolset_id":"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50799","name":"` + alias() + `","arguments":{"query":"x"}}`,
		`{"toolset_id":"` + testOtherTS + `","name":"` + alias() + `","arguments":{"query":"x"}}`,
		`{"toolset_id":"` + testToolset + `","name":"lookup","arguments":{"query":"x"}}`,
	} {
		status, _ := request(t, server, http.MethodPost, "/v1/mcp/tools/call", body)
		if status != http.StatusNotFound {
			t.Fatalf("foreign or unpinned call status %d for %s", status, body)
		}
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("refused call dispatched: calls=%d reserved=%d", fx.calls.Load(), len(ledger.reserved))
	}
}

func TestCallToolAmbiguousUpstreamFailureStaysUnknown(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "drop")
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusBadGateway || fx.calls.Load() != 1 {
		t.Fatalf("ambiguous failure status %d calls=%d: %v", status, fx.calls.Load(), body)
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,list,dispatched,upstream,unknown" {
		t.Fatalf("ambiguous dispatch settled or released: %s", got)
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusOutcomeUnknown {
		t.Fatalf("unknown attempt not recorded: %+v", ledger.evidence)
	}
}

func TestCallToolUpstreamToolErrorSettlesAsObservedAttempt(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "tool_error")
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusOK || body["is_error"] != true || fx.calls.Load() != 1 {
		t.Fatalf("tool error not returned as an observed result: %d %v", status, body)
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,list,dispatched,upstream,settled" {
		t.Fatalf("tool error attempt did not settle: %s", got)
	}
	if ledger.evidence[0].Status != StatusUpstreamError || !ledger.evidence[0].IsError {
		t.Fatalf("tool error evidence wrong: %+v", ledger.evidence)
	}
}

func TestCallToolReleasesReservationWhenAuthorityIsRevokedBeforeDispatch(t *testing.T) {
	server, fx, _, tl, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	rt.failAuthzOn = 2
	status, _ := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusForbidden || fx.calls.Load() != 0 {
		t.Fatalf("revoked call dispatched: status=%d calls=%d", status, fx.calls.Load())
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,released" {
		t.Fatalf("undispatched reservation was not released: %s", got)
	}
}

func TestCallToolBudgetRefusalNeverDispatches(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	ledger.reserveErr = &limits.ExceededError{Dimension: limits.DimensionDailyCost, RetryAfter: time.Minute}
	status, _ := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusTooManyRequests || fx.calls.Load() != 0 {
		t.Fatalf("budget refusal dispatched: status=%d calls=%d", status, fx.calls.Load())
	}
}

func TestCallToolDialFailureReleasesReservation(t *testing.T) {
	server, fx, _, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "dial_fail")
	status, _ := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusBadGateway || fx.calls.Load() != 0 {
		t.Fatalf("failed session setup dispatched a tool call: status=%d calls=%d", status, fx.calls.Load())
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,released" {
		t.Fatalf("session setup did not follow reservation or its failure did not release it: %s", got)
	}
}

func TestCallToolSettleFailureStaysUnknown(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	ledger.settleErr = errors.New("settle failed")
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusBadGateway || fx.calls.Load() != 1 || body["status"] != string(StatusOutcomeUnknown) {
		t.Fatalf("unsettled dispatch was not reported unknown: status=%d calls=%d body=%v", status, fx.calls.Load(), body)
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,list,dispatched,upstream,unknown" {
		t.Fatalf("unsettled dispatch settled or released: %s", got)
	}
}

func TestToolEndpointsEnforceKeyCIDRsWithTrustedProxies(t *testing.T) {
	authority := testAuthority([]string{"tools"}, []string{testToolset})
	authority.Policy.AllowedCIDRs = []string{"10.0.0.0/8"}
	server, fx, ledger, tl, _ := newTestServer(t, authority, "")
	status, body := requestFrom(t, server, http.MethodGet, "/v1/mcp/tools", "", "192.0.2.10:1234", "")
	if status != http.StatusForbidden {
		t.Fatalf("list from a disallowed address succeeded: %d %v", status, body)
	}
	status, _ = requestFrom(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`, "192.0.2.10:1234", "10.1.2.3")
	if status != http.StatusForbidden || fx.calls.Load() != 0 || len(ledger.reserved) != 0 || len(tl.snapshot()) != 0 {
		t.Fatalf("call spoofing a forwarded address through an untrusted proxy dispatched: status=%d calls=%d events=%v", status, fx.calls.Load(), tl.snapshot())
	}
	server.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	status, _ = requestFrom(t, server, http.MethodGet, "/v1/mcp/tools", "", "192.0.2.10:1234", "10.1.2.3")
	if status != http.StatusOK {
		t.Fatalf("list from an allowed address behind a trusted proxy refused: %d", status)
	}
	status, _ = requestFrom(t, server, http.MethodGet, "/v1/mcp/tools", "", "192.0.2.10:1234", "203.0.113.9")
	if status != http.StatusForbidden {
		t.Fatalf("list from a disallowed forwarded address succeeded: %d", status)
	}
}

func jsonErrorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, _ := body["error"].(map[string]any)
	if failure == nil {
		t.Fatalf("response has no error: %v", body)
	}
	code, _ := failure["code"].(string)
	return code
}

func jsonErrorMessage(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, _ := body["error"].(map[string]any)
	if failure == nil {
		t.Fatalf("response has no error: %v", body)
	}
	message, _ := failure["message"].(string)
	return message
}

func driftedSnapshot(t *testing.T, endpoint string) *routing.Snapshot {
	t.Helper()
	snapshot := testSnapshot(t, endpoint)
	snapshot.MCPServers[0].Digest = strings.Repeat("b", 64)
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCallToolDriftedLiveCatalogRefusesWithoutDispatch(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	fx.setDescription("Changed after publication")
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusConflict || jsonErrorCode(t, body) != "tool_definition_changed" {
		t.Fatalf("drifted catalog call was not refused as a definition change: %d %v", status, body)
	}
	if message := jsonErrorMessage(t, body); !strings.Contains(message, "pinned") || strings.Contains(message, "behavior") {
		t.Fatalf("drift refusal misstates what was checked: %q", message)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("drift check invoked the tool: calls=%d", fx.calls.Load())
	}
	if got := strings.Join(tl.snapshot(), ","); got != "reserve,initialize,list,released" {
		t.Fatalf("drifted call dispatched or kept its reservation: %s", got)
	}
	if len(ledger.evidence) != 0 {
		t.Fatalf("drifted call settled an attempt: %+v", ledger.evidence)
	}
	status, body = request(t, server, http.MethodGet, "/v1/mcp/tools?toolset_id="+testToolset, "")
	tools, _ := body["tools"].([]any)
	if status != http.StatusOK || len(tools) != 1 {
		t.Fatalf("pinned definition stopped being readable while drifted: %d %v", status, body)
	}
}

func TestCallToolPinDriftedFromSnapshotRefusesWithDefinitionChanged(t *testing.T) {
	server, fx, ledger, tl, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	rt.release = &runtime.Release{ID: testGenID, Snapshot: driftedSnapshot(t, fx.server.URL)}
	tl.reset()
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusConflict || jsonErrorCode(t, body) != "tool_definition_changed" {
		t.Fatalf("snapshot drifted pin did not refuse as a definition change: %d %v", status, body)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 || len(tl.snapshot()) != 0 {
		t.Fatalf("snapshot drifted call reserved or dispatched: calls=%d events=%v", fx.calls.Load(), tl.snapshot())
	}
	status, body = request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"lookup","arguments":{"query":"x"}}`)
	if status != http.StatusNotFound || jsonErrorCode(t, body) != "tool_not_found" {
		t.Fatalf("unpinned name masqueraded as drift: %d %v", status, body)
	}
}

func TestCallToolUnliveDriftedToolsetReportsDriftOnlyToAllowlistedCallers(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	rt.release = &runtime.Release{ID: testGenID, Snapshot: driftedSnapshot(t, fx.server.URL)}
	rt.failAuthzOn = 1
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusConflict || jsonErrorCode(t, body) != "tool_definition_changed" {
		t.Fatalf("allowlisted caller did not learn the definition changed: %d %v", status, body)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("unlive drifted call dispatched: calls=%d", fx.calls.Load())
	}

	other, otherFixture, _, _, otherRuntime := newTestServer(t, testAuthority([]string{"tools"}, []string{testOtherTS}), "")
	otherRuntime.release = &runtime.Release{ID: testGenID, Snapshot: driftedSnapshot(t, otherFixture.server.URL)}
	status, body = request(t, other, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusNotFound || jsonErrorCode(t, body) != "toolset_not_found" {
		t.Fatalf("caller outside the allowlist learned about drift: %d %v", status, body)
	}
	if otherFixture.calls.Load() != 0 {
		t.Fatalf("foreign drifted call dispatched: calls=%d", otherFixture.calls.Load())
	}
}
