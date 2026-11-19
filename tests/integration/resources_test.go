//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
	"github.com/tyk-swe/olp/tests/harness"
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

func TestListedResourcesSkipStaleRowsAndRefillPages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	store := resources.New(pool)

	const (
		slug          = "stored-listed"
		forbiddenSlug = "stored-hidden"
		model         = "listed-model"
		tunedModel    = "discovered-tuning-model"
		upstreamJob   = "ftjob-observed"
		tuningPath    = "/v1/fine_tuning/jobs"
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tuningPath+"/"+upstreamJob {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"created_at":%d,"status":"succeeded","trained_tokens":12,"fine_tuned_model":%q,"training_file":"file-unbound"}`, upstreamJob, fixture.observedAt.Unix(), tunedModel)
	}))
	t.Cleanup(upstream.Close)
	providerConfig, err := json.Marshal(map[string]string{"kind": "openai", "endpoint": upstream.URL + "/v1", "auth_mode": "api_key"})
	if err != nil {
		t.Fatal(err)
	}
	credentialID := uuid.NewString()
	slotID := uuid.NewString()
	targetID := uuid.NewString()
	providerRevisionID := uuid.NewString()
	credentialVersion := 1

	slots, err := json.Marshal([]routing.RevisionSlot{{Slot: routing.Slot{
		ID: slotID, Name: "default", Enabled: true, Weight: 1,
		CredentialID: &credentialID, CredentialVersion: &credentialVersion,
	}, Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	targets, err := json.Marshal([]routing.PublishedTarget{{
		ID: targetID, ProviderModelID: "listed-model-row", ProviderID: fixture.providerID,
		ProviderModel: model, Weight: 1, TimeoutMS: 2000,
	}})
	if err != nil {
		t.Fatal(err)
	}
	operations, err := json.Marshal([]string{"batch", "fine_tuning"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.provider_credentials (id,provider_id,version) VALUES ($1,$2,1)`, credentialID, fixture.providerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.provider_revisions (id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by)
		VALUES ($1,$2,1,'listed',$3,'[]',$4,1,$1,$5)`, providerRevisionID, fixture.providerID, providerConfig, slots, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	seedRoute := func(routeSlug string) string {
		t.Helper()
		routeID, revisionID := uuid.NewString(), uuid.NewString()
		if _, err := pool.Exec(ctx, `INSERT INTO olp.routes (id,slug,created_by,latest_revision,latest_revision_id,etag,project_id)
			VALUES ($1,$2,$3,1,$4,$5,$6)`, routeID, routeSlug, fixture.ownerID, revisionID, uuid.NewString(), fixture.projectID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO olp.route_revisions (id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,routing_policy,content_policy,fidelity,behavior)
			VALUES ($1,$2,1,$3,$4,5000,1,$5,$6,$7,'{}',NULL,'{"mode":"transformed"}','{}')`,
			revisionID, routeID, routeSlug, operations, targets, uuid.NewString(), fixture.ownerID); err != nil {
			t.Fatal(err)
		}
		return revisionID
	}
	routeRevisionID := seedRoute(slug)
	forbiddenRevisionID := seedRoute(forbiddenSlug)

	snapshotRouteID := uuid.NewString()
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers: map[string]routing.Provider{fixture.providerID: {
			ID: fixture.providerID, Name: "listed", Kind: "openai_compatible", Enabled: true,
			ActiveCredential: &credentialID, RevisionID: providerRevisionID,
			Capabilities: []routing.Capability{{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"}},
			Endpoint:     "https://example.com/v1", AuthMode: "api_key",
			Slots: []routing.Slot{{ID: slotID, Name: "default", Enabled: true, Weight: 1, CredentialID: &credentialID, CredentialVersion: &credentialVersion}},
		}},
		Routes: map[string]routing.Route{slug: {
			ID: snapshotRouteID, Slug: slug, Operations: []string{"generation"}, OverallTimeout: 5000, MaxAttempts: 3,
			RoutingID: snapshotRouteID, RevisionID: routeRevisionID, Revision: 1, PublishedAt: time.Now(), ProjectID: &fixture.projectID,
			Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed},
			Targets:  []routing.Target{{ID: targetID, ProviderID: fixture.providerID, ProviderModel: model, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()}},
		}},
	}
	published, err := release.NewRelease(uuid.NewString(), 1, snapshot, map[string][]byte{credentialID: []byte("listed-secret")})
	if err != nil {
		t.Fatal(err)
	}

	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })

	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "openai", Model: model, Operation: "fine_tuning", PerRequest: new("0.60"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	runtime := &listedRuntime{
		release: published,
		keys:    map[string]keyauthority.Authority{},
		inputs:  &routinginputs.Inputs{Prices: []rates.RoutingPrice{*price}, RefreshedAt: time.Now()},
	}
	newKey := func(t *testing.T, secret string) string {
		t.Helper()
		keyID := uuid.NewString()
		fixture.addKey(t, keyID)
		runtime.keys[secret] = keyauthority.Authority{
			ID: keyID, LookupID: "listed" + secret, ProjectID: &fixture.projectID,
			Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{slug}, AllowProviderState: true},
		}
		return keyID
	}
	runner := newRunner(t)
	limiter := admission.NewDurable(runner, debits.NewSyncer(debits.NewStore(pool), runner))
	server := gateway.New(gateway.Dependencies{
		Runtime:   runtime,
		Admission: quota.NewAdmission(limiter, fleet.New(runner), nil, nil),
		Egress:    &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}},
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Getenv:    func(string) string { return "" },
		Digester:  secrets.NewAuthKey(make([]byte, 32), "listed-resources"),
		Resources: store,
		Resolver:  resources.NewResolver(pool),
		Staging:   spool,
		Uploads:   staging.NewAdmission(spool.CapacityBytes()),
	}, gateway.Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096})
	mux := http.NewServeMux()
	server.Register(mux)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

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

	t.Run("anthropic backward pages refill past stale rows", func(t *testing.T) {
		keyID := newKey(t, "backward")
		anchor := valid(t, keyID, resources.KindAnthropicFile, "backward-anchor")
		next := valid(t, keyID, resources.KindAnthropicFile, "backward-next")
		stale(t, keyID, resources.KindAnthropicFile, "backward-stale")
		newest := valid(t, keyID, resources.KindAnthropicFile, "backward-newest")
		listedIDs := func(body map[string]json.RawMessage) ([]string, bool) {
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
		ids, hasMore := listedIDs(get(t, "backward", "/anthropic/v1/files?limit=1&before_id="+anchor.ID))
		sameStrings(t, "first backward page", ids, next.ID)
		if !hasMore {
			t.Fatal("first backward page has_more = false, want true")
		}
		ids, hasMore = listedIDs(get(t, "backward", "/anthropic/v1/files?limit=1&before_id="+next.ID))
		sameStrings(t, "second backward page", ids, newest.ID)
		if hasMore {
			t.Fatal("second backward page has_more = true, want false")
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
