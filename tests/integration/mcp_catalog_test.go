//go:build integration

package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/management/contract"
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

func catalogManagement(t *testing.T, pool *pgxpool.Pool, ownerID string, policy *egress.Policy) func(method, path, ifMatch, body string, idempotencyKey ...string) apiResult {
	t.Helper()
	ctx := t.Context()
	keys, installation := notificationKeys(t, pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint) VALUES($1,$2,$3,now()+interval '1 hour','test')`, uuid.NewString(), ownerID, auth.Digest(secrets.SessionDigest, token)); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registration.New(access.New(access.Config{Pool: pool, Installation: installation, Origin: "https://test.example", Auth: auth, Keys: keys, Log: slog.New(slog.DiscardHandler)}), policy, registration.NewExecutables(nil, pool)).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	csrf := base64.RawURLEncoding.EncodeToString(auth.Digest(secrets.CSRFDigest, token))
	return func(method, path, ifMatch, body string, idempotencyKey ...string) apiResult {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://test.example")
		r.Header.Set("X-CSRF-Token", csrf)
		if len(idempotencyKey) > 0 {
			r.Header.Set("Idempotency-Key", idempotencyKey[0])
		}
		if ifMatch != "" {
			r.Header.Set("If-Match", ifMatch)
		}
		r.AddCookie(&http.Cookie{Name: "__Host-olp_session", Value: token})
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s %s -> %d", method, path, response.StatusCode)
		return apiResult{status: response.StatusCode, etag: response.Header.Get("ETag"), body: raw}
	}
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
	call := catalogManagement(t, pool, ownerID, policy)
	previewPath := "/api/v1/mcp-servers/" + saved.ID + "/catalog-preview"
	approvePath := "/api/v1/mcp-servers/" + saved.ID + "/catalog-approval"
	expect := func(step string, got apiResult, status int) apiResult {
		t.Helper()
		if got.status != status {
			t.Fatalf("%s: HTTP %d, want %d: %s", step, got.status, status, got.body)
		}
		return got
	}
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
	unchanged := expect("approve unchanged catalog", call("POST", approvePath, fmt.Sprintf("%q", before.etag), currentBody, "approve-unchanged"), http.StatusConflict)
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
	previewed := expect("preview", call("POST", previewPath, "", ""), http.StatusOK)
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
	conflict := expect("approve changed catalog", call("POST", approvePath, previewed.etag, approvalBody, "approve-stale-catalog"), http.StatusConflict)
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
	expect("approve stale etag", call("POST", approvePath, previewed.etag, approvalBody, "approve-stale-etag"), http.StatusPreconditionFailed)
	if after := state(); after.revisionID != before.revisionID || after.digest != before.digest || after.revisions != before.revisions {
		t.Fatalf("stale approval changed certification: before=%+v after=%+v", before, after)
	}
	reviewed := expect("preview again", call("POST", previewPath, "", ""), http.StatusOK)
	var fresh contract.MCPCatalogPreviewResponse
	if err := json.Unmarshal(reviewed.body, &fresh); err != nil {
		t.Fatal(err)
	}
	freshBody := fmt.Sprintf(`{"protocol_version":%q,"digest":%q}`, string(fresh.DiscoveredProtocolVersion), fresh.DiscoveredDigest)
	approved := expect("approve reviewed catalog", call("POST", approvePath, reviewed.etag, freshBody, "approve-reviewed"), http.StatusOK)
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
	replayed := expect("replay approval", call("POST", approvePath, reviewed.etag, freshBody, "approve-reviewed"), http.StatusOK)
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
