package mcp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	testProject   = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50701"
	testServerID  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50702"
	testServerRv  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50703"
	testToolset   = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50704"
	testRevision  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50705"
	testOtherTS   = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50706"
	testOtherRv   = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50707"
	testCredID    = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50708"
	testKeyID     = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50709"
	testGenID     = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50710"
	testServer2   = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50713"
	testServer2R  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50714"
	testServerRv2 = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50715"
	testRevision2 = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50716"
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
	denyResource   bool
	authzCalls     atomic.Int32
	live           map[string]string
	revoked        atomic.Bool
	stale          atomic.Bool
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

func (f *fakeRuntime) AuthorizeMCPTool(a access.Authority, id, revision, project, server, serverRevision string) error {
	if int(f.authzCalls.Add(1)) == f.failAuthzOn {
		return access.Forbidden()
	}
	if f.stale.Load() {
		return runtime.ErrStaleAuthority
	}
	if f.revoked.Load() || !a.AllowsResource("tools", id, project, time.Now()) {
		return access.Forbidden()
	}
	return nil
}

func (f *fakeRuntime) AuthorizeAgentResource(a access.Authority, scope, id, revision, project string) error {
	if int(f.authzCalls.Add(1)) == f.failAuthzOn {
		return access.Forbidden()
	}
	if f.stale.Load() {
		return runtime.ErrStaleAuthority
	}
	if f.revoked.Load() {
		return access.Forbidden()
	}
	if !a.AllowsResource(scope, id, project, time.Now()) {
		return access.Forbidden()
	}
	live := f.live
	if live == nil {
		live = map[string]string{testToolset: testRevision, testOtherTS: testOtherRv}
	}
	if f.denyResource || live[id] != revision || project != testProject {
		return access.Forbidden()
	}
	return nil
}

type fakeLedger struct {
	tl         *timeline
	reserveErr error
	settleErr  error
	repeat     *RetainedCall
	evidence   []AttemptEvidence
	reserved   []ReserveInput
	sequence   int
}

func (f *fakeLedger) Reserve(_ context.Context, in ReserveInput) (ReserveResult, error) {
	if f.reserveErr != nil {
		return ReserveResult{}, f.reserveErr
	}
	if f.repeat != nil {
		return ReserveResult{Repeat: f.repeat}, nil
	}
	f.tl.add("reserve")
	f.reserved = append(f.reserved, in)
	f.sequence++
	suffix := strconv.Itoa(7000 + f.sequence)
	id := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f5" + suffix
	return ReserveResult{Reservation: &Reservation{DispatchID: id, ClaimID: testGenID, RunID: testGenID, StepID: testGenID, AttemptID: id}}, nil
}

func (f *fakeLedger) MarkDispatched(context.Context, *Reservation, AttemptEvidence) error {
	f.tl.add("dispatched")
	return nil
}

func (f *fakeLedger) ReleaseUndispatched(context.Context, *Reservation) error {
	f.tl.add("released")
	return nil
}

func (f *fakeLedger) RefuseUndispatched(_ context.Context, _ *Reservation, evidence AttemptEvidence) error {
	f.tl.add("refused")
	f.evidence = append(f.evidence, evidence)
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

func retainedFrom(t *testing.T, body map[string]any, status Status) *RetainedCall {
	t.Helper()
	attempt, _ := body["attempt_id"].(string)
	dispatch, _ := body["dispatch_id"].(string)
	if attempt == "" || dispatch == "" {
		t.Fatalf("response carries no attempt identity to retain: %v", body)
	}
	return &RetainedCall{AttemptID: attempt, DispatchID: dispatch, Status: status}
}

type fixture struct {
	server      *httptest.Server
	calls       atomic.Int32
	lists       atomic.Int32
	mode        atomic.Value
	description atomic.Value
	notify      atomic.Bool
	lastArgs    atomic.Value
}

func (f *fixture) receivedArgs() string {
	value, _ := f.lastArgs.Load().(string)
	return value
}

func (f *fixture) setMode(mode string) { f.mode.Store(mode) }

func (f *fixture) currentMode() string {
	mode, _ := f.mode.Load().(string)
	return mode
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
	fx := &fixture{}
	fx.setMode(mode)
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
			if fx.currentMode() == "dial_fail" {
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
			if fx.currentMode() == "drop" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &params)
			fx.lastArgs.Store(string(params.Arguments))
			content := []any{map[string]any{"type": "text", "text": "answer for " + params.Name}}
			switch fx.currentMode() {
			case "echo":
				content = []any{map[string]any{"type": "text", "text": "echo:" + string(params.Arguments)}}
			case "secret_result":
				content = []any{map[string]any{"type": "text", "text": "the private result"}}
			case "large_result":
				content = []any{map[string]any{"type": "text", "text": strings.Repeat("x", 256<<10)}}
			case "image_result":
				content = []any{map[string]any{"type": "image", "data": "aGVsbG8=", "mimeType": "image/png"}}
			}
			result = map[string]any{
				"content": content,
				"isError": fx.currentMode() == "tool_error",
			}
			if fx.currentMode() == "structured_result" {
				result = map[string]any{"content": content, "structuredContent": json.RawMessage(`{"nested":[{"message":"pri\u0076ate result"}]}`)}
			}
			switch fx.currentMode() {
			case "missing-result-content":
				result = json.RawMessage(`{}`)
			case "missing-content-kind":
				result = json.RawMessage(`{"content":[{}]}`)
			case "missing-text":
				result = json.RawMessage(`{"content":[{"type":"text"}]}`)
			case "embedded-resource":
				result = json.RawMessage(`{"content":[{"type":"resource","resource":{"uri":"https://resource.example.test","text":"retained resource"}}]}`)
			case "empty-text":
				result = json.RawMessage(`{"content":[{"type":"text","text":""}]}`)
			case "empty-content":
				result = json.RawMessage(`{"content":[]}`)
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
	catalog, err := mcpservers.Certify(t.Context(), testEgress(), endpoint, []byte("static-token"))
	if err != nil || len(catalog.Tools) != 1 {
		t.Fatalf("fixture catalog did not certify: err=%v catalog=%+v", err, catalog)
	}
	digest := catalog.Digest
	description := catalog.Tools[0].Description
	schema := catalog.Tools[0].InputSchema
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
	fx := newFixture(t, tl, "")
	rt := &fakeRuntime{authority: authority, release: &runtime.Release{ID: testGenID, Snapshot: testSnapshot(t, fx.server.URL)}}
	fx.setMode(mode)
	ledger := &fakeLedger{tl: tl}
	server := &Server{
		Runtime: rt, Ledger: ledger, Auth: testAuthKey(), Limiter: &fakeRateLimiter{},
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
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-1","arguments":{"query":"secret-argument"}}`)
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

type fakeRateLimiter struct {
	requests []limits.Request
	err      error
	failOn   int
}

func (f *fakeRateLimiter) Reserve(_ context.Context, request limits.Request) (*limits.Lease, error) {
	f.requests = append(f.requests, request)
	if f.err != nil && (f.failOn == 0 || f.failOn == len(f.requests)) {
		return nil, f.err
	}
	return nil, nil
}

type countingRateLimiter struct {
	counts map[string]int64
}

func (f *countingRateLimiter) Reserve(_ context.Context, request limits.Request) (*limits.Lease, error) {
	if request.RequestsPerMinute != nil && f.counts[request.LookupID] >= *request.RequestsPerMinute {
		return nil, &limits.ExceededError{Dimension: limits.DimensionRequests, RetryAfter: time.Minute}
	}
	f.counts[request.LookupID]++
	return nil, nil
}

func priceTestSnapshot(t *testing.T, rt *fakeRuntime, serverPrice *string, toolPrices map[string]string) {
	t.Helper()
	snapshot := rt.release.Snapshot
	snapshot.MCPServers[0].UnitPrice = serverPrice
	snapshot.MCPServers[0].ToolPrices = toolPrices
	snapshot.MCPServers[0].CallsPerMinute = 120
	snapshot.MCPServers[0].MaxConcurrency = 4
	snapshot.MCPServers[0].PerKeyCallsPerMinute = 30
	snapshot.MCPServers[0].PerKeyMaxConcurrency = 2
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
}

func TestCallToolReservesPinnedPriceAndRateLimits(t *testing.T) {
	authority := testAuthority([]string{"tools"}, []string{testToolset})
	keyRate, keyConcurrency := int64(10), int64(3)
	authority.Policy.RequestsPerMinute = &keyRate
	authority.Policy.MaxConcurrency = &keyConcurrency
	server, fx, ledger, _, rt := newTestServer(t, authority, "")
	serverPrice := "0.10"
	priceTestSnapshot(t, rt, &serverPrice, map[string]string{"lookup": "0.25"})
	limiter := &fakeRateLimiter{}
	server.Limiter = limiter
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"priced-json-1","arguments":{"query":"x"}}`)
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("priced call status %d calls=%d: %v", status, fx.calls.Load(), body)
	}
	if len(ledger.reserved) != 1 || ledger.reserved[0].UnitPrice == nil || *ledger.reserved[0].UnitPrice != "0.25" {
		t.Fatalf("reservation did not carry the pinned per-tool price: %+v", ledger.reserved)
	}
	if len(limiter.requests) != 2 {
		t.Fatalf("call did not reserve per-key and per-server rate limits: %+v", limiter.requests)
	}
	perKey, perServer := limiter.requests[0], limiter.requests[1]
	if perKey.CostOwnerID != testKeyID || perKey.RequestsPerMinute == nil || *perKey.RequestsPerMinute != keyRate ||
		perKey.MaxConcurrency == nil || *perKey.MaxConcurrency != 2 {
		t.Fatalf("per-key reservation did not take the tighter of key and server limits: %+v", perKey)
	}
	if perServer.CostOwnerID != testServerID || perServer.RequestsPerMinute == nil || *perServer.RequestsPerMinute != 120 ||
		perServer.MaxConcurrency == nil || *perServer.MaxConcurrency != 4 {
		t.Fatalf("per-server reservation lost the server limits: %+v", perServer)
	}
	if perKey.LookupID == perServer.LookupID || perKey.LookupID == "" || perServer.LookupID == "" {
		t.Fatalf("rate counters are not separated per scope: %q %q", perKey.LookupID, perServer.LookupID)
	}
}

func TestCallToolRateLimitRefusalReservesAndDispatchesNothing(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	server.Limiter = &fakeRateLimiter{err: &limits.ExceededError{Dimension: limits.DimensionRequests, RetryAfter: time.Minute}}
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"limited-json-1","arguments":{"query":"x"}}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("rate limited call status %d: %v", status, body)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("rate limited call reserved or dispatched: calls=%d reserved=%d", fx.calls.Load(), len(ledger.reserved))
	}
	limiter := &fakeRateLimiter{err: &limits.ExceededError{Dimension: limits.DimensionConcurrency, RetryAfter: time.Second}, failOn: 2}
	server.Limiter = limiter
	status, _ = request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"limited-json-2","arguments":{"query":"x"}}`)
	if status != http.StatusTooManyRequests || fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("concurrency limited call dispatched: status=%d calls=%d reserved=%d", status, fx.calls.Load(), len(ledger.reserved))
	}
}

func TestDelegatedToolCallsShareAgentRateLimitAcrossKeys(t *testing.T) {
	agentRate := int64(1)
	agent := &access.DelegationAgentLimits{ID: testOtherRv, LookupID: "agent-lookup", Limits: access.AdmissionLimits{RequestsPerMinute: &agentRate}}
	first := testAuthority([]string{"tools"}, []string{testToolset})
	first.DelegationAgentID, first.DelegationAgentLimits = agent.ID, agent
	second := testAuthority([]string{"tools"}, []string{testToolset})
	second.ID = testOtherTS
	second.DelegationAgentID, second.DelegationAgentLimits = agent.ID, agent
	server, upstream, ledger, _, runtime := newTestServer(t, first, "")
	runtime.otherAuthority = second
	server.Limiter = &countingRateLimiter{counts: map[string]int64{}}
	mux := http.NewServeMux()
	server.Register(mux)
	call := func(token, callID string) int {
		t.Helper()
		body := `{"toolset_id":"` + testToolset + `","name":"` + alias() + `","call_id":"` + callID + `","arguments":{"query":"x"}}`
		request := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		return response.Code
	}
	if status := call("test-key", "first"); status != http.StatusOK {
		t.Fatalf("first delegated tool call: %d", status)
	}
	if status := call("other-key", "second"); status != http.StatusTooManyRequests || upstream.calls.Load() != 1 || len(ledger.reserved) != 1 {
		t.Fatalf("agent rate limit did not stop second key: status=%d upstream=%d reservations=%d", status, upstream.calls.Load(), len(ledger.reserved))
	}
}

func TestCallToolMissingLimiterFailsClosed(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	server.Limiter = nil
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"missing-limiter-1","arguments":{"query":"x"}}`)
	if status != http.StatusServiceUnavailable || fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("missing limiter dispatched: status=%d calls=%d reserved=%d body=%v", status, fx.calls.Load(), len(ledger.reserved), body)
	}
	if body["error"].(map[string]any)["code"] != "limits_unavailable" {
		t.Fatalf("missing limiter refusal is not limits_unavailable: %v", body)
	}
}

func TestCallToolUnpricedAgainstCeilingRefusesWithoutDispatch(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	ledger.reserveErr = ErrUnpricedToolCall
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"unpriced-json-1","arguments":{"query":"x"}}`)
	if status != http.StatusUnprocessableEntity || fx.calls.Load() != 0 {
		t.Fatalf("unpriced call status %d calls=%d: %v", status, fx.calls.Load(), body)
	}
	problem, _ := body["error"].(map[string]any)
	if problem["code"] != "tool_unpriced" {
		t.Fatalf("unpriced refusal is not typed: %v", body)
	}
}

func TestListingToolsReservesNoDebitAndNoRateLimit(t *testing.T) {
	server, _, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	limiter := &fakeRateLimiter{}
	server.Limiter = limiter
	status, _ := request(t, server, http.MethodGet, "/v1/mcp/tools", "")
	if status != http.StatusOK {
		t.Fatalf("list status %d", status)
	}
	if len(ledger.reserved) != 0 || len(limiter.requests) != 0 {
		t.Fatalf("discovery reserved money or rate: reserved=%d rates=%d", len(ledger.reserved), len(limiter.requests))
	}
}

func TestCallToolInvalidArgumentsRefuseBeforeAnyReservation(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-2","arguments":{"query":3}}`)
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
		`{"toolset_id":"0192cf87-d4ab-7f2e-a8b1-c2d3e4f50799","name":"` + alias() + `","call_id":"existing-json-3","arguments":{"query":"x"}}`,
		`{"toolset_id":"` + testOtherTS + `","name":"` + alias() + `","call_id":"existing-json-4","arguments":{"query":"x"}}`,
		`{"toolset_id":"` + testToolset + `","name":"lookup","call_id":"existing-json-5","arguments":{"query":"x"}}`,
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
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-6","arguments":{"query":"x"}}`)
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
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-7","arguments":{"query":"x"}}`)
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
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-8","arguments":{"query":"x"}}`)
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
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-9","arguments":{"query":"x"}}`)
	if status != http.StatusTooManyRequests || fx.calls.Load() != 0 {
		t.Fatalf("budget refusal dispatched: status=%d calls=%d", status, fx.calls.Load())
	}
}

func TestCallToolDialFailureReleasesReservation(t *testing.T) {
	server, fx, _, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "dial_fail")
	status, _ := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-10","arguments":{"query":"x"}}`)
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
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-11","arguments":{"query":"x"}}`)
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
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-12","arguments":{"query":"x"}}`, "192.0.2.10:1234", "10.1.2.3")
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

func driftedSnapshot(t *testing.T, endpoint string) *routing.Snapshot {
	t.Helper()
	snapshot := testSnapshot(t, endpoint)
	snapshot.MCPServers[0].Digest = strings.Repeat("b", 64)
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func twoServerSnapshot(t *testing.T, endpoint string) *routing.Snapshot {
	t.Helper()
	snapshot := testSnapshot(t, endpoint)
	second := snapshot.MCPServers[0]
	second.ID = testServer2
	second.RevisionID = testServerRv2
	snapshot.MCPServers = append(snapshot.MCPServers, second)
	for index, revision := range snapshot.AgentPublications {
		if revision.ID != testRevision {
			continue
		}
		revision.Definition.Toolset.Tools = append(revision.Definition.Toolset.Tools, agentcontract.Tool{
			ServerID: testServer2, RevisionID: testServer2R, CatalogDigest: second.Digest,
			Name: "lookup", Alias: agentcontract.ToolAlias(testServer2, "lookup"),
		})
		revision.Digest = agentcontract.DraftDigest(revision.Definition)
		snapshot.AgentPublications[index] = revision
	}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestCallToolSnapshotDriftScopesPerServerAndRefusesExplicitListing(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	rt.release = &runtime.Release{ID: testGenID, Snapshot: twoServerSnapshot(t, fx.server.URL)}

	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-13","arguments":{"query":"x"}}`)
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("healthy server in a partially drifted toolset was not callable: %d %v", status, body)
	}
	status, body = request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+agentcontract.ToolAlias(testServer2, "lookup")+`","call_id":"existing-json-14","arguments":{"query":"x"}}`)
	if status != http.StatusConflict || jsonErrorCode(t, body) != "tool_definition_changed" || fx.calls.Load() != 1 {
		t.Fatalf("drifted server beside a healthy one was not refused as drift: %d %v calls=%d", status, body, fx.calls.Load())
	}
	status, body = request(t, server, http.MethodGet, "/v1/mcp/tools?toolset_id="+testToolset, "")
	if status != http.StatusConflict || jsonErrorCode(t, body) != "tool_definition_changed" {
		t.Fatalf("explicit listing silently skipped the drifted server: %d %v", status, body)
	}
	status, body = request(t, server, http.MethodGet, "/v1/mcp/tools", "")
	tools, _ := body["tools"].([]any)
	if status != http.StatusOK || len(tools) != 1 {
		t.Fatalf("all-toolsets listing lost the healthy server beside drift: %d %v", status, body)
	}

	unlive, unliveFixture, _, _, unliveRuntime := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	unliveRuntime.release = &runtime.Release{ID: testGenID, Snapshot: twoServerSnapshot(t, unliveFixture.server.URL)}
	unliveRuntime.denyResource = true
	status, body = request(t, unlive, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-15","arguments":{"query":"x"}}`)
	if status != http.StatusOK || unliveFixture.calls.Load() != 1 {
		t.Fatalf("authority failure caused by a drifted sibling blocked the healthy server: %d %v", status, body)
	}
}

func TestCallToolUncompilablePinnedSchemaRefusesWithItsOwnError(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	snapshot := testSnapshot(t, fx.server.URL)
	snapshot.MCPServers[0].Tools[0].InputSchema = json.RawMessage(`{"type":"object","$ref":"https://schemas.example.test/tool"}`)
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	rt.release = &runtime.Release{ID: testGenID, Snapshot: snapshot}

	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-16","arguments":{"query":"x"}}`)
	if status != http.StatusServiceUnavailable || jsonErrorCode(t, body) != "tool_schema_invalid" {
		t.Fatalf("uncompilable pinned schema was not refused with its own error: %d %v", status, body)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("uncompilable pinned schema reserved or dispatched: calls=%d reserved=%d", fx.calls.Load(), len(ledger.reserved))
	}
	status, body = request(t, server, http.MethodGet, "/v1/mcp/tools?toolset_id="+testToolset, "")
	if status != http.StatusServiceUnavailable || jsonErrorCode(t, body) != "tool_schema_invalid" {
		t.Fatalf("explicit listing silently skipped the uncompilable schema: %d %v", status, body)
	}
}

func TestCallToolDriftedLiveCatalogRefusesWithoutDispatch(t *testing.T) {
	server, fx, ledger, tl, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	fx.setDescription("Changed after publication")
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-17","arguments":{"query":"x"}}`)
	if status != http.StatusConflict || jsonErrorCode(t, body) != "tool_definition_changed" {
		t.Fatalf("drifted catalog call was not refused as a definition change: %d %v", status, body)
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
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-18","arguments":{"query":"x"}}`)
	if status != http.StatusConflict || jsonErrorCode(t, body) != "tool_definition_changed" {
		t.Fatalf("snapshot drifted pin did not refuse as a definition change: %d %v", status, body)
	}
	if fx.calls.Load() != 0 || len(ledger.reserved) != 0 || len(tl.snapshot()) != 0 {
		t.Fatalf("snapshot drifted call reserved or dispatched: calls=%d events=%v", fx.calls.Load(), tl.snapshot())
	}
	status, body = request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"lookup","call_id":"existing-json-19","arguments":{"query":"x"}}`)
	if status != http.StatusNotFound || jsonErrorCode(t, body) != "tool_not_found" {
		t.Fatalf("unpinned name masqueraded as drift: %d %v", status, body)
	}
}

func TestCallToolUnliveDriftedToolsetReportsDriftOnlyToAllowlistedCallers(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	rt.release = &runtime.Release{ID: testGenID, Snapshot: driftedSnapshot(t, fx.server.URL)}
	rt.failAuthzOn = 1
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-20","arguments":{"query":"x"}}`)
	if status != http.StatusConflict || jsonErrorCode(t, body) != "tool_definition_changed" {
		t.Fatalf("allowlisted caller did not learn the definition changed: %d %v", status, body)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("unlive drifted call dispatched: calls=%d", fx.calls.Load())
	}

	other, otherFixture, _, _, otherRuntime := newTestServer(t, testAuthority([]string{"tools"}, []string{testOtherTS}), "")
	otherRuntime.release = &runtime.Release{ID: testGenID, Snapshot: driftedSnapshot(t, otherFixture.server.URL)}
	status, body = request(t, other, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-21","arguments":{"query":"x"}}`)
	if status != http.StatusNotFound || jsonErrorCode(t, body) != "toolset_not_found" {
		t.Fatalf("caller outside the allowlist learned about drift: %d %v", status, body)
	}
	if otherFixture.calls.Load() != 0 {
		t.Fatalf("foreign drifted call dispatched: calls=%d", otherFixture.calls.Load())
	}
}

func republishedSnapshot(t *testing.T, endpoint string) *routing.Snapshot {
	t.Helper()
	snapshot := testSnapshot(t, endpoint)
	snapshot.MCPServers[0].RevisionID = testServerRv2
	for index, revision := range snapshot.AgentPublications {
		if revision.ID != testRevision {
			continue
		}
		revision.Definition.Toolset.Tools[0].RevisionID = testServerRv2
		revision.Digest = agentcontract.DraftDigest(revision.Definition)
		revision.ID = testRevision2
		snapshot.AgentPublications[index] = revision
	}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestRepublishedRevisionUnblocksOnlyItsOwnToolset(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset, testOtherTS}), "")
	drifted := driftedSnapshot(t, fx.server.URL)
	second := drifted.MCPServers[0]
	second.ID, second.RevisionID = testServer2, testServer2R
	catalog, err := mcpservers.Certify(t.Context(), testEgress(), fx.server.URL, []byte("static-token"))
	if err != nil {
		t.Fatalf("fixture catalog did not certify for the second server: %v", err)
	}
	second.Digest = catalog.Digest
	for index, revision := range drifted.AgentPublications {
		if revision.ID != testOtherRv {
			continue
		}
		revision.Definition.Toolset.Tools[0].ServerID = testServer2
		revision.Definition.Toolset.Tools[0].RevisionID = testServer2R
		revision.Definition.Toolset.Tools[0].CatalogDigest = second.Digest
		revision.Definition.Toolset.Tools[0].Alias = agentcontract.ToolAlias(testServer2, "lookup")
		revision.Digest = agentcontract.DraftDigest(revision.Definition)
		drifted.AgentPublications[index] = revision
	}
	drifted.MCPServers = append(drifted.MCPServers, second)
	if err := drifted.Compile(); err != nil {
		t.Fatal(err)
	}
	rt.release = &runtime.Release{ID: testGenID, Snapshot: drifted}

	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testOtherTS+`","name":"`+agentcontract.ToolAlias(testServer2, "lookup")+`","call_id":"existing-json-22","arguments":{"query":"x"}}`)
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("unrelated toolset was disturbed by drift elsewhere: %d %v", status, body)
	}
	status, body = request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-23","arguments":{"query":"x"}}`)
	if status != http.StatusConflict || jsonErrorCode(t, body) != "tool_definition_changed" || fx.calls.Load() != 1 {
		t.Fatalf("drifted toolset dispatched beside a healthy one: %d %v calls=%d", status, body, fx.calls.Load())
	}

	rt.release = &runtime.Release{ID: testGenID, Snapshot: testSnapshot(t, fx.server.URL)}
	status, body = request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"existing-json-24","arguments":{"query":"x"}}`)
	if status != http.StatusOK || fx.calls.Load() != 2 {
		t.Fatalf("reviewed catalog stayed blocked after re-publish: %d %v calls=%d", status, body, fx.calls.Load())
	}
}

func jsonCallBody(callID, query string) string {
	return `{"toolset_id":"` + testToolset + `","name":"` + alias() + `","call_id":"` + callID + `","arguments":{"query":"` + query + `"}}`
}

func TestCallToolRequiresCallerCallID(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","arguments":{"query":"x"}}`)
	if status != http.StatusBadRequest || jsonErrorCode(t, body) != "call_id_required" || fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
		t.Fatalf("call without a caller call id dispatched: status=%d body=%v calls=%d", status, body, fx.calls.Load())
	}
}

func TestCallToolExactRepeatReturnsRetainedResultWithoutReexecuting(t *testing.T) {
	authority := testAuthority([]string{"tools"}, []string{testToolset})
	server, fx, ledger, _, _ := newTestServer(t, authority, "large_result")
	fx.notify.Store(true)
	status, first := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-42", "secret-argument"))
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("first call did not execute once: status=%d body=%v calls=%d", status, first, fx.calls.Load())
	}
	ledger.repeat = retainedFrom(t, first, StatusSucceeded)
	status, second := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-42", "secret-argument"))
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("exact repeat re-executed upstream: status=%d body=%v calls=%d", status, second, fx.calls.Load())
	}
	if second["attempt_id"] != first["attempt_id"] || second["dispatch_id"] != first["dispatch_id"] {
		t.Fatalf("exact repeat lost the retained attempt identity: first=%v second=%v", first, second)
	}
	firstContent, _ := first["content"].([]any)
	secondContent, _ := second["content"].([]any)
	if len(firstContent) != 1 || len(secondContent) != 1 {
		t.Fatalf("exact repeat did not return the retained result: first=%v second=%v", first, second)
	}
	firstText, _ := firstContent[0].(map[string]any)
	secondText, _ := secondContent[0].(map[string]any)
	if firstText["text"] != strings.Repeat("x", 256<<10) || firstText["text"] != secondText["text"] {
		t.Fatalf("exact repeat returned a different result: first=%v second=%v", first, second)
	}
	if len(ledger.reserved) != 1 || len(ledger.evidence) != 1 {
		t.Fatalf("exact repeat reserved or settled a second debit: reserved=%d evidence=%d", len(ledger.reserved), len(ledger.evidence))
	}
	if ledger.evidence[0].Status != StatusSucceeded {
		t.Fatalf("large SSE result did not settle successfully: %+v", ledger.evidence[0])
	}
	reserved := ledger.reserved[0]
	if reserved.CallDigest == "order-42" || reserved.CallDigest != server.callDigest(*authority, "order-42") {
		t.Fatalf("caller call id was not reduced to its stable keyed digest before storage: %q", reserved.CallDigest)
	}
	other := *authority
	other.ID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50711"
	if server.callDigest(other, "order-42") == reserved.CallDigest {
		t.Fatal("call digest did not separate principals")
	}
}

func TestCallToolRepeatAfterTransientResultGoneReportsNotRetained(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	current := time.Now()
	server.Now = func() time.Time { return current }
	status, first := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-gone", "x"))
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("first call did not execute once: status=%d body=%v", status, first)
	}
	current = current.Add(2 * time.Hour)
	ledger.repeat = retainedFrom(t, first, StatusSucceeded)
	status, second := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-gone", "x"))
	if status != http.StatusConflict || jsonErrorCode(t, second) != "result_not_retained" || fx.calls.Load() != 1 {
		t.Fatalf("repeat with no retained result replayed upstream: status=%d body=%v calls=%d", status, second, fx.calls.Load())
	}
	if second["attempt_id"] != first["attempt_id"] || second["dispatch_id"] != first["dispatch_id"] || second["status"] != string(StatusSucceeded) {
		t.Fatalf("not-retained repeat lost the retained status and identity: first=%v second=%v", first, second)
	}
}

func TestCallToolChangedBodyUnderSameCallIDConflicts(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	status, first := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-7", "one"))
	if status != http.StatusOK {
		t.Fatalf("first call failed: status=%d body=%v", status, first)
	}
	ledger.repeat = retainedFrom(t, first, StatusSucceeded)
	ledger.repeat.Conflict = true
	status, second := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-7", "two"))
	if status != http.StatusConflict || jsonErrorCode(t, second) != "call_id_conflict" || fx.calls.Load() != 1 {
		t.Fatalf("changed body under one call id executed: status=%d body=%v calls=%d", status, second, fx.calls.Load())
	}
	if second["attempt_id"] != first["attempt_id"] || second["dispatch_id"] != first["dispatch_id"] {
		t.Fatalf("conflict did not name the original attempt: first=%v second=%v", first, second)
	}
}

func TestCallToolAmbiguousOutcomeStaysUnknownOnRepeat(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "drop")
	status, first := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-unknown", "x"))
	if status != http.StatusBadGateway || first["status"] != string(StatusOutcomeUnknown) || fx.calls.Load() != 1 {
		t.Fatalf("ambiguous dispatch was not held unknown: status=%d body=%v calls=%d", status, first, fx.calls.Load())
	}
	fx.setMode("")
	ledger.repeat = retainedFrom(t, first, StatusOutcomeUnknown)
	status, second := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-unknown", "x"))
	if status != http.StatusBadGateway || second["status"] != string(StatusOutcomeUnknown) || jsonErrorCode(t, second) != "outcome_unknown" || fx.calls.Load() != 1 {
		t.Fatalf("repeat of an unknown outcome redispatched: status=%d body=%v calls=%d", status, second, fx.calls.Load())
	}
	if second["attempt_id"] != first["attempt_id"] || second["dispatch_id"] != first["dispatch_id"] {
		t.Fatalf("unknown repeat lost the held attempt identity: first=%v second=%v", first, second)
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusOutcomeUnknown {
		t.Fatalf("held debit evidence changed on repeat: %+v", ledger.evidence)
	}
}

func TestCallToolUndispatchedFailureFreesCallIDForRetry(t *testing.T) {
	server, fx, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "dial_fail")
	status, first := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-retry", "x"))
	if status != http.StatusBadGateway || fx.calls.Load() != 0 {
		t.Fatalf("unreachable upstream was dispatched: status=%d body=%v calls=%d", status, first, fx.calls.Load())
	}
	fx.setMode("")
	status, second := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-retry", "x"))
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("retry of a never-dispatched call stayed bound: status=%d body=%v calls=%d", status, second, fx.calls.Load())
	}
}

func TestCallToolRepeatOfInFlightCallReportsInProgress(t *testing.T) {
	server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	status, first := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-flight", "x"))
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("first call did not execute once: status=%d body=%v", status, first)
	}
	ledger.repeat = retainedFrom(t, first, StatusInProgress)
	status, second := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-flight", "x"))
	if status != http.StatusConflict || jsonErrorCode(t, second) != "call_in_progress" || fx.calls.Load() != 1 {
		t.Fatalf("in-flight repeat was not held as in progress: status=%d body=%v calls=%d", status, second, fx.calls.Load())
	}
	if second["attempt_id"] != first["attempt_id"] || second["dispatch_id"] != first["dispatch_id"] || second["status"] != string(StatusInProgress) {
		t.Fatalf("in-progress repeat lost the retained identity: first=%v second=%v", first, second)
	}
}

func TestExecuteRefusesEmptyCallerCallIDBeforeReserve(t *testing.T) {
	authority := testAuthority([]string{"tools"}, []string{testToolset})
	server, fx, ledger, _, rt := newTestServer(t, authority, "")
	resolved, failure := server.resolveToolset(*authority, rt.release.Snapshot, testToolset)
	if failure != nil {
		t.Fatalf("toolset did not resolve: %v", failure)
	}
	tool, found := resolved.lookup(alias())
	if found != toolFound {
		t.Fatalf("tool did not resolve: %v", found)
	}
	outcome := server.execute(t.Context(), executeInput{
		authority: *authority, resolved: resolved, tool: tool,
		arguments: json.RawMessage(`{"query":"x"}`), releaseID: testGenID, callID: "",
	})
	if outcome.failure == nil || outcome.failure.code != "call_id_required" || len(ledger.reserved) != 0 || fx.calls.Load() != 0 {
		t.Fatalf("shared execute path accepted an empty caller call id: %+v reserved=%d calls=%d", outcome.failure, len(ledger.reserved), fx.calls.Load())
	}
}

func TestCallToolExactRepeatAfterDriftHitsDriftRefusalBeforeRetainedIdentity(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	status, first := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-drift", "x"))
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("first call did not execute once: status=%d body=%v", status, first)
	}
	ledger.repeat = retainedFrom(t, first, StatusSucceeded)
	rt.release = &runtime.Release{ID: testGenID, Snapshot: driftedSnapshot(t, fx.server.URL)}
	status, second := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("order-drift", "x"))
	if status != http.StatusConflict || jsonErrorCode(t, second) != "tool_definition_changed" || fx.calls.Load() != 1 {
		t.Fatalf("exact repeat after drift bypassed the drift refusal: status=%d body=%v calls=%d", status, second, fx.calls.Load())
	}
}

func TestRetainedResultCapEvictsOldestResult(t *testing.T) {
	server, _, _, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	current := time.Now()
	server.Now = func() time.Time { return current }
	result := mcpservers.ToolResult{Content: []mcpservers.ToolContent{{Type: "text", Text: "kept"}}}
	for i := 0; i < maxRetainedResults; i++ {
		server.retainResult("attempt-"+strconv.Itoa(i), result)
		current = current.Add(time.Second)
	}
	server.retainResult("attempt-newest", result)
	if _, ok := server.retainedResultFor("attempt-0"); ok {
		t.Fatal("retention cap kept the oldest result")
	}
	if _, ok := server.retainedResultFor("attempt-newest"); !ok {
		t.Fatal("retention cap dropped the newest result")
	}
	if _, ok := server.retainedResultFor("attempt-1"); !ok {
		t.Fatal("retention cap evicted more than the oldest result")
	}
}

type toolPolicyEntry struct {
	phase, action, config, kind string
}

func enableToolGuardrails(t *testing.T, rt *fakeRuntime, server *Server, entries ...toolPolicyEntry) {
	t.Helper()
	snapshot := rt.release.Snapshot
	registry := &guardrails.Registry{
		Definitions:          map[string]guardrails.Revision{},
		Policies:             map[string]guardrails.PolicyRevision{},
		ProjectOrganizations: map[string]string{},
	}
	pinned := []guardrails.Entry{}
	for i, entry := range entries {
		id := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f508" + strconv.Itoa(20+i)
		registry.Definitions[id] = guardrails.Revision{ID: id, GuardrailID: id, ProjectID: testProject, Type: cmp.Or(entry.kind, "builtin.keywords"), Configuration: json.RawMessage(entry.config), TimeoutMS: 1000, OnError: guardrails.FailClosed}
		pinned = append(pinned, guardrails.Entry{GuardrailID: id, RevisionID: id, Phase: entry.phase, Action: entry.action, Mandatory: true})
	}
	const policyID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50840"
	registry.Policies[policyID] = guardrails.PolicyRevision{ID: policyID, PolicyID: policyID, ProjectID: testProject, Entries: pinned}
	registry.Attachments = []guardrails.Attachment{{ID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50841", Scope: "toolset", ScopeID: testToolset, PolicyRevisionID: policyID}}
	snapshot.Guardrails = registry
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	server.Guardrails = func(ctx context.Context, in GuardrailEvaluation) (guardrails.Result, error) {
		return in.Engine.Evaluate(ctx, guardrails.Evaluation{RequestID: in.RequestID, Phase: in.Phase, Segments: in.Segments}, in.Masks)
	}
}

func errorCode(body map[string]any) string {
	failure, _ := body["error"].(map[string]any)
	code, _ := failure["code"].(string)
	return code
}

func TestCallToolGuardrailDeniedArgumentRefusesBeforeDispatch(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Input, action: guardrails.Block, config: `{"words":["forbidden"],"substrings":true}`})
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-deny-arg","arguments":{"query":"a forbid\u0064en value"}}`)
	if status != http.StatusBadRequest || errorCode(body) != "guardrail_blocked" {
		t.Fatalf("denied argument status %d: %v", status, body)
	}
	if fx.calls.Load() != 0 {
		t.Fatal("a denied argument still dispatched upstream")
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusGuardrailBlocked {
		t.Fatalf("denied argument was not recorded as blocked: %+v", ledger.evidence)
	}
	decisions := ledger.evidence[0].GuardrailDecisions
	if len(decisions) != 1 || decisions[0].Outcome != "blocked" {
		t.Fatalf("denied argument kept no decision evidence: %+v", decisions)
	}
	raw, _ := json.Marshal(ledger.evidence)
	if strings.Contains(string(raw), "forbidden") || strings.Contains(string(raw), "a forbidden value") {
		t.Fatal("attempt evidence carries the refused argument text")
	}
	if body["attempt_id"] == nil || body["dispatch_id"] == nil {
		t.Fatalf("denied argument lost its attempt identity: %v", body)
	}
}

func TestCallToolGuardrailDeniedResultSettlesWithoutReturningContent(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "structured_result")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Output, action: guardrails.Block, config: `{"words":["private"],"substrings":true}`})
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-deny-result","arguments":{"query":"x"}}`)
	if status != http.StatusBadRequest || errorCode(body) != "guardrail_blocked" {
		t.Fatalf("denied result status %d: %v", status, body)
	}
	if fx.calls.Load() != 1 {
		t.Fatal("a denied result did not come from one dispatched call")
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusGuardrailBlocked {
		t.Fatalf("denied result was recorded as a clean success: %+v", ledger.evidence)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "private result") {
		t.Fatal("the refused result reached the caller")
	}
	ledger.repeat = retainedFrom(t, body, StatusGuardrailBlocked)
	status, body = request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-deny-result","arguments":{"query":"x"}}`)
	if status != http.StatusBadRequest || errorCode(body) != "guardrail_blocked" || fx.calls.Load() != 1 {
		t.Fatalf("repeat of a denied result status %d calls=%d: %v", status, fx.calls.Load(), body)
	}
}

func TestCallToolGuardrailRedactsArgumentsBeforeDispatch(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "echo")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Input, action: guardrails.Redact, config: `{"words":["hunter2"],"substrings":true,"replacement":"[X]"}`})
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-redact-arg","arguments":{"query":"hunter2"}}`)
	if status != http.StatusOK {
		t.Fatalf("redacted call status %d: %v", status, body)
	}
	if received := fx.receivedArgs(); strings.Contains(received, "hunter2") || !strings.Contains(received, "[X]") {
		t.Fatalf("upstream did not receive the redacted arguments: %s", received)
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusSucceeded || len(ledger.evidence[0].GuardrailDecisions) == 0 {
		t.Fatalf("redacted call settled without decision evidence: %+v", ledger.evidence)
	}
}

func TestCallToolGuardrailRedactsResultsBeforeReturning(t *testing.T) {
	server, _, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "secret_result")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Output, action: guardrails.Redact, config: `{"words":["private"],"substrings":true,"replacement":"[X]"}`})
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-redact-result","arguments":{"query":"x"}}`)
	if status != http.StatusOK {
		t.Fatalf("redacted result status %d: %v", status, body)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "private") || !strings.Contains(string(encoded), "[X]") {
		t.Fatalf("caller did not receive only the redacted result: %s", encoded)
	}
	ledger.repeat = retainedFrom(t, body, StatusSucceeded)
	status, body = request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-redact-result","arguments":{"query":"x"}}`)
	encoded, _ = json.Marshal(body)
	if status != http.StatusOK || strings.Contains(string(encoded), "private") {
		t.Fatalf("retained repeat leaked the raw result: %d %s", status, encoded)
	}
}

func TestCallToolGuardrailMaskHidesValueFromUpstreamAndRestoresIt(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "echo")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Input, action: guardrails.Mask, config: `{"words":["hunter2"],"substrings":true}`})
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-mask","arguments":{"query":"hunter2"}}`)
	if status != http.StatusOK {
		t.Fatalf("masked call status %d: %v", status, body)
	}
	if received := fx.receivedArgs(); strings.Contains(received, "hunter2") {
		t.Fatalf("upstream received the unmasked value: %s", received)
	}
	encoded, _ := json.Marshal(body)
	if !strings.Contains(string(encoded), "hunter2") {
		t.Fatalf("caller did not get its masked value restored: %s", encoded)
	}
}

func TestCallToolGuardrailReplacementPreservesValidArgumentJSON(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Input, action: guardrails.Redact, config: `{"words":["hunter2"],"substrings":true,"replacement":"\""}`})
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-broken","arguments":{"query":"hunter2"}}`)
	if status != http.StatusOK || fx.calls.Load() != 1 {
		t.Fatalf("valid replacement was refused: %d %v", status, body)
	}
	var arguments struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(fx.receivedArgs()), &arguments); err != nil || arguments.Query != `"` {
		t.Fatalf("replacement corrupted argument JSON: %s %v", fx.receivedArgs(), err)
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusSucceeded {
		t.Fatalf("replacement was not settled: %+v", ledger.evidence)
	}
}

func TestCallToolGuardrailChecksSchemaAfterArgumentReplacement(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	schema, err := json.Marshal(guardrails.ToolsConfig{Schemas: map[string]json.RawMessage{
		alias(): json.RawMessage(`{"type":"object","properties":{"query":{"enum":["hunter2"]}},"required":["query"]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	enableToolGuardrails(t, rt, server,
		toolPolicyEntry{phase: guardrails.Input, action: guardrails.Redact, config: `{"words":["hunter2"],"substrings":true,"replacement":"changed"}`},
		toolPolicyEntry{phase: guardrails.Input, action: guardrails.Block, kind: "builtin.tools", config: string(schema)},
	)
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("guarded-schema-replacement", "hunter2"))
	if status != http.StatusBadRequest || errorCode(body) != "guardrail_blocked" || fx.calls.Load() != 0 {
		t.Fatalf("replacement escaped the tool argument schema: status=%d body=%v", status, body)
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusGuardrailBlocked {
		t.Fatalf("schema refusal was not retained: %+v", ledger.evidence)
	}
}

func TestCallToolGuardrailPermissionFollowsArgumentReplacement(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	name := alias()
	permission, err := json.Marshal(guardrails.ToolPermissionConfig{Rules: []guardrails.ToolPermissionRule{{
		ID: "only-changed", ToolName: &name, Decision: "allow", AllowedParamPatterns: map[string]string{"query": "changed"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	enableToolGuardrails(t, rt, server,
		toolPolicyEntry{phase: guardrails.Input, action: guardrails.Redact, config: `{"words":["hunter2"],"substrings":true,"replacement":"changed"}`},
		toolPolicyEntry{phase: guardrails.Input, action: guardrails.Block, kind: "builtin.tool_permission", config: string(permission)},
	)
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("guarded-permission-replacement", "hunter2"))
	if status != http.StatusOK {
		t.Fatalf("permission policy did not inspect the transformed arguments: %d %v", status, body)
	}
	var arguments struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(fx.receivedArgs()), &arguments); err != nil || arguments.Query != "changed" {
		t.Fatalf("permission policy dispatched different arguments: %s %v", fx.receivedArgs(), err)
	}
}

func TestCallToolGuardrailRedactsEscapedStructuredResult(t *testing.T) {
	server, _, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "structured_result")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Output, action: guardrails.Redact, config: `{"words":["private"],"substrings":true,"replacement":"\"\\"}`})
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("guarded-structured-replacement", "safe"))
	if status != http.StatusOK {
		t.Fatalf("structured replacement failed: %d %v", status, body)
	}
	structured, ok := body["structured_content"].(map[string]any)
	if !ok {
		t.Fatalf("replacement lost structured content: %v", body)
	}
	nested, ok := structured["nested"].([]any)
	if !ok || len(nested) != 1 {
		t.Fatalf("replacement lost the result array: %v", structured)
	}
	item, ok := nested[0].(map[string]any)
	if !ok || item["message"] != "\"\\ result" {
		t.Fatalf("structured replacement was not decoded and re-escaped: %v", nested)
	}
}

func TestCallToolGuardrailOpaqueResultFollowsOutputCoverage(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "image_result")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Output, action: guardrails.Block, config: `{"words":["private"],"substrings":true}`})
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-opaque","arguments":{"query":"x"}}`)
	if status != http.StatusUnprocessableEntity || errorCode(body) != "guardrail_surface_unavailable" {
		t.Fatalf("opaque result status %d: %v", status, body)
	}
	if fx.calls.Load() != 1 || len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusGuardrailBlocked {
		t.Fatalf("opaque result did not settle as blocked: calls=%d evidence=%+v", fx.calls.Load(), ledger.evidence)
	}

	server, _, _, _, rt = newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "image_result")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Input, action: guardrails.Block, config: `{"words":["forbidden"],"substrings":true}`})
	status, body = request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-opaque-open","arguments":{"query":"x"}}`)
	if status != http.StatusOK {
		t.Fatalf("opaque result without output coverage refused: %d %v", status, body)
	}
}

func TestCallToolGuardrailUnrunnablePhaseRefusesBeforeDispatch(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.OutputStream, action: guardrails.Block, config: `{"words":["private"],"substrings":true}`})
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call",
		`{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"guarded-stream","arguments":{"query":"x"}}`)
	if status != http.StatusUnprocessableEntity || errorCode(body) != "guardrail_surface_unavailable" || fx.calls.Load() != 0 {
		t.Fatalf("stream-phase policy status %d calls=%d: %v", status, fx.calls.Load(), body)
	}
}

func TestSessionCallToolGuardrailDeniedArgumentMatchesJSONSurface(t *testing.T) {
	server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Input, action: guardrails.Block, config: `{"words":["forbidden"],"substrings":true}`})
	sessionID := openSession(t, server, "test-key", latestProtocol())
	called := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"a forbid\u0064en value"},"_meta":{"olp.call_id":"guarded-session-deny"}}}`)
	failure := rpcError(t, called)
	data, _ := failure["data"].(map[string]any)
	if data["code"] != "guardrail_blocked" || fx.calls.Load() != 0 {
		t.Fatalf("session denied argument diverged from the JSON surface: calls=%d error=%v", fx.calls.Load(), failure)
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusGuardrailBlocked {
		t.Fatalf("session denial was not recorded as blocked: %+v", ledger.evidence)
	}
}

func TestSessionCallToolGuardrailDeniesEscapedStructuredResult(t *testing.T) {
	server, _, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "structured_result")
	enableToolGuardrails(t, rt, server, toolPolicyEntry{phase: guardrails.Output, action: guardrails.Block, config: `{"words":["private"],"substrings":true}`})
	sessionID := openSession(t, server, "test-key", latestProtocol())
	called := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"safe"},"_meta":{"olp.call_id":"guarded-session-output"}}}`)
	failure := rpcError(t, called)
	data, _ := failure["data"].(map[string]any)
	if data["code"] != "guardrail_blocked" {
		t.Fatalf("session returned an escaped prohibited result: %v", called.body)
	}
	if len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusGuardrailBlocked {
		t.Fatalf("session did not retain its blocked result: %+v", ledger.evidence)
	}
}

func TestToolGuardrailRefreshAffectsNewSessionCallsAndKeepsInflightPolicy(t *testing.T) {
	server, fx, _, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "secret_result")
	inputPolicy := toolPolicyEntry{phase: guardrails.Input, action: guardrails.Block, config: `{"words":["forbidden"],"substrings":true}`}
	enableToolGuardrails(t, rt, server, inputPolicy)
	next := &fakeRuntime{release: &runtime.Release{ID: testGenID, Snapshot: testSnapshot(t, fx.server.URL)}}
	enableToolGuardrails(t, next, &Server{}, inputPolicy, toolPolicyEntry{phase: guardrails.Output, action: guardrails.Block, config: `{"words":["private"],"substrings":true}`})
	evaluate := server.Guardrails
	server.Guardrails = func(ctx context.Context, in GuardrailEvaluation) (guardrails.Result, error) {
		result, err := evaluate(ctx, in)
		if in.Phase == guardrails.Input {
			rt.release = next.release
		}
		return result, err
	}
	sessionID := openSession(t, server, "test-key", latestProtocol())
	first := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"safe"},"_meta":{"olp.call_id":"guardrail-refresh-first"}}}`)
	if _, failed := first.body["error"]; failed || fx.calls.Load() != 1 {
		t.Fatalf("policy refresh changed an in-flight call: calls=%d body=%v", fx.calls.Load(), first.body)
	}
	second := rpcRequest(t, server, "test-key", sessionID, latestProtocol(), "/mcp",
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"`+alias()+`","arguments":{"query":"safe"},"_meta":{"olp.call_id":"guardrail-refresh-second"}}}`)
	failure := rpcError(t, second)
	data, _ := failure["data"].(map[string]any)
	if data["code"] != "guardrail_blocked" || fx.calls.Load() != 2 {
		t.Fatalf("an existing session missed the refreshed output policy: calls=%d error=%v", fx.calls.Load(), failure)
	}
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call", jsonCallBody("guardrail-refresh-json", "safe"))
	if status != http.StatusBadRequest || errorCode(body) != "guardrail_blocked" || fx.calls.Load() != 3 {
		t.Fatalf("JSON and session calls diverged after refresh: status=%d calls=%d body=%v", status, fx.calls.Load(), body)
	}
}

func TestToolCallRequiresUserGrantWithoutStaticFallback(t *testing.T) {
	configured, upstream, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
	registered := rt.release.Snapshot.MCPServers[0]
	snapshot := &routing.Snapshot{
		Generation:        rt.release.Snapshot.Generation,
		Providers:         rt.release.Snapshot.Providers,
		Routes:            rt.release.Snapshot.Routes,
		AgentPublications: rt.release.Snapshot.AgentPublications,
		MCPServers: []routing.MCPServer{{
			ID: registered.ID, ProjectID: registered.ProjectID, RevisionID: registered.RevisionID,
			Endpoint: registered.Endpoint, Protocol: registered.Protocol, Digest: registered.Digest,
			CredentialID: registered.CredentialID, Tools: registered.Tools,
			OAuth: &routing.MCPOAuth{ClientID: "tool-client", Audience: "tool-audience", Scopes: []string{"tools.read"}, AuthorizationEndpoint: upstream.server.URL + "/authorize", TokenEndpoint: upstream.server.URL + "/token"},
		}},
	}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		Runtime: &fakeRuntime{authority: rt.authority, release: &runtime.Release{ID: testGenID, Snapshot: snapshot}},
		Ledger:  ledger, Auth: configured.Auth, Limiter: configured.Limiter, Egress: configured.Egress,
		Credentials: configured.Credentials, Sessions: configured.Sessions,
	}
	status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call", `{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"grant-required","arguments":{"query":"hello"}}`)
	problem, ok := body["error"].(map[string]any)
	if status != http.StatusForbidden || !ok || problem["code"] != "user_grant_required" {
		t.Fatalf("missing individual grant did not refuse execution: %d %v", status, body)
	}
	if upstream.calls.Load() != 0 {
		t.Fatal("missing individual grant fell back to the registered static credential")
	}
}

func TestGrantEnrollmentRefusesAccountingIdentityWithoutVerifiedWorkload(t *testing.T) {
	accountingAuthority := testAuthority([]string{"tools"}, []string{testToolset})
	server, _, _, _, rt := newTestServer(t, accountingAuthority, "")
	labeled := &Server{Runtime: &fakeRuntime{authority: &access.Authority{ID: accountingAuthority.ID, ProjectID: accountingAuthority.ProjectID, Policy: accountingAuthority.Policy, EndUserDigest: strings.Repeat("a", 64)}, release: rt.release}, Auth: server.Auth}
	status, body := request(t, labeled, http.MethodPost, "/v1/mcp/servers/"+testServerID+"/grant-enrollments", `{"toolset_id":"`+testToolset+`"}`)
	problem, ok := body["error"].(map[string]any)
	if status != http.StatusForbidden || !ok || problem["code"] != "verified_individual_required" {
		t.Fatalf("accounting identity authorized personal enrollment: %d %v", status, body)
	}
}

func TestJSONCallAndSimulationRejectAmbiguousEnvelopes(t *testing.T) {
	valid := `{"toolset_id":"` + testToolset + `","name":"` + alias() + `","call_id":"envelope-call","arguments":{"query":"x"}}`
	for _, path := range []string{"/v1/mcp/tools/call", "/v1/mcp/tools/simulate"} {
		for name, body := range map[string]string{
			"duplicate":          strings.Replace(valid, `"call_id":"envelope-call"`, `"call_id":"other","call_id":"envelope-call"`, 1),
			"trailing":           valid + ` {}`,
			"invalid UTF-8":      strings.Replace(valid, `"query":"x"`, `"query":"`+string([]byte{0xff})+`"`, 1),
			"unpaired surrogate": strings.Replace(valid, `"query":"x"`, `"query":"\ud800"`, 1),
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				server, fixture, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
				status, response := request(t, server, http.MethodPost, path, body)
				if status != http.StatusBadRequest || jsonErrorCode(t, response) != "invalid_request" {
					t.Fatalf("ambiguous envelope accepted: %d %v", status, response)
				}
				if fixture.calls.Load() != 0 || len(ledger.reserved) != 0 {
					t.Fatal("ambiguous envelope reached execution")
				}
			})
		}
	}
}

func TestToolCallRefusesMalformedAndUnsupportedResults(t *testing.T) {
	for _, mode := range []string{"missing-result-content", "missing-content-kind", "missing-text", "embedded-resource", "empty-text", "empty-content"} {
		t.Run(mode, func(t *testing.T) {
			server, fixture, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), mode)
			status, body := request(t, server, "POST", "/v1/mcp/tools/call", `{"toolset_id":"`+testToolset+`","name":"`+alias()+`","call_id":"result-check","arguments":{"query":"x"}}`)
			if mode == "empty-text" || mode == "empty-content" {
				if status != http.StatusOK {
					t.Fatalf("valid empty content refused: %d %v", status, body)
				}
				return
			}
			if status != http.StatusBadGateway || body["status"] != string(StatusOutcomeUnknown) {
				t.Fatalf("invalid result settled successfully: %d %v", status, body)
			}
			if fixture.calls.Load() != 1 || len(ledger.evidence) != 1 || ledger.evidence[0].Status != StatusOutcomeUnknown {
				t.Fatalf("invalid result lost uncertain dispatch: %+v", ledger.evidence)
			}
		})
	}
}

func TestIdentityEvidenceReflectsCredentialAndLiveAuthorityOutcomes(t *testing.T) {
	oauth := &routing.MCPOAuth{ClientID: "client", Audience: "resource", Scopes: []string{"read"}, AuthorizationEndpoint: "https://identity.example.test/authorize", TokenEndpoint: "https://identity.example.test/token"}
	delegation := &routing.MCPDelegation{Issuer: "https://gateway.example.test", Audience: "resource"}
	for _, tc := range []struct {
		name              string
		oauth             *routing.MCPOAuth
		delegation        *routing.MCPDelegation
		missingCredential bool
		revoke            bool
		stale             bool
		status            int
		registered        IdentityOutcome
		personal          IdentityOutcome
		delegated         IdentityOutcome
		entra             IdentityOutcome
	}{
		{name: "static success", status: 200, registered: IdentityPassed, personal: IdentityNotConfigured, delegated: IdentityNotConfigured},
		{name: "static credential unavailable", missingCredential: true, status: 503, registered: IdentityPassed, personal: IdentityNotConfigured, delegated: IdentityNotConfigured},
		{name: "personal grant absent", oauth: oauth, status: 403, registered: IdentityPassed, personal: IdentityDenied, delegated: IdentityNotConfigured},
		{name: "delegation requires personal grant", oauth: oauth, delegation: delegation, status: 403, registered: IdentityPassed, personal: IdentityDenied, delegated: IdentityDenied},
		{name: "Entra requires verified user", oauth: &routing.MCPOAuth{ClientID: "client", Audience: "resource", Scopes: []string{"resource/read"}, TokenEndpoint: "https://identity.example.test/token", EntraOBO: &routing.MCPEntraOBO{Issuer: "https://identity.example.test"}}, status: 403, registered: IdentityPassed, personal: IdentityDenied, delegated: IdentityNotConfigured, entra: IdentityDenied},
		{name: "authority revoked before dispatch", revoke: true, status: 403, registered: IdentityDenied, personal: IdentityNotConfigured, delegated: IdentityNotConfigured},
		{name: "authority stale before dispatch", stale: true, status: 503, registered: IdentityDenied, personal: IdentityNotConfigured, delegated: IdentityNotConfigured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, fx, ledger, _, rt := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
			source := rt.release.Snapshot
			registered := source.MCPServers[0]
			snapshot := &routing.Snapshot{Generation: source.Generation, Providers: source.Providers, Routes: source.Routes, AgentPublications: source.AgentPublications, MCPServers: []routing.MCPServer{{ID: registered.ID, ProjectID: registered.ProjectID, RevisionID: registered.RevisionID, Endpoint: registered.Endpoint, Protocol: registered.Protocol, Digest: registered.Digest, CredentialID: registered.CredentialID, Tools: registered.Tools, OAuth: tc.oauth, Delegation: tc.delegation}}}
			if err := snapshot.Compile(); err != nil {
				t.Fatal(err)
			}
			rt.release = &runtime.Release{Snapshot: snapshot}
			if tc.missingCredential {
				server.Credentials = nil
			}
			if tc.revoke || tc.stale {
				server.Credentials = func(context.Context, string) ([]byte, error) {
					rt.revoked.Store(tc.revoke)
					rt.stale.Store(tc.stale)
					return []byte("static-token"), nil
				}
			}
			status, body := request(t, server, http.MethodPost, "/v1/mcp/tools/call", `{"toolset_id":"`+testToolset+`","name":"`+agentcontract.ToolAlias(testServerID, "lookup")+`","arguments":{"query":"hello"},"call_id":"identity-evidence"}`)
			if status != tc.status {
				t.Fatalf("status=%d expected%d: %v", status, tc.status, body)
			}
			if len(ledger.evidence) != 1 {
				t.Fatalf("missing durable identity evidence: %v", ledger.evidence)
			}
			for _, want := range []IdentityDecision{{Hook: RegisteredServerHook, Outcome: tc.registered}, {Hook: UserPermissionHook, Outcome: tc.personal}, {Hook: DelegatedSigningHook, Outcome: tc.delegated}} {
				actual := slices.IndexFunc(ledger.evidence[0].IdentityDecisions, func(d IdentityDecision) bool { return d.Hook == want.Hook })
				if actual < 0 || ledger.evidence[0].IdentityDecisions[actual].Outcome != want.Outcome {
					t.Fatalf("incorrect %s outcome: %v", want.Hook, ledger.evidence[0].IdentityDecisions)
				}
			}
			if tc.entra != "" {
				index := slices.IndexFunc(ledger.evidence[0].IdentityDecisions, func(d IdentityDecision) bool { return d.Hook == EntraOBOHook })
				if index < 0 || ledger.evidence[0].IdentityDecisions[index].Outcome != tc.entra {
					t.Fatalf("incorrect Entra outcome: %v", ledger.evidence[0].IdentityDecisions)
				}
			}
			if tc.status != 200 && fx.calls.Load() != 0 {
				t.Fatal("denied identity dispatched a tool")
			}
		})
	}
}

func TestToolRetryGuidanceRoundsUpOnlyPositiveDelays(t *testing.T) {
	for _, tc := range []struct {
		delay   time.Duration
		seconds int64
	}{
		{-time.Nanosecond, 0}, {0, 0}, {time.Nanosecond, 1}, {time.Second, 1}, {time.Second + time.Nanosecond, 2}, {1500 * time.Millisecond, 2}, {2 * time.Second, 2},
	} {
		t.Run(tc.delay.String(), func(t *testing.T) {
			server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
			session := openSession(t, server, "test-key", latestProtocol())
			ledger.reserveErr = &limits.ExceededError{Dimension: limits.DimensionDailyCost, RetryAfter: tc.delay}
			response := rpcRequest(t, server, "test-key", "", "", "/v1/mcp/tools/call", jsonCallBody("retry-guidance", "x"))
			expectedHeader := func() string {
				if tc.seconds == 0 {
					return ""
				}
				return strconv.FormatInt(tc.seconds, 10)
			}()
			if response.status != http.StatusTooManyRequests || response.headers.Get("Retry-After") != expectedHeader {
				t.Fatalf("incorrect retry guidance: status=%d header=%q expected%q", response.status, response.headers.Get("Retry-After"), expectedHeader)
			}
			rpc := rpcRequest(t, server, "test-key", session, latestProtocol(), "/mcp", sessionCallBody(2, "retry-guidance", "x"))
			var envelope struct {
				Error *struct {
					Data struct {
						Code    string `json:"code"`
						Seconds int64  `json:"retry_after_seconds"`
					} `json:"data"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(rpc.raw), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error == nil || envelope.Error.Data.Code != "budget_exceeded" || envelope.Error.Data.Seconds != tc.seconds {
				t.Fatalf("incorrect MCP retry guidance: %s", rpc.raw)
			}
			if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
				t.Fatal("budget refusal reserved or dispatched a call")
			}
		})
	}
}

func TestToolCallIdentifiersPreserveSupportedBytesAndRejectControls(t *testing.T) {
	type jsonCallInput struct {
		ToolsetID string          `json:"toolset_id"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
	}
	type mcpCallInput struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      callRequestMeta `json:"_meta"`
	}

	for _, tc := range []struct {
		name, id string
		valid    bool
	}{
		{"single byte", "a", true}, {"maximum bytes", strings.Repeat("x", maxCallIDBytes), true}, {"embedded space", "a b", true}, {"leading and trailing space", " a ", true},
		{"over maximum", strings.Repeat("x", maxCallIDBytes+1), false}, {"empty", "", false}, {"space only", " ", false}, {"null byte", "a\x00b", false}, {"control byte", "a\x1fb", false}, {"delete byte", "a\x7fb", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, fx, ledger, _, _ := newTestServer(t, testAuthority([]string{"tools"}, []string{testToolset}), "")
			session := openSession(t, server, "test-key", latestProtocol())
			arguments := json.RawMessage(`{"query":"x"}`)
			raw, err := json.Marshal(jsonCallInput{ToolsetID: testToolset, Name: alias(), CallID: tc.id, Arguments: arguments})
			if err != nil {
				t.Fatal(err)
			}
			response := rpcRequest(t, server, "test-key", "", "", "/v1/mcp/tools/call", string(raw))
			expectedStatus := func() int {
				if tc.valid {
					return http.StatusOK
				}
				return http.StatusBadRequest
			}()
			if response.status != expectedStatus {
				t.Fatalf("call identifier validity=%v returned%d: %s", tc.valid, response.status, response.raw)
			}
			if tc.valid {
				ledger.repeat = retainedFrom(t, response.body, StatusSucceeded)
			}
			encoded, encodeErr := json.Marshal(struct {
				Version string       `json:"jsonrpc"`
				ID      int          `json:"id"`
				Method  string       `json:"method"`
				Params  mcpCallInput `json:"params"`
			}{Version: "2.0", ID: 2, Method: "tools/call", Params: mcpCallInput{Name: alias(), Arguments: arguments, Meta: callRequestMeta{CallID: &tc.id}}})
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			rpc := rpcRequest(t, server, "test-key", session, latestProtocol(), "/mcp", string(encoded))
			if (rpc.body["error"] == nil) != tc.valid {
				t.Fatalf("MCP identifier validity=%v: %s", tc.valid, rpc.raw)
			}
			if tc.valid {
				if fx.calls.Load() != 1 || len(ledger.reserved) != 1 {
					t.Fatal("supported identifier was lost or replayed upstream")
				}
				expectedDigest := hex.EncodeToString(server.Auth.Digest(secrets.ToolCallDigest, testKeyID+"\x00"+tc.id))
				if ledger.reserved[0].CallDigest != expectedDigest {
					t.Fatal("caller identifier was normalized before binding")
				}
				return
			}
			if fx.calls.Load() != 0 || len(ledger.reserved) != 0 {
				t.Fatal("invalid identifier reserved or dispatched a call")
			}
		})
	}
}

func TestWorkflowResolvesAndRunsOnlyAuthorizedReadOnlyTools(t *testing.T) {
	authority := testAuthority([]string{"tools"}, []string{testToolset})
	server, upstream, _, _, installed := newTestServer(t, authority, "")
	source := installed.release.Snapshot
	published := source.AgentPublications[0]
	original := published.Definition.Toolset.Tools[0]
	releaseFor := func(classification string) *runtime.Release {
		t.Helper()
		tool := agentcontract.Tool{
			ServerID: original.ServerID, RevisionID: original.RevisionID, CatalogDigest: original.CatalogDigest,
			Name: original.Name, Alias: original.Alias, Classification: classification,
		}
		definition := agentcontract.Draft{ProjectID: testProject, Name: "tools", Toolset: &agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}
		snapshot := &routing.Snapshot{
			Generation: source.Generation, Providers: source.Providers, Routes: source.Routes, MCPServers: source.MCPServers,
			AgentPublications: []agentcontract.Revision{{ID: testRevision, PublicationID: testToolset, ProjectID: testProject, Kind: "toolset", Digest: agentcontract.DraftDigest(definition), Definition: definition, Current: true}},
		}
		if err := snapshot.Compile(); err != nil {
			t.Fatal(err)
		}
		return &runtime.Release{ID: testGenID, Snapshot: snapshot}
	}
	unclassified := releaseFor("")
	unclassifiedTools, resolveErr := server.Resolve(t.Context(), *authority, unclassified, []string{testToolset})
	if resolveErr != nil || len(unclassifiedTools) != 1 {
		t.Fatalf("resolve published tool: %v %v", unclassifiedTools, resolveErr)
	}
	_, approvalErr := server.Call(t.Context(), *authority, unclassified, unclassifiedTools[0], testGenID, 0, "unclassified", json.RawMessage(`{"query":"q"}`))
	approval, isApproval := errors.AsType[*WorkflowError](approvalErr)
	if !isApproval || approval.Code != "tool_approval_required" {
		t.Fatalf("unclassified tool was not refused: %v", approvalErr)
	}
	if upstream.calls.Load() != 0 {
		t.Fatal("unclassified tool reached upstream")
	}
	readOnly := releaseFor("read_only")
	tools, toolsErr := server.Resolve(t.Context(), *authority, readOnly, []string{testToolset})
	if toolsErr != nil || len(tools) != 1 {
		t.Fatalf("resolve read-only tool: %v %v", tools, toolsErr)
	}
	_, argumentsErr := server.Call(t.Context(), *authority, readOnly, tools[0], testGenID, 0, "bad-arguments", json.RawMessage(`{"query":42}`))
	invalid, isInvalid := errors.AsType[*WorkflowError](argumentsErr)
	if !isInvalid || invalid.Code != "invalid_tool_arguments" || upstream.calls.Load() != 0 {
		t.Fatalf("invalid arguments reached upstream: %v", argumentsErr)
	}
	result, callErr := server.Call(t.Context(), *authority, readOnly, tools[0], testGenID, 1, "valid-call", json.RawMessage(`{"query":"q"}`))
	if callErr != nil || !bytes.Contains(result, []byte("answer for lookup")) {
		t.Fatalf("read-only call failed: %s %v", result, callErr)
	}
	if upstream.calls.Load() != 1 {
		t.Fatalf("read-only call count: %d", upstream.calls.Load())
	}
	agentRate := int64(1)
	delegated := *authority
	delegated.DelegationAgentID = testOtherRv
	delegated.DelegationAgentLimits = &access.DelegationAgentLimits{ID: testOtherRv, LookupID: "agent-lookup", Limits: access.AdmissionLimits{RequestsPerMinute: &agentRate}}
	server.Limiter = &countingRateLimiter{counts: map[string]int64{"agent-lookup": 1}}
	if _, err := server.Call(t.Context(), delegated, readOnly, tools[0], testGenID, 2, "delegated-workflow", json.RawMessage(`{"query":"q"}`)); err != nil || upstream.calls.Load() != 2 {
		t.Fatalf("delegated workflow could not use its parent admission: %v", err)
	}
	if _, err := server.Resolve(t.Context(), *testAuthority([]string{"tools"}, nil), readOnly, []string{testToolset}); err == nil {
		t.Fatal("unauthorized toolset resolved")
	}
	if _, err := server.Resolve(t.Context(), *authority, readOnly, []string{testToolset, testToolset}); err == nil {
		t.Fatal("duplicate tool aliases resolved")
	}
	stale := WorkflowTool{ToolsetID: tools[0].ToolsetID, RevisionID: testOtherRv, Definition: tools[0].Definition}
	if _, err := server.Call(t.Context(), *authority, readOnly, stale, testGenID, 2, "stale", json.RawMessage(`{"query":"q"}`)); err == nil {
		t.Fatal("stale tool revision executed")
	}
}
