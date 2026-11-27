//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/domain/agentcontract"
	limitdebits "github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/messages"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
)

type catalogFixture struct {
	mu      sync.Mutex
	tools   []upstream.ListedTool
	calls   atomic.Int64
	badAuth atomic.Int64
}

func (f *catalogFixture) set(tools []upstream.ListedTool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools = tools
}

func (f *catalogFixture) current() []upstream.ListedTool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upstream.ListedTool{}, f.tools...)
}

func (f *catalogFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			f.badAuth.Add(1)
		}
		var request messages.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Valid() {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fixture-session")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{"tools":{}}}}`, request.ID, messages.SupportedProtocols()[0])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			listing, err := json.Marshal(f.current())
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":%s}}`, request.ID, listing)
		case "tools/call":
			f.calls.Add(1)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"called"}]}}`, request.ID)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
}

func fixtureTool(name, description, schema string) upstream.ListedTool {
	return upstream.ListedTool{Name: name, Description: description, InputSchema: json.RawMessage(schema)}
}

func catalogManagement(t *testing.T, pool *pgxpool.Pool, ownerID string, policy *egress.Policy) *managementClient {
	t.Helper()
	return managementHTTPAs(t, pool, ownerID, func(mux *http.ServeMux, server *access.Server) {
		registration.New(server, policy, registration.NewExecutables(nil, pool)).Register(mux)
	})
}

type savedRegistration struct {
	etag       string
	revisionID string
	digest     string
	certified  time.Time
	revisions  int
}

func TestMCPCatalogReviewApproval(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	ctx := t.Context()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	ownerID, projectID := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO olp.users (id,email,display_name,role,etag) VALUES ($1::uuid,$1::text || '@example.com','Catalog fixture','owner',$1::uuid)`, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.projects (id,name,etag,created_by) VALUES ($1::uuid,$1::text,$1::uuid,$2)`, projectID, ownerID); err != nil {
		t.Fatal(err)
	}
	fixture := &catalogFixture{}
	fixture.set([]upstream.ListedTool{
		fixtureTool("lookup", "Look up a record", `{"type":"object","properties":{"id":{"type":"string"}}}`),
		fixtureTool("legacy", "Legacy lookup", `{"type":"object","properties":{}}`),
		fixtureTool("status", "Report status", `{"type":"object","properties":{}}`),
	})
	upstreamServer := httptest.NewServer(fixture.handler(t))
	t.Cleanup(upstreamServer.Close)
	keys, installation := notificationKeys(t, pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	certified, err := upstream.CertifyOptions(ctx, policy, upstreamServer.URL, []byte("fixture-token"), upstream.TransportOptions{Transport: routing.MCPStreamableHTTP})
	if err != nil {
		t.Fatal(err)
	}
	credentialID := uuid.NewString()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := keys.Store(ctx, tx, installation, credentialID, secrets.MCPCredential, []byte("fixture-token"), nil); err != nil {
		t.Fatal(err)
	}
	saved, err := registration.Save(ctx, tx, ownerID, nil, registration.Definition{
		ProjectID: projectID, Name: "fixture-tools", Endpoint: upstreamServer.URL, Transport: "streamable_http",
		Enabled: true, Catalog: certified, CredentialID: &credentialID,
		ToolPrices: map[string]string{"lookup": "1.5", "legacy": "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	publicationID, toolsetRevisionID := uuid.NewString(), uuid.NewString()
	pin := func(name string) agentcontract.Tool {
		return agentcontract.Tool{ServerID: saved.ID, RevisionID: saved.RevisionID, CatalogDigest: certified.Digest, Name: name, Alias: agentcontract.ToolAlias(saved.ID, name)}
	}
	draft := agentcontract.Draft{ProjectID: projectID, Name: "Fixture tools", Toolset: &agentcontract.Toolset{Tools: []agentcontract.Tool{pin("status"), pin("lookup")}}}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by) VALUES($1,$2,'toolset','Fixture tools',$3,$4,$5)`, publicationID, projectID, draft, uuid.NewString(), ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by) VALUES($1,$2,$3,'toolset',$4,$5,$6)`, toolsetRevisionID, publicationID, projectID, draft, agentcontract.DraftDigest(draft), ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.agent_publications SET latest_revision_id=$2 WHERE id=$1`, publicationID, toolsetRevisionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	state := func() savedRegistration {
		t.Helper()
		var current savedRegistration
		err := pool.QueryRow(ctx, `SELECT s.etag::text,s.latest_revision_id::text,v.digest,v.certified_at,(SELECT count(*) FROM olp.mcp_server_revisions WHERE server_id=s.id) FROM olp.mcp_servers s JOIN olp.mcp_server_revisions v ON v.id=s.latest_revision_id WHERE s.id=$1`, saved.ID).Scan(&current.etag, &current.revisionID, &current.digest, &current.certified, &current.revisions)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}
	before := state()
	client := catalogManagement(t, pool, ownerID, policy)
	previewPath := "/api/v1/mcp-servers/" + saved.ID + "/catalog-preview"
	approvePath := "/api/v1/mcp-servers/" + saved.ID + "/catalog-approval"
	problemType := func(step string, got apiResult) string {
		t.Helper()
		var problem struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(got.body, &problem); err != nil {
			t.Fatalf("%s problem = %s", step, got.body)
		}
		return problem.Type
	}
	currentBody := fmt.Sprintf(`{"protocol_version":%q,"digest":%q}`, certified.Protocol, certified.Digest)
	unchanged := expectStatus(t, "approve unchanged catalog", client.call(t, "POST", approvePath, fmt.Sprintf("%q", before.etag), currentBody, "approve-unchanged"), http.StatusConflict)
	if kind := problemType("approve unchanged catalog", unchanged); !strings.HasSuffix(kind, "/mcp_catalog_current") {
		t.Fatalf("unchanged approval problem = %s", kind)
	}
	if after := state(); after != before {
		t.Fatalf("unchanged approval changed the registration: before=%+v after=%+v", before, after)
	}
	fixture.set([]upstream.ListedTool{
		fixtureTool("lookup", "Look up a record", `{"type":"object","properties":{"id":{"type":"string"},"verbose":{"type":"boolean"}},"required":["id"]}`),
		fixtureTool("search", "Search records", `{"type":"object","properties":{"q":{"type":"string"}}}`),
		fixtureTool("status", "Report status", `{"type":"object","properties":{}}`),
	})
	previewed := expectStatus(t, "preview", client.call(t, "POST", previewPath, "", ""), http.StatusOK)
	var preview contract.MCPCatalogPreviewResponse
	if err := json.Unmarshal(previewed.body, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.ServerId.String() != saved.ID || preview.Etag.String() != before.etag || preview.SavedDigest != certified.Digest || preview.DiscoveredDigest == certified.Digest {
		t.Fatalf("preview identity = %+v", preview)
	}
	if len(preview.Added) != 1 || preview.Added[0].Name != "search" || len(preview.Removed) != 1 || preview.Removed[0].Name != "legacy" {
		t.Fatalf("preview added/removed = %+v / %+v", preview.Added, preview.Removed)
	}
	if len(preview.Modified) != 1 || preview.Modified[0].Name != "lookup" || len(preview.Modified[0].ChangedFields) != 1 || preview.Modified[0].ChangedFields[0] != "input_schema" {
		t.Fatalf("preview modified = %+v", preview.Modified)
	}
	if len(preview.Toolsets) != 1 || preview.Toolsets[0].Id.String() != publicationID || !slices.Equal(preview.Toolsets[0].Tools, []string{"lookup", "status"}) {
		t.Fatalf("preview toolsets = %+v", preview.Toolsets)
	}
	if after := state(); after != before {
		t.Fatalf("preview changed the registration: before=%+v after=%+v", before, after)
	}
	approvalBody := fmt.Sprintf(`{"protocol_version":%q,"digest":%q}`, string(preview.DiscoveredProtocolVersion), preview.DiscoveredDigest)
	fixture.set([]upstream.ListedTool{
		fixtureTool("lookup", "Look up a record", `{"type":"object","properties":{"id":{"type":"string"},"verbose":{"type":"boolean"}},"required":["id"]}`),
		fixtureTool("search", "Search everything", `{"type":"object","properties":{"q":{"type":"string"}}}`),
		fixtureTool("status", "Report status", `{"type":"object","properties":{}}`),
	})
	conflict := expectStatus(t, "approve changed catalog", client.call(t, "POST", approvePath, previewed.etag, approvalBody, "approve-stale-catalog"), http.StatusConflict)
	if kind := problemType("approve changed catalog", conflict); !strings.HasSuffix(kind, "/mcp_catalog_changed") {
		t.Fatalf("conflict problem = %s", kind)
	}
	if after := state(); after != before {
		t.Fatalf("conflicting approval changed the registration: before=%+v after=%+v", before, after)
	}
	fixture.set([]upstream.ListedTool{
		fixtureTool("lookup", "Look up a record", `{"type":"object","properties":{"id":{"type":"string"},"verbose":{"type":"boolean"}},"required":["id"]}`),
		fixtureTool("search", "Search records", `{"type":"object","properties":{"q":{"type":"string"}}}`),
		fixtureTool("status", "Report status", `{"type":"object","properties":{}}`),
	})
	if _, err := pool.Exec(ctx, `UPDATE olp.mcp_servers SET etag=gen_random_uuid() WHERE id=$1`, saved.ID); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, "approve stale etag", client.call(t, "POST", approvePath, previewed.etag, approvalBody, "approve-stale-etag"), http.StatusPreconditionFailed)
	if after := state(); after.revisionID != before.revisionID || after.digest != before.digest || after.revisions != before.revisions {
		t.Fatalf("stale approval changed certification: before=%+v after=%+v", before, after)
	}
	reviewed := expectStatus(t, "preview again", client.call(t, "POST", previewPath, "", ""), http.StatusOK)
	var fresh contract.MCPCatalogPreviewResponse
	if err := json.Unmarshal(reviewed.body, &fresh); err != nil {
		t.Fatal(err)
	}
	freshBody := fmt.Sprintf(`{"protocol_version":%q,"digest":%q}`, string(fresh.DiscoveredProtocolVersion), fresh.DiscoveredDigest)
	approved := expectStatus(t, "approve reviewed catalog", client.call(t, "POST", approvePath, reviewed.etag, freshBody, "approve-reviewed"), http.StatusOK)
	var server contract.MCPServerResponse
	if err := json.Unmarshal(approved.body, &server); err != nil {
		t.Fatal(err)
	}
	if server.Revision != 2 || server.Catalog.Digest != fresh.DiscoveredDigest || string(server.Catalog.ProtocolVersion) != string(fresh.DiscoveredProtocolVersion) {
		t.Fatalf("approved server = %+v", server)
	}
	if len(server.Catalog.Tools) != 3 || server.Catalog.Tools[0].Name != "lookup" || server.Catalog.Tools[1].Name != "search" || server.Catalog.Tools[2].Name != "status" {
		t.Fatalf("approved tools = %+v", server.Catalog.Tools)
	}
	if len(server.ToolPrices) != 1 || server.ToolPrices["lookup"] != "1.5" {
		t.Fatalf("approved tool prices = %v", server.ToolPrices)
	}
	replayed := expectStatus(t, "replay approval", client.call(t, "POST", approvePath, reviewed.etag, freshBody, "approve-reviewed"), http.StatusOK)
	if string(replayed.body) != string(approved.body) {
		t.Fatalf("replay differed: %s", replayed.body)
	}
	if after := state(); after.revisions != 2 || after.digest != fresh.DiscoveredDigest {
		t.Fatalf("final registration = %+v", after)
	}
	if fixture.calls.Load() != 0 {
		t.Fatalf("fixture saw %d tools/call requests", fixture.calls.Load())
	}
	if fixture.badAuth.Load() != 0 {
		t.Fatalf("fixture saw %d requests without the stored credential", fixture.badAuth.Load())
	}
}

func TestMCPManualSettlementPreservesPricingState(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		pricing, price string
		expected       int64
	}{{"unpriced", "NULL", 1}, {"free", "0", 0}} {
		t.Run(test.pricing, func(t *testing.T) {
			t.Parallel()
			pricing, price, expected := test.pricing, test.price, test.expected
			pool, _ := harness.Database(t)
			f := newUsageFixture(t, pool)
			ctx := t.Context()
			pub, rev, server, srev, dispatch, call := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
			sql := fmt.Sprintf(`
 BEGIN;
 INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by) VALUES('%[1]s','%[2]s','toolset','audit','{}','%[1]s','%[3]s');
 INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by) VALUES('%[4]s','%[1]s','%[2]s','toolset','{}',repeat('0',64),'%[3]s');
 INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,latest_revision_id,etag,created_by) VALUES('%[5]s','%[2]s','audit','streamable_http','https://example.com/mcp',true,'%[6]s','%[5]s','%[3]s');
 INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,created_by) VALUES('%[6]s','%[5]s',1,'https://example.com/mcp','2025-03-26','[]',repeat('0',64),'%[3]s');
 INSERT INTO olp.execution_debits(id,api_key_id,project_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,state,prepared_until,dispatched_at) VALUES('%[7]s','%[8]s','%[2]s','tool','{}',decode(repeat('00',32),'hex'),'%[7]s','USD',0,0,'outcome_unknown',now(),now());
 INSERT INTO olp.execution_debit_owners(dispatch_id,owner_id,owner_kind,basis,bound,daily_window_id,weekly_window_id,monthly_window_id) VALUES('%[7]s','%[8]s','key','cost',0,0,0,0);
 INSERT INTO olp.mcp_tool_calls(id,dispatch_id,api_key_id,project_id,toolset_id,toolset_revision_id,server_id,server_revision_id,tool_alias,input_digest,status,started_at,pricing_state,currency,unit_price) VALUES('%[9]s','%[7]s','%[8]s','%[2]s','%[1]s','%[4]s','%[5]s','%[6]s','t_'||repeat('0',32)||'_'||repeat('0',32),repeat('0',64),'outcome_unknown',now(),'%[10]s','USD',%[11]s);
 COMMIT;`, pub, f.projectID, f.ownerID, rev, server, srev, dispatch, f.keyID, call, pricing, price)
			if _, err := pool.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			client := managementHTTPAs(t, pool, f.ownerID, func(mux *http.ServeMux, a *access.Server) {
				accounting.NewManagement(a).Register(mux)
			})
			for range 2 {
				expectStatus(t, "resolve", client.call(t, http.MethodPost, "/api/v1/mcp-tool-calls/"+call+"/resolve", "", `{"outcome":"settled","evidence_digest":"`+strings.Repeat("a", 64)+`","observed_cost":"0","observed_charge":"0"}`, "audit"), http.StatusOK)
			}
			var attempts, uncharged int64
			if err := pool.QueryRow(ctx, `SELECT unpriced_attempts,uncharged_attempts FROM olp.api_key_cost_windows WHERE api_key_id=$1 AND window_kind='month'`, f.keyID).Scan(&attempts, &uncharged); err != nil {
				t.Fatal(err)
			}
			t.Logf("two resolve requests returned HTTP 200; %s attempts=%d, uncharged=%d", pricing, attempts, uncharged)
			if attempts != expected || uncharged != expected {
				t.Fatalf("pricing=%s: unpriced=%d uncharged=%d, want %d", pricing, attempts, uncharged, expected)
			}
		})
	}
}

type cancelOnBegin struct{ cancel context.CancelFunc }

func (c cancelOnBegin) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (c cancelOnBegin) TraceQueryEnd(_ context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil && data.CommandTag.String() == "BEGIN" {
		c.cancel()
	}
}

func TestCanceledToolSettlementKeepsConnectionReusable(t *testing.T) {
	t.Parallel()
	db, _ := harness.EmptyDatabase(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	config := db.Config()
	config.MinConns, config.MaxConns = 0, 1
	config.ConnConfig.Tracer = cancelOnBegin{cancel: cancel}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	connections := pool.Stat().NewConnsCount()
	if err := limitdebits.NewStore(pool).SettleTool(ctx, limitdebits.Result{}); !errors.Is(err, limitdebits.ErrConflict) || ctx.Err() == nil {
		t.Fatalf("settlement = %v, context = %v", err, ctx.Err())
	}
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := pool.Stat().NewConnsCount(); got != connections {
		t.Fatalf("connections opened = %d, want %d", got, connections)
	}
}

func TestMCPLateSettlementAfterOrphanRecovery(t *testing.T) {
	t.Parallel()
	for _, status := range []accounting.Status{accounting.StatusSucceeded, accounting.StatusUpstreamError, accounting.StatusGuardrailBlocked} {
		t.Run(string(status), func(t *testing.T) {
			pool, _ := harness.Database(t)
			f := newUsageFixture(t, pool)
			ctx := t.Context()
			pub, rev, server, srev, dispatch, call := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
			sql := fmt.Sprintf(`
 INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by) VALUES('%[1]s','%[2]s','toolset','late','{}','%[1]s','%[3]s');
 INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by) VALUES('%[4]s','%[1]s','%[2]s','toolset','{}',repeat('0',64),'%[3]s');
 INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,latest_revision_id,etag,created_by) VALUES('%[5]s','%[2]s','late','streamable_http','https://example.com/mcp',true,'%[6]s','%[5]s','%[3]s');
 INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,created_by) VALUES('%[6]s','%[5]s',1,'https://example.com/mcp','2025-03-26','[]',repeat('0',64),'%[3]s');
 INSERT INTO olp.execution_debits(id,api_key_id,project_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,state,prepared_until) VALUES('%[7]s','%[8]s','%[2]s','tool','{}',decode(repeat('00',32),'hex'),'%[7]s','USD',1,1,'ready',now()+interval '1 hour');
 INSERT INTO olp.execution_debit_owners(dispatch_id,owner_id,owner_kind,basis,bound,daily_window_id,weekly_window_id,monthly_window_id) VALUES('%[7]s','%[8]s','key','cost',1,0,0,0);`, pub, f.projectID, f.ownerID, rev, server, srev, dispatch, f.keyID)
			if _, err := pool.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			price := "1"
			reservation := &accounting.Reservation{DispatchID: dispatch, ClaimID: dispatch, AttemptID: call, APIKeyID: f.keyID, ProjectID: f.projectID, ToolsetID: pub, ToolsetRevisionID: rev, ToolAlias: "t_" + strings.Repeat("0", 32) + "_" + strings.Repeat("0", 32), InputDigest: strings.Repeat("0", 64), CallDigest: strings.Repeat("1", 64), UnitPrice: &price, Currency: "USD"}
			evidence := accounting.AttemptEvidence{ServerID: server, ServerRevisionID: srev, StartedAt: time.Now().UTC(), Status: accounting.StatusDispatched}
			ledger := accounting.NewDurableLedger(pool, nil, nil, nil)
			if err := ledger.MarkDispatched(ctx, reservation, evidence); err != nil {
				t.Fatal(err)
			}
			if n, err := ledger.RecoverOrphanedDispatches(ctx, time.Now().Add(time.Minute), 1); err != nil || n != 1 {
				t.Fatalf("recover = %d, %v", n, err)
			}
			unknown := evidence
			unknown.Status = accounting.StatusOutcomeUnknown
			unknown.ResultDigest = strings.Repeat("3", 64)
			unknown.CompletedAt = time.Now().UTC()
			if err := ledger.MarkUnknown(ctx, reservation, unknown); err != nil {
				t.Fatal(err)
			}
			evidence.Status = status
			evidence.IsError = status == accounting.StatusUpstreamError
			evidence.CompletedAt = time.Now().UTC()
			evidence.ResultDigest = strings.Repeat("2", 64)
			for range 2 {
				if err := ledger.Settle(ctx, reservation, evidence); err != nil {
					t.Fatal(err)
				}
			}
			var callStatus, debitState, resultDigest string
			var chargedOnce bool
			var receipts int
			if err := pool.QueryRow(ctx, `SELECT c.status,d.state,c.result_digest,d.observed_cost=1 AND d.observed_charge=1,(SELECT count(*) FROM olp.execution_debit_results WHERE dispatch_id=d.id) FROM olp.mcp_tool_calls c JOIN olp.execution_debits d ON d.id=c.dispatch_id WHERE c.id=$1`, call).Scan(&callStatus, &debitState, &resultDigest, &chargedOnce, &receipts); err != nil {
				t.Fatal(err)
			}
			t.Logf("call=%s debit=%s charged_once=%t receipts=%d", callStatus, debitState, chargedOnce, receipts)
			if callStatus != string(status) || debitState != "settled" || !chargedOnce || resultDigest != evidence.ResultDigest || receipts != 1 {
				t.Fatalf("late settlement: call=%s debit=%s receipts=%d", callStatus, debitState, receipts)
			}
		})
	}
}
