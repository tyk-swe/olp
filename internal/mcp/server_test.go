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
	"strconv"
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
	if live[id] != revision || project != testProject {
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
			result = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "answer for " + params.Name}},
				"isError": fx.currentMode() == "tool_error",
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
	unliveRuntime.failAuthzOn = 2
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
	server, fx, ledger, _, _ := newTestServer(t, authority, "")
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
	if firstText["text"] != secondText["text"] {
		t.Fatalf("exact repeat returned a different result: first=%v second=%v", first, second)
	}
	if len(ledger.reserved) != 1 || len(ledger.evidence) != 1 {
		t.Fatalf("exact repeat reserved or settled a second debit: reserved=%d evidence=%d", len(ledger.reserved), len(ledger.evidence))
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
