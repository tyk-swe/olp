//go:build integration

package integration_test

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

type usageFixture struct {
	pool         *pgxpool.Pool
	ownerID      string
	projectID    string
	providerID   string
	keyID        string
	generationID string
	observedAt   time.Time
}

func newUsageFixture(t *testing.T, pool *pgxpool.Pool) usageFixture {
	t.Helper()
	fixture := usageFixture{
		pool: pool, ownerID: uuid.NewString(), projectID: uuid.NewString(), providerID: uuid.NewString(),
		keyID: uuid.NewString(), generationID: uuid.NewString(), observedAt: time.Now().UTC().Add(-time.Minute),
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users (id,email,display_name,role,etag)
		VALUES ($1::uuid,$1::text || '@example.com','Usage fixture','owner',$1::uuid)`, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.projects (id,name,etag,created_by)
		VALUES ($1::uuid,$1::text,$1::uuid,$2)`, fixture.projectID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers
		(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id)
		VALUES ($1::uuid,$1::text,'openai_compatible','draft','{}',$1::uuid,$1::uuid,$2,$3)`,
		fixture.providerID, fixture.ownerID, fixture.projectID); err != nil {
		t.Fatal(err)
	}
	fixture.addKey(t, fixture.keyID)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.runtime_releases (id,sequence,sha256,snapshot,created_by)
		SELECT $1,coalesce(max(sequence),0)+1,repeat('0',64),'{}',$2 FROM olp.runtime_releases`,
		fixture.generationID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	pricingID := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.pricing_revisions (id,revision,effective_at,created_by)
		SELECT $1,coalesce(max(revision),0)+1,$2,$3 FROM olp.pricing_revisions`,
		pricingID, fixture.observedAt.Add(-time.Hour), fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.prices
		(pricing_revision_id,provider_kind,provider_id,model,operation,input_per_million,output_per_million,currency)
		VALUES ($1,'openai_compatible',$2,$3,'generation',1,2,'USD')`, pricingID, fixture.providerID, vendor.Model); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f usageFixture) addKey(t *testing.T, keyID string) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO olp.api_keys (id,lookup_id,digest,name,created_by,policy,etag,project_id)
		VALUES ($1::uuid,$1::text,decode(repeat('00',32),'hex'),'Usage fixture',$2,'{}',$1::uuid,$3)`,
		keyID, f.ownerID, f.projectID); err != nil {
		t.Fatal(err)
	}
}

func (f usageFixture) attempt(ordinal int) usage.Attempt {
	status, input, output := http.StatusOK, int64(vendor.InputTokens), int64(vendor.OutputTokens)
	return usage.Attempt{
		ID: uuid.NewString(), Ordinal: ordinal, ProviderID: f.providerID, UpstreamModel: vendor.Model,
		StartedAt: f.observedAt, CompletedAt: f.observedAt, StatusCode: &status, Committed: true,
		Usage: &usage.AttemptUsage{Evidence: usage.EvidenceObservedComplete, UsageQuantities: usage.UsageQuantities{InputTokens: &input, OutputTokens: &output}},
	}
}

func (f usageFixture) event(attempts ...usage.Attempt) *usage.Event {
	final := attempts[len(attempts)-1]
	return &usage.Event{
		Version: usage.WireVersion, EventID: uuid.NewString(), RequestID: uuid.NewString(),
		RuntimeGenerationID: f.generationID, APIKeyID: f.keyID, ProviderID: &final.ProviderID,
		RouteSlug: "usage-fixture", UpstreamModel: &final.UpstreamModel, Operation: "generation", Surface: "openai",
		RequestStartedAt: f.observedAt, RequestCompletedAt: f.observedAt, ObservedAt: f.observedAt,
		StatusCode: final.StatusCode, Committed: final.Committed, UsageComplete: true,
		InputTokens: final.Usage.InputTokens, OutputTokens: final.Usage.OutputTokens, Attempts: attempts,
	}
}

type apiResult struct {
	status   int
	etag     string
	location string
	body     []byte
}

func expectStatus(t *testing.T, step string, got apiResult, want int) apiResult {
	t.Helper()
	if got.status != want {
		if step != "" {
			t.Fatalf("%s: HTTP %d, want %d: %s", step, got.status, want, got.body)
		}
		t.Fatalf("HTTP %d, want %d: %s", got.status, want, got.body)
	}
	return got
}

func mustExec(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func notificationKeys(t *testing.T, pool *pgxpool.Pool) (*secrets.KeyRing, string) {
	t.Helper()
	var installation string
	if err := pool.QueryRow(t.Context(), "SELECT id::text FROM olp.installation WHERE singleton").Scan(&installation); err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.ParseRingWithUnwrapper(context.Background(), []byte(`{"active_version":1,"keys":[{"version":1,"key":"`+strings.Repeat("01", 32)+`"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	return keys, installation
}

type managementClient struct {
	pool      *pgxpool.Pool
	access    *access.Server
	auth      *secrets.AuthKey
	server    *httptest.Server
	sessionID string
	token     string
	csrf      string
}

func managementHTTPAs(t *testing.T, pool *pgxpool.Pool, userID string, register func(*http.ServeMux, *access.Server)) *managementClient {
	t.Helper()
	ctx := t.Context()
	keys, installation := notificationKeys(t, pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	accessServer := access.New(access.Config{
		Pool: pool, Installation: installation, Origin: "https://test.example",
		Auth: auth, Keys: keys, Log: slog.New(slog.DiscardHandler),
	})
	mux := http.NewServeMux()
	register(mux, accessServer)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	base := &managementClient{pool: pool, access: accessServer, auth: auth, server: server}
	return base.as(t, userID)
}

func (c *managementClient) as(t *testing.T, userID string) *managementClient {
	t.Helper()
	sessionID := uuid.NewString()
	token := uuid.NewString()
	if _, err := c.pool.Exec(t.Context(), `INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint) VALUES($1,$2,$3,now()+interval '1 hour','test')`,
		sessionID, userID, c.auth.Digest(secrets.SessionDigest, token)); err != nil {
		t.Fatal(err)
	}
	return &managementClient{
		pool:      c.pool,
		access:    c.access,
		auth:      c.auth,
		server:    c.server,
		sessionID: sessionID,
		token:     token,
		csrf:      base64.RawURLEncoding.EncodeToString(c.auth.Digest(secrets.CSRFDigest, token)),
	}
}

func (c *managementClient) tryCall(ctx context.Context, method, path, ifMatch, body string, idempotencyKey ...string) (apiResult, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.server.URL+path, reader)
	if err != nil {
		return apiResult{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://test.example")
	key := uuid.NewString()
	if len(idempotencyKey) > 0 {
		key = idempotencyKey[0]
	}
	r.Header.Set("Idempotency-Key", key)
	r.Header.Set("X-CSRF-Token", c.csrf)
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	r.AddCookie(&http.Cookie{Name: access.SessionCookie, Value: c.token})
	response, err := c.server.Client().Do(r)
	if err != nil {
		return apiResult{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return apiResult{}, err
	}
	return apiResult{
		status:   response.StatusCode,
		etag:     response.Header.Get("ETag"),
		location: response.Header.Get("Location"),
		body:     raw,
	}, nil
}

func (c *managementClient) call(t *testing.T, method, path, ifMatch, body string, idempotencyKey ...string) apiResult {
	t.Helper()
	res, err := c.tryCall(t.Context(), method, path, ifMatch, body, idempotencyKey...)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
