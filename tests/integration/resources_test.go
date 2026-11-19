//go:build integration

package integration_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func (f usageFixture) resource(kind, keyID, upstream string) *resources.Resource {
	return &resources.Resource{
		Kind: kind, APIKeyID: keyID, RouteSlug: "stored",
		ProviderID: f.providerID, ProviderRevisionID: uuid.NewString(),
		RouteRevisionID: uuid.NewString(), SlotID: uuid.NewString(), TargetID: uuid.NewString(),
		UpstreamID: upstream, State: "completed",
	}
}

func upstreams(listed []*resources.Resource) []string {
	names := make([]string, len(listed))
	for i, resource := range listed {
		names[i] = resource.UpstreamID
	}
	return names
}

func TestStoredResourcesAnswerOnlyTheKeyThatOwnsThem(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	otherKey := uuid.NewString()
	fixture.addKey(t, otherKey)
	store := resources.New(pool)
	put := func(keyID, upstream string) *resources.Resource {
		t.Helper()
		stored, err := store.Put(ctx, fixture.resource(resources.KindBatch, keyID, upstream))
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	list := func(keyID string, limit int, after string) []string {
		t.Helper()
		listed, err := store.ListKinds(ctx, []string{resources.KindBatch}, keyID, limit, after)
		if err != nil {
			t.Fatalf("list after %q: %v", after, err)
		}
		return upstreams(listed)
	}
	same := func(what string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", what, got, want)
			}
		}
	}

	put(otherKey, "foreign")
	oldest := put(fixture.keyID, "oldest")
	middle := put(fixture.keyID, "middle")
	newest := put(fixture.keyID, "newest")

	if got, err := store.Get(ctx, resources.KindBatch, fixture.keyID, oldest.ID); err != nil || got.UpstreamID != "oldest" {
		t.Fatalf("the owner reading its resource: %+v, %v", got, err)
	}
	if _, err := store.Get(ctx, resources.KindBatch, otherKey, oldest.ID); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("another key reading the resource: %v, want %v", err, resources.ErrNotFound)
	}
	if _, err := store.Get(ctx, resources.KindFile, fixture.keyID, oldest.ID); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("reading the resource as another kind: %v, want %v", err, resources.ErrNotFound)
	}

	if got, err := store.GetByUpstream(ctx, oldest.NativeIdentity()); err != nil || got.ID != oldest.ID {
		t.Fatalf("finding the resource by its upstream identity: %+v, %v", got, err)
	}
	renamed := oldest.NativeIdentity()
	renamed.UpstreamID = "newest"
	if _, err := store.GetByUpstream(ctx, renamed); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("finding the resource under another upstream identifier: %v, want %v", err, resources.ErrNotFound)
	}

	same("the owner's first page", list(fixture.keyID, 2, ""), "newest", "middle")
	same("the owner's second page", list(fixture.keyID, 2, middle.ID), "oldest")
	same("another key's list", list(otherKey, 10, ""), "foreign")
	same("another key paging from the owner's cursor", list(otherKey, 10, newest.ID))

	if err := store.Tombstone(ctx, middle.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, resources.KindBatch, fixture.keyID, middle.ID); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("reading a deleted resource: %v, want %v", err, resources.ErrNotFound)
	}
	if _, err := store.GetByUpstream(ctx, middle.NativeIdentity()); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("finding a deleted resource by its upstream identity: %v, want %v", err, resources.ErrNotFound)
	}
	if got, err := store.GetByUpstreamIncludingDeleted(ctx, middle.NativeIdentity()); err != nil || got.ID != middle.ID || got.State != resources.StateDeleted {
		t.Fatalf("the deleted resource's identifier mapping: %+v, %v", got, err)
	}
	same("the owner's list after a deletion", list(fixture.keyID, 10, ""), "newest", "oldest")

	lapsed := fixture.resource(resources.KindBatch, fixture.keyID, "lapsed")
	past := time.Now().Add(-time.Minute)
	lapsed.ExpiresAt = &past
	expired, err := store.Put(ctx, lapsed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, resources.KindBatch, fixture.keyID, expired.ID); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("reading an expired resource: %v, want %v", err, resources.ErrNotFound)
	}
	same("the owner's list after an expiry", list(fixture.keyID, 10, ""), "newest", "oldest")
}

func TestDurableContractsAreSealedForTheirOwner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	otherKey := uuid.NewString()
	fixture.addKey(t, otherKey)
	installation, err := database.Installation(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	keys := keyRing(t, "01")
	auth := secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	store := resources.NewEncrypted(pool, installation, keys)
	payload := []byte(`{"native":"the caller's uploaded file"}`)
	contract := func(upstream string) *resources.Resource {
		version, expires := resources.DurableContractVersion, time.Now().Add(time.Hour)
		resource := fixture.resource(resources.KindStrictFile, fixture.keyID, upstream)
		resource.ContractVersion, resource.ExpiresAt = &version, &expires
		return resource
	}

	if _, err := resources.New(pool).PutDurableContract(ctx, contract("unsealed"), payload); !errors.Is(err, resources.ErrContract) {
		t.Fatalf("storing a payload without a key ring: %v, want %v", err, resources.ErrContract)
	}
	stored, err := store.PutDurableContract(ctx, contract("sealed"), payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, got, err := store.ReadDurableContract(ctx, resources.KindStrictFile, fixture.keyID, stored.ID); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the owner reading its payload: %q, %v", got, err)
	}

	refusals := map[string]struct {
		store *resources.Store
		owner string
		want  error
	}{
		"another key":          {store, otherKey, resources.ErrNotFound},
		"another master key":   {resources.NewEncrypted(pool, installation, keyRing(t, "02")), fixture.keyID, resources.ErrContract},
		"another installation": {resources.NewEncrypted(pool, uuid.NewString(), keys), fixture.keyID, resources.ErrContract},
	}
	for name, refusal := range refusals {
		if _, got, err := refusal.store.ReadDurableContract(ctx, resources.KindStrictFile, refusal.owner, stored.ID); !errors.Is(err, refusal.want) || got != nil {
			t.Errorf("%s reading the payload: %q, %v, want %v", name, got, err, refusal.want)
		}
	}

	if err := store.Tombstone(ctx, stored.ID); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.secrets WHERE id::text=$1`, stored.UUID.String()).Scan(&kept); err != nil || kept != 0 {
		t.Fatalf("sealed payloads kept after deletion: %d, %v", kept, err)
	}

	revoked, err := store.PutDurableContract(ctx, contract("revoked"), payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE olp.api_keys SET revoked_at=now() WHERE id=$1`, fixture.keyID); err != nil {
		t.Fatal(err)
	}
	if _, got, err := store.ReadDurableContract(ctx, resources.KindStrictFile, fixture.keyID, revoked.ID); !errors.Is(err, resources.ErrNotFound) || got != nil {
		t.Fatalf("a revoked key reading its payload: %q, %v, want %v", got, err, resources.ErrNotFound)
	}
}

func TestStoredCreationKeepsOneIdentityAcrossReplayAndChildren(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	store := resources.New(pool)
	candidate := fixture.resource(resources.KindFile, fixture.keyID, "")
	submission := resources.Submission{Operation: "batch:file", Key: uuid.NewString(), Digest: bytes.Repeat([]byte{1}, 32)}
	creation, err := store.BeginCreate(ctx, candidate, submission)
	if err != nil {
		t.Fatal(err)
	}
	if err := creation.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	prepared, err := creation.PrepareCreated(resources.CreationResult{IDs: []string{"first-file", "second-file"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	accepted := prepared.Resource()
	if err := creation.Close(ctx); err != nil {
		t.Fatal(err)
	}
	replayedCandidate := *candidate
	replayed, err := store.BeginCreate(ctx, &replayedCandidate, submission)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Existing() || replayed.Resource().ID != accepted.ID || replayed.Resource().UpstreamID != "first-file" {
		t.Fatalf("replay lost the accepted identity: %+v", replayed.Resource())
	}
	if err := replayed.Dispatch(ctx); !errors.Is(err, resources.ErrSubmissionConflict) {
		t.Fatalf("replay could dispatch another creation: %v", err)
	}
	listed, err := store.ListKinds(ctx, []string{resources.KindFile}, fixture.keyID, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].UpstreamID == listed[1].UpstreamID {
		t.Fatalf("created native identities were lost: %+v", listed)
	}
	for _, child := range listed {
		if child.State != "active" || child.ProviderID != accepted.ProviderID || child.RouteRevisionID != accepted.RouteRevisionID || child.APIKeyID != fixture.keyID {
			t.Fatalf("created identity lost its ownership: %+v", child)
		}
	}
}

type listedRuntime struct {
	release *release.Release
	inputs  *routinginputs.Inputs
	keys    map[string]keyauthority.Authority
}

func (r *listedRuntime) Release() *release.Release { return r.release }

func (r *listedRuntime) RoutingInputs() *routinginputs.Inputs { return r.inputs }

func (r *listedRuntime) Authenticate(secret string) (*keyauthority.Authority, error) {
	authority, ok := r.keys[secret]
	if !ok {
		return nil, release.ErrInvalidKey
	}
	return &authority, nil
}

func (r *listedRuntime) CheckRouteFidelity(context.Context, routing.Route) error { return nil }

func (r *listedRuntime) GrantGeneration(string) int64 { return 0 }

func (r *listedRuntime) CredentialRefused(string, int64) {}

func (r *listedRuntime) Eligibility(string) routing.Eligibility { return routing.Eligible }

func (r *listedRuntime) Secret(_ context.Context, _ *release.Release, _ string) ([]byte, int64, error) {
	return []byte("listed-secret"), 0, nil
}

func (r *listedRuntime) NetworkSecret(_ context.Context, _ *release.Release, _, _ string) ([]byte, error) {
	return []byte("listed-secret"), nil
}

type storedPlan struct {
	slug, model string
	kind        string
	surface     string
	profile     string
	version     string
	toolsets    []string
	tools       kernel.WorkflowToolRunner
	secure      bool
	operations  []string
	prices      []rates.RoutingPrice
	upstream    http.Handler
}

type storedGateway struct {
	t       *testing.T
	fixture usageFixture
	plan    storedPlan
	store   *resources.Store
	runtime *listedRuntime
	mux     *http.ServeMux
	server  *httptest.Server

	providerRevisionID, routeRevisionID, slotID, targetID, modelID, credentialID string
}

func newStoredGateway(t *testing.T, fixture usageFixture, plan storedPlan) *storedGateway {
	t.Helper()
	ctx, pool := t.Context(), fixture.pool
	upstream := httptest.NewUnstartedServer(plan.upstream)
	var network *egress.ConnectionOptions
	if plan.secure {
		upstream.StartTLS()
		network = &egress.ConnectionOptions{TrustRootsPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}))}
	} else {
		upstream.Start()
	}
	t.Cleanup(upstream.Close)
	installation, err := database.Installation(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	keys, auth := keyRing(t, "01"), secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	g := &storedGateway{
		t: t, fixture: fixture, plan: plan, store: resources.NewEncrypted(pool, installation, keys),
		providerRevisionID: uuid.NewString(), slotID: uuid.NewString(), targetID: uuid.NewString(), modelID: uuid.NewString(), credentialID: uuid.NewString(),
	}
	credentialVersion := 1
	slot := routing.Slot{ID: g.slotID, Name: "default", Enabled: true, Weight: 1, CredentialID: &g.credentialID, CredentialVersion: &credentialVersion}
	kind, endpoint := cmp.Or(plan.kind, "openai"), upstream.URL+cmp.Or(plan.version, "/v1")
	profileRevision := ""
	if plan.profile != "" {
		profileRevision = connectors.ProfileRevision
	}
	providerConfig := encoded(t, map[string]any{"kind": kind, "endpoint": endpoint, "auth_mode": "api_key", "deployment": plan.model, "profile_id": plan.profile, "profile_revision": profileRevision, "options": map[string]any{"network": network}})
	certified := make([]routing.RevisionCapability, len(plan.operations))
	for i, operation := range plan.operations {
		certified[i] = routing.RevisionCapability{Operation: operation, Surface: cmp.Or(plan.surface, "openai"), Mode: "unary", Source: "certified"}
	}
	models := encoded(t, []routing.RevisionModel{{ID: g.modelID, UpstreamModel: plan.model, DisplayName: plan.model, Capabilities: certified}})
	if _, err := pool.Exec(ctx, `INSERT INTO olp.provider_credentials (id,provider_id,version) VALUES ($1,$2,1)`, g.credentialID, fixture.providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.provider_revisions (id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by)
		VALUES ($1,$2,1,'listed',$3,$6,$4,1,$1,$5)`, g.providerRevisionID, fixture.providerID, providerConfig,
		encoded(t, []routing.RevisionSlot{{Slot: slot, Default: true}}), fixture.ownerID, models); err != nil {
		t.Fatal(err)
	}
	routeID, routeRevisionID := g.seedRoute(plan.slug)
	g.routeRevisionID = routeRevisionID

	capabilities := make([]routing.Capability, len(plan.operations))
	for i, operation := range plan.operations {
		capabilities[i] = routing.Capability{Model: plan.model, Operation: operation, Surface: cmp.Or(plan.surface, "openai"), Mode: "unary", ToolCalls: plan.tools != nil}
	}
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers: map[string]routing.Provider{fixture.providerID: {
			ID: fixture.providerID, Name: "listed", Kind: connectors.Kind(kind), Enabled: true, ProfileID: plan.profile, ProfileRevision: profileRevision, Network: network,
			ActiveCredential: &g.credentialID, RevisionID: g.providerRevisionID, Capabilities: capabilities,
			Endpoint: endpoint, AuthMode: "api_key", Slots: []routing.Slot{slot}, Deployment: plan.model,
		}},
		Routes: map[string]routing.Route{plan.slug: {
			ID: routeID, Slug: plan.slug, Operations: plan.operations, OverallTimeout: 5000, MaxAttempts: 3,
			RoutingID: routeID, RevisionID: g.routeRevisionID, Revision: 1, PublishedAt: time.Now(), ProjectID: &fixture.projectID,
			Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed},
			Targets:  []routing.Target{{ID: g.targetID, ProviderID: fixture.providerID, ProviderModel: plan.model, Weight: 1, Timeout: 2000, RoutingID: g.modelID}},
		}},
	}
	if plan.tools != nil {
		route := snapshot.Routes[plan.slug]
		route.ToolExecution, route.Toolsets = "gateway", plan.toolsets
		snapshot.Routes[plan.slug] = route
	}
	published, err := release.NewRelease(uuid.NewString(), 1, snapshot, map[string][]byte{g.credentialID: []byte("listed-secret")})
	if err != nil {
		t.Fatal(err)
	}
	g.runtime = &listedRuntime{
		release: published,
		keys:    map[string]keyauthority.Authority{},
		inputs:  &routinginputs.Inputs{Prices: plan.prices, RefreshedAt: time.Now()},
	}
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	runner := newRunner(t)
	debitStore := debits.NewStore(pool)
	syncer := debits.NewSyncer(debitStore, runner)
	limiter := admission.NewDurable(runner, syncer)
	workflow := kernel.Dependencies{}
	if plan.tools != nil {
		workflow = kernel.Dependencies{WorkflowTools: plan.tools, WorkflowRuns: runs.NewWorkflowStore(pool, auth, keys, installation), Debits: kernel.DurableSessionDebits(pool, debitStore, syncer)}
	}
	server := kernel.New(kernel.Dependencies{
		WorkflowTools: workflow.WorkflowTools, WorkflowRuns: workflow.WorkflowRuns, Debits: workflow.Debits,
		Runtime:   g.runtime,
		Admission: quota.NewAdmission(limiter, fleet.New(runner), nil, nil),
		Egress:    &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Getenv:    func(string) string { return "" },
		Digester:  secrets.NewAuthKey(make([]byte, 32), "listed-resources"),
		Resources: g.store,
		Resolver:  resources.NewResolver(pool),
		Batches:   batches.NewStore(pool),
		Staging:   spool,
		Uploads:   staging.NewAdmission(spool.CapacityBytes()),
	}, kernel.Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxMediaBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096})
	g.mux = http.NewServeMux()
	gateway.Register(server, g.mux)
	g.server = httptest.NewServer(g.mux)
	t.Cleanup(g.server.Close)
	return g
}

func encoded(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (g *storedGateway) seedRoute(slug string) (string, string) {
	g.t.Helper()
	ctx, pool, fixture := g.t.Context(), g.fixture.pool, g.fixture
	routeID, revisionID := uuid.NewString(), uuid.NewString()
	targets := encoded(g.t, []routing.PublishedTarget{{
		ID: g.targetID, ProviderModelID: g.modelID, ProviderID: fixture.providerID,
		ProviderModel: g.plan.model, Weight: 1, TimeoutMS: 2000,
	}})
	if _, err := pool.Exec(ctx, `INSERT INTO olp.routes (id,slug,created_by,latest_revision,latest_revision_id,etag,project_id)
		VALUES ($1,$2,$3,1,$4,$5,$6)`, routeID, slug, fixture.ownerID, revisionID, uuid.NewString(), fixture.projectID); err != nil {
		g.t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.route_revisions (id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,routing_policy,content_policy,fidelity,behavior)
		VALUES ($1,$2,1,$3,$4,5000,1,$5,$6,$7,'{}',NULL,'{"mode":"transformed"}','{}')`,
		revisionID, routeID, slug, encoded(g.t, g.plan.operations), targets, uuid.NewString(), fixture.ownerID); err != nil {
		g.t.Fatal(err)
	}
	return routeID, revisionID
}

func (g *storedGateway) key(t *testing.T, secret string) string {
	t.Helper()
	keyID := uuid.NewString()
	g.fixture.addKey(t, keyID)
	g.runtime.keys[secret] = keyauthority.Authority{
		ID: keyID, LookupID: "listed" + secret, ProjectID: &g.fixture.projectID,
		Policy: keyauthority.KeyPolicy{Scopes: []string{"inference", "tools"}, AllowedRoutes: []string{g.plan.slug}, AllowProviderState: true, AllowedToolsets: g.plan.toolsets},
	}
	return keyID
}

func TestListedResourcesSkipStaleRowsAndRefillPages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)

	const (
		slug          = "stored-listed"
		forbiddenSlug = "stored-hidden"
		model         = "listed-model"
		tunedModel    = "discovered-tuning-model"
		upstreamJob   = "ftjob-observed"
		tuningPath    = "/v1/fine_tuning/jobs"
	)
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "openai", Model: model, Operation: "fine_tuning", PerRequest: new("0.60"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	stored := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, operations: []string{"batch", "fine_tuning"}, prices: []rates.RoutingPrice{*price},
		upstream: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tuningPath+"/"+upstreamJob {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":%q,"created_at":%d,"status":"succeeded","trained_tokens":12,"fine_tuned_model":%q,"training_file":"file-unbound"}`, upstreamJob, fixture.observedAt.Unix(), tunedModel)
		}),
	})
	store, runtime, mux, httpServer := stored.store, stored.runtime, stored.mux, stored.server
	providerRevisionID, routeRevisionID, slotID, targetID, credentialID := stored.providerRevisionID, stored.routeRevisionID, stored.slotID, stored.targetID, stored.credentialID
	_, forbiddenRevisionID := stored.seedRoute(forbiddenSlug)
	newKey := stored.key
	put := func(t *testing.T, keyID, kind, upstream string, revise func(*resources.Resource)) *resources.Resource {
		t.Helper()
		resource := fixture.resource(kind, keyID, upstream)
		if revise != nil {
			revise(resource)
		}
		stored, err := store.Put(ctx, resource)
		if err != nil {
			t.Fatal(err)
		}
		return stored
	}
	pin := func(resource *resources.Resource) {
		resource.RouteSlug = slug
		resource.ProviderRevisionID = providerRevisionID
		resource.RouteRevisionID = routeRevisionID
		resource.SlotID = slotID
		resource.TargetID = targetID
		resource.CredentialID = &credentialID
	}
	valid := func(t *testing.T, keyID, kind, upstream string) *resources.Resource {
		return put(t, keyID, kind, upstream, pin)
	}
	stale := func(t *testing.T, keyID, kind, upstream string) *resources.Resource {
		return put(t, keyID, kind, upstream, nil)
	}
	get := func(t *testing.T, secret, path string) map[string]json.RawMessage {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
		response, err := httpServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s, want 200", path, response.StatusCode, body)
		}
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("GET %s body %s: %v", path, body, err)
		}
		return decoded
	}
	geminiNames := func(t *testing.T, body map[string]json.RawMessage) []string {
		t.Helper()
		var files []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(body["files"], &files); err != nil {
			t.Fatalf("files %s: %v", body["files"], err)
		}
		names := make([]string, len(files))
		for i, file := range files {
			names[i] = file.Name
		}
		return names
	}
	sameStrings := func(t *testing.T, what string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", what, got, want)
			}
		}
	}

	t.Run("stale and hidden rows are skipped", func(t *testing.T) {
		keyID := newKey(t, "mixed")
		oldest := valid(t, keyID, resources.KindGeminiFile, "mixed-oldest")
		stale(t, keyID, resources.KindGeminiFile, "mixed-stale")
		put(t, keyID, resources.KindGeminiFile, "mixed-hidden", func(resource *resources.Resource) {
			resource.RouteSlug = forbiddenSlug
			resource.ProviderRevisionID = providerRevisionID
			resource.RouteRevisionID = forbiddenRevisionID
			resource.SlotID = slotID
			resource.TargetID = targetID
			resource.CredentialID = &credentialID
		})
		newest := valid(t, keyID, resources.KindGeminiFile, "mixed-newest")
		body := get(t, "mixed", "/gemini/v1beta/files?pageSize=10")
		sameStrings(t, "listed files", geminiNames(t, body), "files/"+newest.ID, "files/"+oldest.ID)
		if _, ok := body["nextPageToken"]; ok {
			t.Fatalf("nextPageToken = %s, want none", body["nextPageToken"])
		}
	})

	t.Run("a full page of stale rows refills from later valid rows", func(t *testing.T) {
		keyID := newKey(t, "refill")
		oldest := valid(t, keyID, resources.KindGeminiFile, "refill-oldest")
		middle := valid(t, keyID, resources.KindGeminiFile, "refill-middle")
		newest := valid(t, keyID, resources.KindGeminiFile, "refill-newest")
		for _, upstream := range []string{"refill-stale-1", "refill-stale-2", "refill-stale-3", "refill-stale-4"} {
			stale(t, keyID, resources.KindGeminiFile, upstream)
		}
		body := get(t, "refill", "/gemini/v1beta/files?pageSize=2")
		sameStrings(t, "first page", geminiNames(t, body), "files/"+newest.ID, "files/"+middle.ID)
		var token string
		if err := json.Unmarshal(body["nextPageToken"], &token); err != nil || token != middle.ID {
			t.Fatalf("nextPageToken = %s, want %q", body["nextPageToken"], middle.ID)
		}
		body = get(t, "refill", "/gemini/v1beta/files?pageSize=2&pageToken="+token)
		sameStrings(t, "second page", geminiNames(t, body), "files/"+oldest.ID)
		if _, ok := body["nextPageToken"]; ok {
			t.Fatalf("nextPageToken = %s, want none", body["nextPageToken"])
		}
	})

	t.Run("an all stale list is empty with no cursor", func(t *testing.T) {
		keyID := newKey(t, "empty")
		for _, upstream := range []string{"empty-stale-1", "empty-stale-2", "empty-stale-3"} {
			stale(t, keyID, resources.KindGeminiFile, upstream)
		}
		body := get(t, "empty", "/gemini/v1beta/files?pageSize=2")
		sameStrings(t, "listed files", geminiNames(t, body))
		if _, ok := body["nextPageToken"]; ok {
			t.Fatalf("nextPageToken = %s, want none", body["nextPageToken"])
		}
	})

	t.Run("continues after the hidden anchor expires", func(t *testing.T) {
		keyID := newKey(t, "expired-anchor")
		oldest := valid(t, keyID, resources.KindGeminiFile, "oldest")
		anchor := stale(t, keyID, resources.KindGeminiFile, "expired")
		for range 399 {
			stale(t, keyID, resources.KindGeminiFile, uuid.NewString())
		}
		newest := valid(t, keyID, resources.KindGeminiFile, "newest")
		const path = "/gemini/v1beta/files?pageSize=1"
		first := get(t, "expired-anchor", path)
		sameStrings(t, "first page", geminiNames(t, first), "files/"+newest.ID)
		var token string
		if err := json.Unmarshal(first["nextPageToken"], &token); err != nil || token == "" || token == newest.ID || strings.Contains(token, anchor.ID) {
			t.Fatalf("nextPageToken = %s, want an opaque continuation", first["nextPageToken"])
		}
		if err := store.Update(ctx, anchor.ID, "", nil, new(time.Now().Add(-time.Minute))); err != nil {
			t.Fatal(err)
		}
		second := get(t, "expired-anchor", path+"&pageToken="+token)
		sameStrings(t, "page after expiry", geminiNames(t, second), "files/"+oldest.ID)
		if _, ok := second["nextPageToken"]; ok {
			t.Fatalf("nextPageToken = %s, want none", second["nextPageToken"])
		}
	})

	anthropicIDs := func(t *testing.T, body map[string]json.RawMessage) ([]string, bool) {
		t.Helper()
		var data []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body["data"], &data); err != nil {
			t.Fatalf("data %s: %v", body["data"], err)
		}
		var hasMore bool
		if err := json.Unmarshal(body["has_more"], &hasMore); err != nil {
			t.Fatalf("has_more %s: %v", body["has_more"], err)
		}
		ids := make([]string, len(data))
		for i, entry := range data {
			ids[i] = entry.ID
		}
		return ids, hasMore
	}
	t.Run("anthropic backward pages refill past stale rows", func(t *testing.T) {
		keyID := newKey(t, "backward")
		anchor := valid(t, keyID, resources.KindAnthropicFile, "backward-anchor")
		next := valid(t, keyID, resources.KindAnthropicFile, "backward-next")
		stale(t, keyID, resources.KindAnthropicFile, "backward-stale")
		newest := valid(t, keyID, resources.KindAnthropicFile, "backward-newest")
		ids, hasMore := anthropicIDs(t, get(t, "backward", "/anthropic/v1/files?limit=1&before_id="+anchor.ID))
		sameStrings(t, "first backward page", ids, next.ID)
		if !hasMore {
			t.Fatal("first backward page has_more = false, want true")
		}
		ids, hasMore = anthropicIDs(t, get(t, "backward", "/anthropic/v1/files?limit=1&before_id="+next.ID))
		sameStrings(t, "second backward page", ids, newest.ID)
		if hasMore {
			t.Fatal("second backward page has_more = true, want false")
		}
	})

	t.Run("anthropic batch pages skip a full scan of stale bindings", func(t *testing.T) {
		const secret = "batch-pages"
		keyID := newKey(t, secret)
		oldest := valid(t, keyID, resources.KindAnthropicBatch, "batch-oldest")
		for range 405 {
			stale(t, keyID, resources.KindAnthropicBatch, uuid.NewString())
		}
		newest := valid(t, keyID, resources.KindAnthropicBatch, "batch-newest")
		const path = "/anthropic/v1/messages/batches?limit=1"
		for _, tc := range []struct{ cursorParam, anchor, want string }{
			{"after_id", newest.ID, oldest.ID},
			{"before_id", oldest.ID, newest.ID},
		} {
			ids, more := anthropicIDs(t, get(t, secret, path+"&"+tc.cursorParam+"="+tc.anchor))
			sameStrings(t, tc.cursorParam, ids, tc.want)
			if more {
				t.Fatalf("%s has_more = true after the last batch", tc.cursorParam)
			}
		}
	})

	seedTuningJob := func(t *testing.T, keyID string) *resources.Resource {
		t.Helper()
		return put(t, keyID, "fine_tuning_job", upstreamJob, func(resource *resources.Resource) {
			pin(resource)
			resource.State = "running"
			resource.ContractVersion = new(retainedapi.Revision)
			resource.Metadata = json.RawMessage(fmt.Sprintf(`{"upstream_model":%q}`, model))
		})
	}
	expect := func(t *testing.T, secret, path string, status int, code string) {
		t.Helper()
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+secret)
		writer := httptest.NewRecorder()
		mux.ServeHTTP(writer, request)
		if writer.Code != status || !strings.Contains(writer.Body.String(), code) {
			t.Fatalf("GET %s returned %d %s, want %d %s", path, writer.Code, writer.Body, status, code)
		}
	}
	t.Run("failed file binding keeps verified tuning results", func(t *testing.T) {
		const secret = "tuning-get"
		keyID := newKey(t, secret)
		job := seedTuningJob(t, keyID)
		expect(t, secret, tuningPath+"/"+job.ID, http.StatusBadGateway, "provider_protocol_error")
		saved, err := store.Get(ctx, job.Kind, keyID, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.State != "succeeded" {
			t.Fatalf("job state = %q after failed file binding", saved.State)
		}
		var enabled bool
		if err := pool.QueryRow(ctx, `SELECT enabled FROM olp.provider_models WHERE provider_id=$1 AND upstream_model=$2`, fixture.providerID, tunedModel).Scan(&enabled); err != nil || enabled {
			t.Fatalf("discovered model enabled = %v, err = %v", enabled, err)
		}
	})
	t.Run("unresolved priced read keeps the route cost hold", func(t *testing.T) {
		const secret = "tuning-list"
		keyID := newKey(t, secret)
		authority := runtime.keys[secret]
		authority.Policy.RouteLimits = keyauthority.RouteLimits{slug: {DailyCostLimit: new("1.00"), MaxConcurrency: new(int64(1))}}
		runtime.keys[secret] = authority
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := ledger.EnsureKeyRouteBudget(ctx, tx, keyID, slug); err != nil {
			t.Fatal(err)
		}
		if err := ledger.OpenFreshOwner(ctx, tx, limits.OwnerAggregate, limits.KeyRouteBudgetID(keyID, slug)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		seedTuningJob(t, keyID)
		expect(t, secret, tuningPath, http.StatusBadGateway, "provider_protocol_error")
		expect(t, secret, tuningPath, http.StatusTooManyRequests, "budget_exhausted")
	})
}

type upstreamCall struct {
	request, key, authorization, body string
	beta, accept                      string
}

type retainedUpstream struct {
	mu      sync.Mutex
	calls   []upstreamCall
	replies func(request string, nth int) map[string]string
	headers func(*http.Request) map[string]string
}

const providerOwned = "provider-owned"

var providerPrefix = regexp.MustCompile(`^/v1(beta)?(/openai(/deployments/[^/]+)?)?`)

func (u *retainedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	request := r.Method + " " + providerPrefix.ReplaceAllString(r.URL.Path, "")
	u.mu.Lock()
	u.calls = append(u.calls, upstreamCall{
		request: request, key: cmp.Or(r.Header.Get("api-key"), r.Header.Get("x-api-key"), r.Header.Get("x-goog-api-key")),
		authorization: r.Header.Get("Authorization"), body: string(body),
		beta: r.Header.Get("Anthropic-Beta"), accept: r.Header.Get("Accept"),
	})
	nth := 0
	for _, call := range u.calls {
		if call.request == request {
			nth++
		}
	}
	u.mu.Unlock()
	reply, known := u.replies(request, nth)[request]
	if !known {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if u.headers != nil {
		for name, value := range u.headers(r) {
			w.Header().Set(name, value)
		}
	}
	_, _ = io.WriteString(w, reply)
}

func openAIReplies(_ string, nth int) map[string]string {
	const file = `{"id":"file-provider-owned","object":"file","bytes":7,"created_at":1700000000,"filename":"batch.jsonl","purpose":"batch"}`
	batch := func(status string) string {
		return `{"id":"batch_provider-owned","object":"batch","endpoint":"/v1/chat/completions","input_file_id":"file-provider-owned","completion_window":"24h","status":"` + status + `","created_at":1700000000}`
	}
	response := func(id string) string {
		return `{"id":"` + id + `","object":"response","created_at":1700000000,"status":"completed","model":"provider-model","output":[{"id":"msg_2","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	}
	replies := map[string]string{
		"POST /files":                                      file,
		"GET /files/file-provider-owned":                   file,
		"GET /files/file-provider-owned/content":           "CONTENT",
		"DELETE /files/file-provider-owned":                `{"id":"file-provider-owned","object":"file","deleted":true}`,
		"POST /batches":                                    batch("validating"),
		"GET /batches/batch_provider-owned":                batch("in_progress"),
		"POST /batches/batch_provider-owned/cancel":        batch("cancelling"),
		"POST /responses":                                  response(fmt.Sprintf("resp_provider-owned-%d", nth)),
		"GET /responses/resp_provider-owned-1":             response("resp_provider-owned-1"),
		"GET /responses/resp_provider-owned-1/input_items": `{"object":"list","data":[{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"first_id":"msg_1","last_id":"msg_1","has_more":false}`,
		"DELETE /responses/resp_provider-owned-1":          `{"id":"resp_provider-owned-1","object":"response.deleted","deleted":true}`,
	}
	return replies
}

func anthropicReplies(string, int) map[string]string {
	const file = `{"id":"file_provider-owned","type":"file","filename":"a.txt","mime_type":"text/plain","size_bytes":5,"created_at":"2026-01-01T00:00:00Z","downloadable":true}`
	batch := func(status string) string {
		return `{"id":"msgbatch_provider-owned","type":"message_batch","processing_status":"` + status + `","request_counts":{"processing":1,"succeeded":0,"errored":0,"canceled":0,"expired":0},"created_at":"2026-01-01T00:00:00Z","expires_at":"2026-01-02T00:00:00Z","results_url":null}`
	}
	return map[string]string{
		"POST /files":                                           file,
		"GET /files/file_provider-owned":                        file,
		"GET /files/file_provider-owned/content":                "CONTENT",
		"DELETE /files/file_provider-owned":                     `{"id":"file_provider-owned","type":"file_deleted"}`,
		"POST /messages/batches":                                batch("in_progress"),
		"GET /messages/batches/msgbatch_provider-owned":         batch("in_progress"),
		"POST /messages/batches/msgbatch_provider-owned/cancel": batch("canceling"),
		"GET /messages/batches/msgbatch_provider-owned/results": `{"custom_id":"one","result":{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}}}` + "\n",
	}
}

func (u *retainedUpstream) sent(request string) []upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(u.calls), func(call upstreamCall) bool { return call.request != request })
}

type retainedObject struct {
	ID, Status       string
	InputFileID      string `json:"input_file_id"`
	ProcessingStatus string `json:"processing_status"`
	VectorStoreID    string `json:"vector_store_id"`
	Deleted          bool
	Data             []struct{ ID string }
}

func (o retainedObject) identifiers() []string {
	ids := make([]string, len(o.Data))
	for i, entry := range o.Data {
		ids[i] = entry.ID
	}
	return ids
}

func (g *storedGateway) send(t *testing.T, method, path string, headers map[string]string, body string, want int) (http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, g.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-OLP-Route", g.plan.slug)
	for name, value := range headers {
		if value != "" {
			request.Header.Set(name, value)
		}
	}
	response, err := g.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != want {
		t.Fatalf("%s %s with %v = %d, want %d: %s", method, path, headers, response.StatusCode, want, raw)
	}
	if strings.Contains(string(raw)+fmt.Sprint(response.Header), providerOwned) {
		t.Fatalf("%s %s exposed a provider identifier: %v %s", method, path, response.Header, raw)
	}
	return response.Header, raw
}

func (g *storedGateway) call(t *testing.T, key, method, path, contentType, body string, want int) retainedObject {
	t.Helper()
	_, raw := g.send(t, method, path, map[string]string{"Authorization": "Bearer " + key, "Content-Type": contentType}, body, want)
	var decoded retainedObject
	_ = json.Unmarshal(raw, &decoded)
	return decoded
}

func TestFilesAndBatchesHideProviderIdentifiersIsolateKeysAndSpendTheProviderCredential(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "stored-batch", "batch-model"
	upstream := &retainedUpstream{replies: openAIReplies}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	line := `{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + slug + `","messages":[{"role":"user","content":"hi"}]}}`
	form := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n" + line + "\n\r\n--b--\r\n"
	file := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if file.ID == "" {
		t.Fatal("upload returned no file identifier")
	}
	uploads := upstream.sent("POST /files")
	if len(uploads) != 1 || uploads[0].key != "listed-secret" || uploads[0].authorization != "" {
		t.Fatalf("upload reached the provider as %+v, want one call carrying only the provider credential", uploads)
	}
	if !strings.Contains(uploads[0].body, `"model":"`+model+`"`) || strings.Contains(uploads[0].body, slug) {
		t.Fatalf("uploaded lines must name the provider model instead of the route: %s", uploads[0].body)
	}
	if got := g.call(t, "owner", "GET", "/v1/files/"+file.ID, "", "", 200); got.ID != file.ID {
		t.Fatalf("file read returned %q, want %q", got.ID, file.ID)
	}
	if got := g.call(t, "owner", "GET", "/v1/files", "", "", 200).identifiers(); !slices.Equal(got, []string{file.ID}) {
		t.Fatalf("owner lists files %v, want only %s", got, file.ID)
	}
	g.call(t, "owner", "GET", "/v1/files/"+file.ID+"/content", "", "", 200)

	g.call(t, "stranger", "GET", "/v1/files/"+file.ID, "", "", 404)
	if got := g.call(t, "stranger", "GET", "/v1/files", "", "", 200).identifiers(); len(got) != 0 {
		t.Fatalf("another key lists files %v", got)
	}
	submission := `{"input_file_id":"` + file.ID + `","endpoint":"/v1/chat/completions","completion_window":"24h"}`
	g.call(t, "stranger", "POST", "/v1/batches", "application/json", submission, 404)
	if sent := upstream.sent("POST /batches"); len(sent) != 0 {
		t.Fatalf("a batch over another key's file reached the provider: %+v", sent)
	}

	batch := g.call(t, "owner", "POST", "/v1/batches", "application/json", submission, 200)
	if batch.ID == "" || batch.InputFileID != file.ID || batch.Status != "validating" {
		t.Fatalf("batch = %+v, want a gateway identifier over file %s", batch, file.ID)
	}
	submissions := upstream.sent("POST /batches")
	if len(submissions) != 1 || !strings.Contains(submissions[0].body, `"input_file_id":"file-provider-owned"`) || submissions[0].key != "listed-secret" {
		t.Fatalf("batch reached the provider as %+v, want the provider's file identifier and credential", submissions)
	}
	if got := g.call(t, "owner", "GET", "/v1/batches/"+batch.ID, "", "", 200); got.ID != batch.ID || got.InputFileID != file.ID || got.Status != "in_progress" {
		t.Fatalf("batch read = %+v", got)
	}
	if got := g.call(t, "owner", "GET", "/v1/batches", "", "", 200).identifiers(); !slices.Equal(got, []string{batch.ID}) {
		t.Fatalf("owner lists batches %v, want only %s", got, batch.ID)
	}
	g.call(t, "stranger", "GET", "/v1/batches/"+batch.ID, "", "", 404)
	g.call(t, "stranger", "POST", "/v1/batches/"+batch.ID+"/cancel", "", "", 404)
	if got := g.call(t, "owner", "POST", "/v1/batches/"+batch.ID+"/cancel", "", "", 200); got.ID != batch.ID || got.Status != "cancelling" {
		t.Fatalf("batch cancel = %+v", got)
	}
	if sent := upstream.sent("POST /batches/batch_provider-owned/cancel"); len(sent) != 1 {
		t.Fatalf("the provider saw %d cancels, want only the owner's", len(sent))
	}

	g.call(t, "stranger", "DELETE", "/v1/files/"+file.ID, "", "", 404)
	if got := g.call(t, "owner", "DELETE", "/v1/files/"+file.ID, "", "", 200); !got.Deleted || got.ID != file.ID {
		t.Fatalf("file delete = %+v", got)
	}
	if sent := upstream.sent("DELETE /files/file-provider-owned"); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want only the owner's", len(sent))
	}
	g.call(t, "owner", "GET", "/v1/files/"+file.ID, "", "", 404)
}

func TestStoredResponsesHideProviderIdentifiersContinueOnTheirProviderAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "stored-responses", "responses-model"
	upstream := &retainedUpstream{replies: openAIReplies}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const json = "application/json"

	first := g.call(t, "owner", "POST", "/v1/responses", json, `{"model":"`+slug+`","input":"hi","store":true}`, 200)
	if first.ID == "" {
		t.Fatal("stored response returned no identifier")
	}
	created := upstream.sent("POST /responses")
	if len(created) != 1 || created[0].key != "listed-secret" || created[0].authorization != "" || !strings.Contains(created[0].body, `"model":"`+model+`"`) {
		t.Fatalf("response reached the provider as %+v, want the provider model and credential only", created)
	}
	if got := g.call(t, "owner", "GET", "/v1/responses/"+first.ID, "", "", 200); got.ID != first.ID {
		t.Fatalf("response read returned %q, want %q", got.ID, first.ID)
	}
	if got := g.call(t, "owner", "GET", "/v1/responses/"+first.ID+"/input_items", "", "", 200); len(got.Data) != 1 {
		t.Fatalf("input items = %+v, want the provider's one item", got)
	}

	continuation := `{"model":"` + slug + `","input":"again","previous_response_id":"` + first.ID + `","store":true}`
	g.call(t, "stranger", "GET", "/v1/responses/"+first.ID, "", "", 404)
	g.call(t, "stranger", "GET", "/v1/responses/"+first.ID+"/input_items", "", "", 404)
	g.call(t, "stranger", "POST", "/v1/responses", json, continuation, 400)
	g.call(t, "stranger", "DELETE", "/v1/responses/"+first.ID, "", "", 404)
	if sent := upstream.sent("POST /responses"); len(sent) != 1 {
		t.Fatalf("another key's continuation reached the provider: %+v", sent)
	}

	second := g.call(t, "owner", "POST", "/v1/responses", json, continuation, 200)
	if second.ID == "" || second.ID == first.ID {
		t.Fatalf("continuation returned %q after %q, want a new identifier", second.ID, first.ID)
	}
	if sent := upstream.sent("POST /responses"); len(sent) != 2 || !strings.Contains(sent[1].body, `"previous_response_id":"resp_provider-owned-1"`) {
		t.Fatalf("continuation reached the provider as %+v, want the provider's identifier of the first response", sent)
	}

	if got := g.call(t, "owner", "DELETE", "/v1/responses/"+first.ID, "", "", 200); !got.Deleted || got.ID != first.ID {
		t.Fatalf("response delete = %+v", got)
	}
	if sent := upstream.sent("DELETE /responses/resp_provider-owned-1"); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want only the owner's", len(sent))
	}
	g.call(t, "owner", "GET", "/v1/responses/"+first.ID, "", "", 404)
}

func TestAnthropicFilesAndBatchesHideProviderIdentifiersIsolateKeysAndKeepUnsettledBatches(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "claude-route", "claude-model"
	upstream := &retainedUpstream{replies: anthropicReplies}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "anthropic", surface: "anthropic", operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const files, batches = "/anthropic/v1/files", "/anthropic/v1/messages/batches"

	upload := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	file := g.call(t, "owner", "POST", files, "multipart/form-data; boundary=b", upload, 200)
	if file.ID == "" {
		t.Fatal("upload returned no file identifier")
	}
	uploaded := upstream.sent("POST /files")
	if len(uploaded) != 1 || uploaded[0].key != "listed-secret" || uploaded[0].authorization != "" || !strings.Contains(uploaded[0].body, "hello") {
		t.Fatalf("upload reached the provider as %+v, want the file under the provider credential only", uploaded)
	}
	if got := g.call(t, "owner", "GET", files+"/"+file.ID, "", "", 200); got.ID != file.ID {
		t.Fatalf("file read returned %q, want %q", got.ID, file.ID)
	}
	if got := g.call(t, "owner", "GET", files, "", "", 200).identifiers(); !slices.Equal(got, []string{file.ID}) {
		t.Fatalf("owner lists files %v, want %v", got, []string{file.ID})
	}
	g.call(t, "owner", "GET", files+"/"+file.ID+"/content", "", "", 200)

	submission := `{"requests":[{"custom_id":"one","params":{"model":"` + slug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}}]}`
	batch := g.call(t, "owner", "POST", batches, "application/json", submission, 200)
	if batch.ID == "" {
		t.Fatal("batch creation returned no identifier")
	}
	submitted := upstream.sent("POST /messages/batches")
	if len(submitted) != 1 || !strings.Contains(submitted[0].body, `"model":"`+model+`"`) || strings.Contains(submitted[0].body, slug) {
		t.Fatalf("batch reached the provider as %+v, want the provider model in place of the route", submitted)
	}
	if got := g.call(t, "owner", "GET", batches+"/"+batch.ID, "", "", 200); got.ID != batch.ID {
		t.Fatalf("batch read returned %q, want %q", got.ID, batch.ID)
	}
	if got := g.call(t, "owner", "GET", batches, "", "", 200).identifiers(); !slices.Equal(got, []string{batch.ID}) {
		t.Fatalf("owner lists batches %v, want %v", got, []string{batch.ID})
	}

	if got := g.call(t, "stranger", "GET", files, "", "", 200).identifiers(); len(got) != 0 {
		t.Fatalf("another key lists files %v", got)
	}
	if got := g.call(t, "stranger", "GET", batches, "", "", 200).identifiers(); len(got) != 0 {
		t.Fatalf("another key lists batches %v", got)
	}
	g.call(t, "stranger", "GET", files+"/"+file.ID, "", "", 404)
	g.call(t, "stranger", "GET", files+"/"+file.ID+"/content", "", "", 404)
	g.call(t, "stranger", "DELETE", files+"/"+file.ID, "", "", 404)
	g.call(t, "stranger", "GET", batches+"/"+batch.ID, "", "", 404)
	g.call(t, "stranger", "GET", batches+"/"+batch.ID+"/results", "", "", 404)
	g.call(t, "stranger", "POST", batches+"/"+batch.ID+"/cancel", "", "", 404)
	g.call(t, "stranger", "DELETE", batches+"/"+batch.ID, "", "", 404)

	if got := g.call(t, "owner", "POST", batches+"/"+batch.ID+"/cancel", "", "", 200); got.ID != batch.ID || got.ProcessingStatus != "canceling" {
		t.Fatalf("batch cancel = %+v", got)
	}
	g.call(t, "owner", "GET", batches+"/"+batch.ID+"/results", "", "", 200)
	g.call(t, "owner", "DELETE", batches+"/"+batch.ID, "", "", 409)
	if got := g.call(t, "owner", "GET", batches+"/"+batch.ID, "", "", 200); got.ID != batch.ID {
		t.Fatalf("an unsettled batch disappeared after a refused delete: %+v", got)
	}
	if got := g.call(t, "owner", "DELETE", files+"/"+file.ID, "", "", 200); got.ID != file.ID {
		t.Fatalf("file delete = %+v", got)
	}
	g.call(t, "owner", "GET", files+"/"+file.ID, "", "", 404)
	for request, want := range map[string]int{
		"POST /messages/batches/msgbatch_provider-owned/cancel": 1,
		"GET /messages/batches/msgbatch_provider-owned/results": 1,
		"DELETE /messages/batches/msgbatch_provider-owned":      0,
		"DELETE /files/file_provider-owned":                     1,
	} {
		if sent := upstream.sent(request); len(sent) != want {
			t.Fatalf("the provider saw %d of %q, want %d", len(sent), request, want)
		}
	}
	fileBeta := uploaded[0].beta
	if fileBeta == "" {
		t.Fatal("the upload did not opt into the provider's file API")
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	for _, sent := range upstream.calls {
		onFile := strings.Contains(sent.request, " /files")
		if onFile && sent.beta != fileBeta || !onFile && sent.beta != "" {
			t.Fatalf("%q reached the provider with beta %q, want the file API opt-in on file requests only", sent.request, sent.beta)
		}
		download := strings.HasSuffix(sent.request, "/content")
		if binary := sent.accept == "application/binary"; binary != download {
			t.Fatalf("%q asked the provider for %q, want binary for a file download only", sent.request, sent.accept)
		}
	}
}

func geminiReplies(_ string, nth int) map[string]string {
	now := time.Now().UTC()
	file := fmt.Sprintf(`{"name":"files/%[1]s","displayName":"notes","mimeType":"text/plain","sizeBytes":"5","createTime":%[2]q,"updateTime":%[2]q,"expirationTime":%[3]q,"sha256Hash":"aGFzaA==","uri":"https://provider.invalid/v1beta/files/%[1]s","state":"ACTIVE"}`,
		providerOwned, now.Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	interaction := func(n int) string {
		return fmt.Sprintf(`{"id":"interaction-%s-%d","status":"completed","model":"gemini-model","steps":[{"type":"model_output","content":[{"type":"text","text":"hello"}]}],"usage":{"total_input_tokens":3,"total_output_tokens":2,"total_tokens":5}}`, providerOwned, n)
	}
	return map[string]string{
		"POST /upload/v1beta/files":                                "",
		"POST /upload/session-" + providerOwned:                    `{"file":` + file + `}`,
		"GET /files/" + providerOwned:                              file,
		"DELETE /files/" + providerOwned:                           `{}`,
		"POST /interactions":                                       interaction(nth),
		"GET /interactions/interaction-" + providerOwned + "-1":    interaction(1),
		"DELETE /interactions/interaction-" + providerOwned + "-1": `{}`,
	}
}

func geminiUploadHeaders(r *http.Request) map[string]string {
	switch r.Header.Get("X-Goog-Upload-Command") {
	case "start":
		return map[string]string{"X-Goog-Upload-URL": "https://" + r.Host + "/upload/session-" + providerOwned, "X-Goog-Upload-Status": "active"}
	case "upload, finalize":
		return map[string]string{"X-Goog-Upload-Status": "final"}
	}
	return nil
}

func geminiKey(key string, extra map[string]string) map[string]string {
	headers := maps.Clone(extra)
	if headers == nil {
		headers = map[string]string{}
	}
	headers["x-goog-api-key"] = key
	return headers
}

type geminiFile struct {
	Name, URI string
}

func TestGeminiFilesUploadThroughAGatewaySessionHideProviderIdentifiersAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	upstream := &retainedUpstream{replies: geminiReplies, headers: geminiUploadHeaders}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "gemini-files", model: "gemini-model", kind: "gemini", surface: "gemini", version: "/v1beta", secure: true,
		operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const files = "/gemini/v1beta/"

	started, _ := g.send(t, "POST", "/gemini/upload/v1beta/files", geminiKey("owner", map[string]string{
		"Content-Type": "application/json", "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start",
		"X-Goog-Upload-Header-Content-Length": "5", "X-Goog-Upload-Header-Content-Type": "text/plain",
	}), `{"file":{"display_name":"notes"}}`, 200)
	session, found := strings.CutPrefix(started.Get("X-Goog-Upload-URL"), g.server.URL)
	if !found || !strings.HasPrefix(session, "/gemini/upload/") {
		t.Fatalf("upload session is %q, want a session on the gateway", started.Get("X-Goog-Upload-URL"))
	}
	finalize := map[string]string{"X-Goog-Upload-Command": "upload, finalize", "X-Goog-Upload-Offset": "0"}
	g.send(t, "POST", session, geminiKey("stranger", finalize), "stolen", 404)
	if sent := upstream.sent("POST /upload/session-" + providerOwned); len(sent) != 0 {
		t.Fatalf("another key's bytes reached the provider session: %+v", sent)
	}
	_, raw := g.send(t, "POST", session, geminiKey("owner", finalize), "hello", 200)
	var uploaded struct{ File geminiFile }
	if err := json.Unmarshal(raw, &uploaded); err != nil || !strings.HasPrefix(uploaded.File.Name, "files/") || uploaded.File.URI != g.server.URL+files+uploaded.File.Name {
		t.Fatalf("finalized upload = %s, want a file named and addressed by the gateway", raw)
	}
	name := uploaded.File.Name
	sent := upstream.sent("POST /upload/session-" + providerOwned)
	if len(sent) != 1 || sent[0].key != "listed-secret" || sent[0].body != "hello" {
		t.Fatalf("the provider session received %+v, want the owner's bytes under the provider credential", sent)
	}

	listed := func(key string) []string {
		_, raw := g.send(t, "GET", files+"files", geminiKey(key, nil), "", 200)
		var page struct{ Files []geminiFile }
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(page.Files))
		for i, file := range page.Files {
			names[i] = file.Name
		}
		return names
	}
	_, raw = g.send(t, "GET", files+name, geminiKey("owner", nil), "", 200)
	var read geminiFile
	if err := json.Unmarshal(raw, &read); err != nil || read.Name != name {
		t.Fatalf("file read = %s, want %q", raw, name)
	}
	if got := listed("owner"); !slices.Equal(got, []string{name}) {
		t.Fatalf("owner lists files %v, want %v", got, []string{name})
	}
	if got := listed("stranger"); len(got) != 0 {
		t.Fatalf("another key lists files %v", got)
	}
	g.send(t, "GET", files+name, geminiKey("stranger", nil), "", 404)
	g.send(t, "DELETE", files+name, geminiKey("stranger", nil), "", 404)

	g.send(t, "DELETE", files+name, geminiKey("owner", nil), "", 200)
	g.send(t, "GET", files+name, geminiKey("owner", nil), "", 404)
	if got := listed("owner"); len(got) != 0 {
		t.Fatalf("a deleted file is still listed: %v", got)
	}
	if sent := upstream.sent("DELETE /files/" + providerOwned); len(sent) != 1 || sent[0].key != "listed-secret" {
		t.Fatalf("the provider saw deletes %+v, want one under the provider credential", sent)
	}
}

func TestGeminiInteractionsHideProviderIdentifiersContinueOnTheirProviderAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "gemini-interactions", "gemini-model"
	upstream := &retainedUpstream{replies: geminiReplies}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "gemini", surface: "gemini", profile: "gemini-interactions", version: "/v1beta", secure: true,
		operations: []string{"generation"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const interactions = "/gemini/v1beta/interactions"
	type interaction struct{ ID, Model, Status string }
	create := func(key, body string, want int) interaction {
		t.Helper()
		_, raw := g.send(t, "POST", interactions, geminiKey(key, map[string]string{"Content-Type": "application/json"}), body, want)
		var decoded interaction
		_ = json.Unmarshal(raw, &decoded)
		return decoded
	}

	first := create("owner", `{"model":"`+slug+`","input":"hi"}`, 200)
	if first.ID == "" || first.Model != slug || first.Status != "completed" {
		t.Fatalf("interaction = %+v, want a gateway identifier under the route name", first)
	}
	created := upstream.sent("POST /interactions")
	if len(created) != 1 || created[0].key != "listed-secret" || !strings.Contains(created[0].body, `"model":"`+model+`"`) || strings.Contains(created[0].body, slug) {
		t.Fatalf("interaction reached the provider as %+v, want the provider model under the provider credential", created)
	}
	_, raw := g.send(t, "GET", interactions+"/"+first.ID, geminiKey("owner", nil), "", 200)
	var read interaction
	if err := json.Unmarshal(raw, &read); err != nil || read.ID != first.ID || read.Model != slug {
		t.Fatalf("interaction read = %s, want %q under the route name", raw, first.ID)
	}

	g.send(t, "GET", interactions+"/"+first.ID, geminiKey("stranger", nil), "", 404)
	g.send(t, "DELETE", interactions+"/"+first.ID, geminiKey("stranger", nil), "", 404)
	create("stranger", `{"model":"`+slug+`","input":"again","previous_interaction_id":"`+first.ID+`"}`, 400)
	create("stranger", `{"model":"`+slug+`","input":"again","previous_interaction_id":"interaction_0123456789abcdef0123456789abcdef"}`, 400)
	if sent := upstream.sent("POST /interactions"); len(sent) != 1 {
		t.Fatalf("another key's continuation reached the provider: %+v", sent)
	}

	second := create("owner", `{"model":"`+slug+`","input":"again","previous_interaction_id":"`+first.ID+`"}`, 200)
	if second.ID == "" || second.ID == first.ID {
		t.Fatalf("continuation = %+v, want its own identifier", second)
	}
	continued := upstream.sent("POST /interactions")
	if len(continued) != 2 || !strings.Contains(continued[1].body, `"previous_interaction_id":"interaction-`+providerOwned+`-1"`) || strings.Contains(continued[1].body, first.ID) {
		t.Fatalf("continuation reached the provider as %+v, want the provider's parent identifier", continued)
	}

	g.send(t, "DELETE", interactions+"/"+first.ID, geminiKey("owner", nil), "", 200)
	g.send(t, "GET", interactions+"/"+first.ID, geminiKey("owner", nil), "", 404)
	if sent := upstream.sent("DELETE /interactions/interaction-" + providerOwned + "-1"); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want 1", len(sent))
	}
}

func vectorStoreReplies(string, int) map[string]string {
	store, path := `{"id":"vs_`+providerOwned+`","object":"vector_store","name":"notes","status":"completed","created_at":1,"file_counts":{"total":0}}`, "/vector_stores/vs_"+providerOwned
	file := `{"id":"file-` + providerOwned + `","object":"vector_store.file","vector_store_id":"vs_` + providerOwned + `","status":"completed","created_at":1}`
	return map[string]string{
		"POST /files":             `{"id":"file-` + providerOwned + `","object":"file","bytes":5,"created_at":1,"filename":"a.txt","purpose":"assistants","status":"processed"}`,
		"POST /vector_stores":     store,
		"GET " + path:             store,
		"POST " + path:            store,
		"DELETE " + path:          `{"id":"vs_` + providerOwned + `","object":"vector_store.deleted","deleted":true}`,
		"POST " + path + "/files": file,
		"GET " + path + "/files":  `{"object":"list","data":[` + file + `],"has_more":false}`,
		"GET " + path + "/files/file-" + providerOwned: file,
	}
}

func TestVectorStoresHideProviderIdentifiersBindFilesReplayCreationAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	upstream := &retainedUpstream{replies: vectorStoreReplies}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const stores = "/v1/vector_stores"
	submit := func(key, path, contentType, body, submission string, want int) retainedObject {
		t.Helper()
		_, raw := g.send(t, "POST", path, map[string]string{"Authorization": "Bearer " + key, "Content-Type": contentType, "Idempotency-Key": submission}, body, want)
		var decoded retainedObject
		_ = json.Unmarshal(raw, &decoded)
		return decoded
	}

	upload := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nassistants\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	file := submit("owner", "/v1/files", "multipart/form-data; boundary=b", upload, uuid.NewString(), 200)
	creation := uuid.NewString()
	store := submit("owner", stores, "application/json", `{"name":"notes"}`, creation, 200)
	if file.ID == "" || store.ID == "" {
		t.Fatalf("creation returned file %q and store %q, want gateway identifiers", file.ID, store.ID)
	}
	replayed, _ := g.send(t, "POST", stores, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json", "Idempotency-Key": creation}, `{"name":"notes"}`, 409)
	if got := replayed.Get("X-OLP-Resource-ID"); got != store.ID {
		t.Fatalf("a replayed creation points at %q, want %q", got, store.ID)
	}
	if created := upstream.sent("POST /vector_stores"); len(created) != 1 || created[0].key != "listed-secret" {
		t.Fatalf("the provider saw creations %+v, want one under the provider credential", created)
	}
	own := stores + "/" + store.ID
	if got := g.call(t, "owner", "GET", own, "", "", 200); got.ID != store.ID {
		t.Fatalf("store read returned %q, want %q", got.ID, store.ID)
	}
	if got := g.call(t, "owner", "GET", stores, "", "", 200).identifiers(); !slices.Equal(got, []string{store.ID}) {
		t.Fatalf("owner lists stores %v, want %v", got, []string{store.ID})
	}
	if got := submit("owner", own, "application/json", `{"name":"renamed"}`, "", 200); got.ID != store.ID {
		t.Fatalf("store update returned %q, want %q", got.ID, store.ID)
	}

	attached := submit("owner", own+"/files", "application/json", `{"file_id":"`+file.ID+`"}`, uuid.NewString(), 200)
	if attached.ID == "" || attached.VectorStoreID != store.ID {
		t.Fatalf("attachment = %+v, want a gateway identifier inside store %q", attached, store.ID)
	}
	bound := upstream.sent("POST /vector_stores/vs_" + providerOwned + "/files")
	if len(bound) != 1 || !strings.Contains(bound[0].body, `"file_id":"file-`+providerOwned+`"`) || strings.Contains(bound[0].body, file.ID) {
		t.Fatalf("attachment reached the provider as %+v, want the provider's file identifier", bound)
	}
	if got := g.call(t, "owner", "GET", own+"/files/"+attached.ID, "", "", 200); got.ID != attached.ID || got.VectorStoreID != store.ID {
		t.Fatalf("attachment read = %+v, want %q inside %q", got, attached.ID, store.ID)
	}
	if got := g.call(t, "owner", "GET", own+"/files", "", "", 200).identifiers(); !slices.Equal(got, []string{attached.ID}) {
		t.Fatalf("owner lists attachments %v, want %v", got, []string{attached.ID})
	}

	seen := len(upstream.sent("GET /vector_stores/vs_"+providerOwned)) + len(upstream.sent("POST /vector_stores/vs_"+providerOwned+"/files"))
	if got := g.call(t, "stranger", "GET", stores, "", "", 200).identifiers(); len(got) != 0 {
		t.Fatalf("another key lists stores %v", got)
	}
	g.call(t, "stranger", "GET", own, "", "", 404)
	g.call(t, "stranger", "GET", own+"/files", "", "", 404)
	g.call(t, "stranger", "GET", own+"/files/"+attached.ID, "", "", 404)
	g.call(t, "stranger", "DELETE", own, "", "", 404)
	submit("stranger", own, "application/json", `{"name":"taken"}`, "", 404)
	submit("stranger", own+"/files", "application/json", `{"file_id":"`+file.ID+`"}`, uuid.NewString(), 404)
	if after := len(upstream.sent("GET /vector_stores/vs_"+providerOwned)) + len(upstream.sent("POST /vector_stores/vs_"+providerOwned+"/files")); after != seen {
		t.Fatalf("another key's requests reached the provider %d times", after-seen)
	}

	if got := g.call(t, "owner", "DELETE", own, "", "", 200); got.ID != store.ID || !got.Deleted {
		t.Fatalf("store delete = %+v", got)
	}
	g.call(t, "owner", "GET", own, "", "", 404)
	if sent := upstream.sent("DELETE /vector_stores/vs_" + providerOwned); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want 1", len(sent))
	}
}

type routeTools struct {
	mu    sync.Mutex
	calls []toolCall
}

type toolCall struct {
	run       string
	arguments string
}

func (*routeTools) Resolve(_ context.Context, _ keyauthority.Authority, _ *release.Release, toolsets []string) ([]kernel.WorkflowTool, error) {
	price := "0.01"
	return []kernel.WorkflowTool{{ToolsetID: toolsets[0], RevisionID: uuid.NewString(), Definition: routing.MCPToolsetTool{
		Classification: "read_only", Alias: "lookup", Name: "lookup", Description: "Look up a fact.", UnitPrice: &price,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`),
	}}}, nil
}

func (*routeTools) AuthorizeWorkflowTool(keyauthority.Authority, kernel.WorkflowTool) error {
	return nil
}

func (r *routeTools) Call(_ context.Context, _ keyauthority.Authority, _ *release.Release, _ kernel.WorkflowTool, run string, _ int, _ string, arguments json.RawMessage) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, toolCall{run: run, arguments: string(arguments)})
	return json.RawMessage(`{"content":[{"type":"text","text":"forty-two"}]}`), nil
}

func (*routeTools) CallApproved(context.Context, keyauthority.Authority, *release.Release, kernel.WorkflowTool, string, int, string, json.RawMessage, runs.WorkflowApproval) (json.RawMessage, error) {
	return nil, errors.New("the route tool needs no approval")
}

func TestGatewayToolLoopRunsTheRouteToolFeedsItsResultBackAndSettlesEveryModelTurn(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "tools", vendor.Model
	var pricingID string
	if err := pool.QueryRow(t.Context(), `SELECT pricing_revision_id::text FROM olp.prices WHERE provider_id=$1`,
		fixture.providerID).Scan(&pricingID); err != nil {
		t.Fatal(err)
	}
	type turn struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    any    `json:"content"`
		} `json:"messages"`
	}
	var mu sync.Mutex
	var turns []turn
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent turn
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		mu.Lock()
		turns = append(turns, sent)
		first := len(turns) == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"answer\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"It is forty-two."},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`)
	})
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price: rates.Price{ProviderKind: "openai_compatible", Model: model, Operation: "generation",
			InputPerMillion: new("1"), OutputPerMillion: new("2"), Currency: "USD"},
		RevisionID: pricingID, Revision: 1, EffectiveAt: fixture.observedAt.Add(-time.Hour),
	})
	tools := &routeTools{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "openai_compatible", operations: []string{"generation"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream, toolsets: []string{uuid.NewString()}, tools: tools,
	})
	g.key(t, "owner")
	headers := map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}

	_, refused := g.send(t, "POST", "/v1/chat/completions", headers,
		`{"model":"tools","olp_toolsets":["`+uuid.NewString()+`"],"messages":[{"role":"user","content":"what is the answer?"}]}`, 400)
	if !strings.Contains(string(refused), "toolset_not_permitted") || len(turns) != 0 {
		t.Fatalf("a toolset outside the route reached the model %d times: %s", len(turns), refused)
	}

	_, raw := g.send(t, "POST", "/v1/chat/completions", headers,
		`{"model":"tools","messages":[{"role":"user","content":"what is the answer?"}]}`, 200)
	var reply struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Model != slug || len(reply.Choices) != 1 || reply.Choices[0].Message.Content != "It is forty-two." ||
		len(reply.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("the caller did not get only the final answer: %s", raw)
	}
	if len(tools.calls) != 1 || tools.calls[0].arguments != `{"q":"answer"}` {
		t.Fatalf("tool calls = %+v, want the one call the model asked for", tools.calls)
	}
	if len(turns) != 2 || len(turns[0].Tools) != 1 || turns[0].Tools[0].Function.Name != "lookup" {
		t.Fatalf("model turns = %+v, want the route tool offered and one continuation", turns)
	}
	result := turns[1].Messages[len(turns[1].Messages)-1]
	if text, _ := result.Content.(string); result.Role != "tool" || result.ToolCallID != "call_1" || !strings.Contains(text, "forty-two") {
		t.Fatalf("the continuation ended with %+v, want the tool result for call_1", result)
	}

	const inputTokens, outputTokens = 5 + 9, 3 + 4
	var settled, runCount int
	var run string
	var priced bool
	if err := pool.QueryRow(t.Context(), `SELECT count(*), count(DISTINCT run_id), coalesce(min(run_id::text),''),
		coalesce(sum(observed_cost),0) = ($2::numeric * 1 + $3::numeric * 2) / 1000000 FROM olp.execution_debits
		WHERE api_key_id=$1 AND kind='model' AND state='settled'`, g.runtime.keys["owner"].ID, inputTokens, outputTokens).
		Scan(&settled, &runCount, &run, &priced); err != nil {
		t.Fatal(err)
	}
	if settled != 2 || runCount != 1 || run != tools.calls[0].run || !priced {
		t.Fatalf("settled model debits=%d runs=%d run=%s priced for both turns=%t, want 2 debits on run %s",
			settled, runCount, run, priced, tools.calls[0].run)
	}
}
