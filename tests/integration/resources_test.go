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
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/a2a"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/agentcontract"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/healthprobes"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestA2ACancellationAuditsProjectActorBeforePeerDispatch(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	actor, task := uuid.NewString(), uuid.NewString()
	const upstream = "peer-task"
	seedA2ACancellation(t, fixture, actor, task, upstream)

	checkAudit := func(ctx context.Context) {
		var rows int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.audit WHERE action='a2a_task.cancel_attempt'
			AND resource_type='a2a_task' AND resource_id=$1 AND project_id=$2 AND actor_user_id=$3`,
			task, fixture.projectID, actor).Scan(&rows)
		if err != nil || rows != 1 {
			t.Errorf("attributed cancellation attempts = %d, want 1 (%v)", rows, err)
		}
	}
	peer := cancellationPeer{cancel: func(ctx context.Context, request *protocol.CancelTaskRequest) (*protocol.Task, error) {
		checkAudit(ctx)
		if request.ID != upstream {
			return nil, fmt.Errorf("cancellation targeted %q, want %q", request.ID, upstream)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM olp.project_members WHERE project_id=$1 AND user_id=$2`, fixture.projectID, actor); err != nil {
			return nil, err
		}
		return &protocol.Task{ID: request.ID, ContextID: "peer-context", Status: protocol.TaskStatus{State: protocol.TaskStateCanceled}}, nil
	}}
	store := tasks.New(pool, tasks.Limits{}, nil, slog.New(slog.DiscardHandler))
	call := managementHTTPAs(t, pool, actor, func(mux *http.ServeMux, server *access.Server) {
		a2a.NewManagement(server, store, agents.NewTaskOperations(store, peer, nil)).Register(mux)
	})
	if result := call("POST", "/api/v1/a2a-tasks/"+task+"/cancel", "", ""); result.status != http.StatusNotFound {
		t.Fatalf("cancellation after project access was removed = %d %s, want 404", result.status, result.body)
	}
	var state tasks.State
	if err := pool.QueryRow(t.Context(), `SELECT state FROM olp.a2a_tasks WHERE id=$1`, task).Scan(&state); err != nil || state != tasks.Canceled {
		t.Fatalf("stored task state = %q, want %q (%v)", state, tasks.Canceled, err)
	}
	checkAudit(t.Context())
}

func seedA2ACancellation(t *testing.T, f usageFixture, actor, task, upstream string) {
	t.Helper()
	publication, revision, dispatch := uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, err := f.pool.Exec(t.Context(), `WITH actor AS (
		INSERT INTO olp.users(id,email,display_name,role,access_scope,etag)
		VALUES($1::uuid,$1::text||'@example.test','Task operator','operator','assigned',$1::uuid) RETURNING id
	), membership AS (
		INSERT INTO olp.project_members(project_id,user_id,role,added_by) SELECT $2,id,'manager',$3 FROM actor
	), publication AS (
		INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by)
		VALUES($4,$2,'agent','Task peer','{}',$4,$3) RETURNING id
	), revision AS (
		INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by)
		SELECT $5,id,$2,'agent',jsonb_build_object('project_id',$2::uuid,'agent','{}'::jsonb),repeat('0',64),$3 FROM publication
		RETURNING id, publication_id
	), debit AS (
		INSERT INTO olp.execution_debits(id,api_key_id,project_id,kind,affinity,intent_digest,claim_id,currency,cost_bound,charge_bound,prepared_until)
		VALUES($6,$7,$2,'agent','{}',decode(repeat('00',32),'hex'),$6,'USD',0,0,now()) RETURNING id
	)
	INSERT INTO olp.a2a_tasks(id,api_key_id,project_id,publication_id,revision_id,dispatch_id,upstream_id,state)
	SELECT $8,$7,$2,revision.publication_id,revision.id,debit.id,$9,'active' FROM revision CROSS JOIN debit`,
		actor, f.projectID, f.ownerID, publication, revision, dispatch, f.keyID, task, upstream)
	if err != nil {
		t.Fatal(err)
	}
}

type cancellationPeer struct {
	agents.Peer
	cancel func(context.Context, *protocol.CancelTaskRequest) (*protocol.Task, error)
}

func (p cancellationPeer) Open(context.Context, agentcontract.Definition, []byte, bool) (agents.Peer, error) {
	return p, nil
}

func (p cancellationPeer) Cancel(ctx context.Context, request *protocol.CancelTaskRequest) (*protocol.Task, error) {
	return p.cancel(ctx, request)
}

func (cancellationPeer) Close() {}

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
	sealed := contract("sealed")
	stored, err := store.PutDurableContract(ctx, sealed, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, got, err := store.ReadDurableContract(ctx, resources.KindStrictFile, fixture.keyID, stored.ID); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the owner reading its payload: %q, %v", got, err)
	}
	var conflict *pgconn.PgError
	if _, err := store.PutDurableContract(ctx, sealed, payload); !errors.As(err, &conflict) || conflict.Code != "23505" {
		t.Fatalf("storing a second live mapping for a native file: %v, want a unique violation", err)
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

	replacementPayload := []byte(`{"native":"a replacement file"}`)
	replacement, err := store.PutDurableContract(ctx, sealed, replacementPayload)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == stored.ID {
		t.Fatalf("replacement reused the deleted gateway ID %q", stored.ID)
	}
	if _, got, err := store.ReadDurableContract(ctx, resources.KindStrictFile, fixture.keyID, replacement.ID); err != nil || !bytes.Equal(got, replacementPayload) {
		t.Fatalf("reading the replacement payload: %q, %v", got, err)
	}
	if _, _, err := store.ReadDurableContract(ctx, resources.KindStrictFile, fixture.keyID, stored.ID); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("reading the deleted payload after replacement: %v, want %v", err, resources.ErrNotFound)
	}
	if mapped, err := store.GetByUpstreamIncludingDeleted(ctx, stored.NativeIdentity()); err != nil || mapped.ID != replacement.ID {
		t.Fatalf("projecting the reused native file identifier: %+v, %v, want %q", mapped, err, replacement.ID)
	}
	if _, err := pool.Exec(ctx, `UPDATE olp.api_keys SET revoked_at=now() WHERE id=$1`, fixture.keyID); err != nil {
		t.Fatal(err)
	}
	if _, got, err := store.ReadDurableContract(ctx, resources.KindStrictFile, fixture.keyID, replacement.ID); !errors.Is(err, resources.ErrNotFound) || got != nil {
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
	release atomic.Pointer[release.Release]
	inputs  *routinginputs.Inputs
	keys    map[string]keyauthority.Authority
}

func (r *listedRuntime) Release() *release.Release { return r.release.Load() }

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
	vendor      string
	surface     string
	profile     string
	version     string
	toolsets    []string
	tools       kernel.WorkflowToolRunner
	secure      bool
	streams     bool
	fidelity    string
	probe       *routing.HealthProbe
	limits      *routing.Limits
	defaults    map[string]connectors.DefaultSet
	sink        events.Sink
	operations  []string
	prices      []rates.RoutingPrice
	upstream    http.Handler
	valkey      scripts.Commander
	sessions    bool
	front       func(http.Handler) http.Handler
}

func (p storedPlan) served(mux http.Handler) http.Handler {
	if p.front == nil {
		return mux
	}
	return p.front(mux)
}

type storedGateway struct {
	t       *testing.T
	fixture usageFixture
	plan    storedPlan
	store   *resources.Store
	runtime *listedRuntime
	mux     *http.ServeMux
	server  *httptest.Server
	kernel  *kernel.Server
	fleet   *fleet.State
	limiter *admission.Limiter

	snapshot *routing.Snapshot
	secrets  map[string][]byte

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
	providerConfig := encoded(t, map[string]any{"kind": kind, "endpoint": endpoint, "auth_mode": "api_key", "deployment": plan.model, "profile_id": plan.profile, "profile_revision": profileRevision, "options": map[string]any{"network": network, "operation_defaults": plan.defaults}})
	modes := []string{"unary"}
	if plan.streams {
		modes = []string{"unary", "streaming"}
	}
	certified := make([]routing.RevisionCapability, 0, len(plan.operations)*len(modes))
	capabilities := make([]routing.Capability, 0, len(plan.operations)*len(modes))
	for _, operation := range plan.operations {
		for _, mode := range operationModes(operation, modes) {
			certified = append(certified, routing.RevisionCapability{Operation: operation, Surface: cmp.Or(plan.surface, "openai"), Mode: mode, Source: "certified"})
			capabilities = append(capabilities, routing.Capability{Model: plan.model, Operation: operation, Surface: cmp.Or(plan.surface, "openai"), Mode: mode, ToolCalls: plan.tools != nil})
		}
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
	if _, err := pool.Exec(ctx, `UPDATE olp.providers SET state='active',active_revision=1,active_revision_id=$2 WHERE id=$1`, fixture.providerID, g.providerRevisionID); err != nil {
		t.Fatal(err)
	}
	routeID, routeRevisionID := g.seedRoute(plan.slug)
	g.routeRevisionID = routeRevisionID

	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers: map[string]routing.Provider{fixture.providerID: {
			ID: fixture.providerID, Name: "listed", Kind: connectors.Kind(kind), VendorID: plan.vendor, Enabled: true, ProfileID: plan.profile, ProfileRevision: profileRevision, Network: network,
			ActiveCredential: &g.credentialID, RevisionID: g.providerRevisionID, Capabilities: capabilities,
			Endpoint: endpoint, AuthMode: "api_key", Slots: []routing.Slot{slot}, Deployment: plan.model, HealthProbe: plan.probe, Limits: plan.limits, OperationDefaults: plan.defaults,
		}},
		Routes: map[string]routing.Route{plan.slug: {
			ID: routeID, Slug: plan.slug, Operations: plan.operations, OverallTimeout: 5000, MaxAttempts: 3,
			RoutingID: routeID, RevisionID: g.routeRevisionID, Revision: 1, PublishedAt: time.Now(), ProjectID: &fixture.projectID,
			Fidelity: routing.RouteFidelity{Mode: cmp.Or(plan.fidelity, routing.FidelityTransformed)},
			Targets:  []routing.Target{{ID: g.targetID, ProviderID: fixture.providerID, ProviderModel: plan.model, Weight: 1, Timeout: 2000, RoutingID: g.modelID}},
		}},
	}
	if plan.tools != nil {
		route := snapshot.Routes[plan.slug]
		route.ToolExecution, route.Toolsets = "gateway", plan.toolsets
		snapshot.Routes[plan.slug] = route
	}
	g.snapshot, g.secrets = snapshot, map[string][]byte{g.credentialID: []byte("listed-secret")}
	g.runtime = &listedRuntime{
		keys:   map[string]keyauthority.Authority{},
		inputs: &routinginputs.Inputs{Prices: plan.prices, RefreshedAt: time.Now()},
	}
	g.republish(t, func(*routing.Route) {})
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	runner := runnerOver(t, cmp.Or(plan.valkey, scripts.Commander(harness.Valkey(t))))
	debitStore := debits.NewStore(pool)
	syncer := debits.NewSyncer(debitStore, runner)
	limiter := admission.NewDurable(runner, syncer)
	g.limiter = limiter
	workflow := kernel.Dependencies{}
	if plan.sessions {
		workflow = kernel.Dependencies{Debits: &kernel.SessionDebits{Session: debits.NewSession(debitStore, syncer)}}
	}
	if plan.tools != nil {
		workflow = kernel.Dependencies{
			WorkflowTools: plan.tools,
			WorkflowRuns:  runs.NewWorkflowStore(pool, auth, keys, installation),
			Debits: &kernel.SessionDebits{
				Session: debits.NewSession(debitStore, syncer),
				SettleWorkflow: func(ctx context.Context, event *usage.Event, attempt int) ([]limits.Snapshot, error) {
					return ingest.PersistWorkflowAttempt(ctx, pool, event, attempt)
				},
			},
		}
	}
	g.fleet = fleet.New(runner)
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	stored, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.runtime_releases (id,sequence,sha256,snapshot,created_by)
		SELECT $1,coalesce(max(sequence),0)+1,repeat('0',64),$2,$3 FROM olp.runtime_releases`, snapshot.Generation.ID, stored, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	mediaTransport := transport.New(transport.Deps{
		Client: policy.Client(kernel.UpstreamHeaderTimeout), Auth: connectors.NewAuth(policy, func(string) string { return "" }),
		Egress: policy, Spool: spool, MaxResponseBytes: 1 << 20, Log: quiet,
	})
	jobStore := jobs.NewStore(pool)
	server := kernel.New(kernel.Dependencies{
		Media: &kernel.MediaDeps{Transport: mediaTransport, Store: jobStore, Jobs: jobs.New(jobs.Deps{
			Store: jobStore, Transport: mediaTransport, Credentials: g.runtime, Gaps: &atomic.Uint64{}, Log: quiet, Now: time.Now,
		})},
		WorkflowTools: workflow.WorkflowTools, WorkflowRuns: workflow.WorkflowRuns, Debits: workflow.Debits,
		Runtime:   g.runtime,
		Sink:      plan.sink,
		Admission: quota.NewAdmission(limiter, g.fleet, nil, nil),
		Egress:    policy,
		Log:       quiet,
		Getenv:    func(string) string { return "" },
		Digester:  secrets.NewAuthKey(make([]byte, 32), "listed-resources"),
		Resources: g.store,
		Resolver:  resources.NewResolver(pool),
		Batches:   batches.NewStore(pool),
		Staging:   spool,
		Uploads:   staging.NewAdmission(spool.CapacityBytes()),
	}, kernel.Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxMediaBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096})
	g.kernel = server
	g.mux = http.NewServeMux()
	gateway.Register(server, g.mux)
	g.server = httptest.NewServer(plan.served(g.mux))
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
	if _, err := pool.Exec(ctx, `INSERT INTO olp.route_revisions (id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,routing_policy,fidelity,behavior)
		VALUES ($1,$2,1,$3,$4,5000,1,$5,$6,$7,'{}',$8,'{}')`,
		revisionID, routeID, slug, encoded(g.t, g.plan.operations), targets, uuid.NewString(), fixture.ownerID,
		encoded(g.t, routing.RouteFidelity{Mode: cmp.Or(g.plan.fidelity, routing.FidelityTransformed)})); err != nil {
		g.t.Fatal(err)
	}
	return routeID, revisionID
}

func operationModes(operation string, modes []string) []string {
	if operation == "video_create" {
		return []string{"async"}
	}
	if operation == "realtime" {
		return []string{"realtime"}
	}
	return modes
}

func (g *storedGateway) republish(t *testing.T, edit func(*routing.Route)) {
	t.Helper()
	route := g.snapshot.Routes[g.plan.slug]
	edit(&route)
	g.snapshot.Routes[g.plan.slug] = route
	published, err := release.NewRelease(uuid.NewString(), 1, g.snapshot, g.secrets)
	if err != nil {
		t.Fatal(err)
	}
	g.runtime.release.Store(published)
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
	request, query, key, authorization, body string
	beta, accept, upload                     string
}

type retainedUpstream struct {
	mu      sync.Mutex
	calls   []upstreamCall
	replies func(request string, nth int) map[string]string
	headers func(*http.Request) map[string]string
	status  func(request string) int
}

const providerOwned = "provider-owned"

var providerPrefix = regexp.MustCompile(`^/v1(beta)?(/openai(/deployments/[^/]+)?)?`)

func (u *retainedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	request := r.Method + " " + providerPrefix.ReplaceAllString(r.URL.Path, "")
	u.mu.Lock()
	u.calls = append(u.calls, upstreamCall{
		request: request, key: cmp.Or(r.Header.Get("api-key"), r.Header.Get("x-api-key"), r.Header.Get("x-goog-api-key")),
		authorization: r.Header.Get("Authorization"), body: string(body), query: r.URL.RawQuery,
		beta: r.Header.Get("Anthropic-Beta"), accept: r.Header.Get("Accept"),
		upload: r.Header.Get("X-Goog-Upload-Command") + "@" + r.Header.Get("X-Goog-Upload-Offset"),
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
	if u.status != nil {
		w.WriteHeader(cmp.Or(u.status(request), http.StatusOK))
	}
	_, _ = io.WriteString(w, reply)
}

func openAIReplies(_ string, nth int) map[string]string {
	const file = `{"id":"file-provider-owned","object":"file","bytes":7,"created_at":1700000000,"filename":"batch.jsonl","purpose":"batch"}`
	batch := func(status string) string {
		return `{"id":"batch_provider-owned","object":"batch","endpoint":"/v1/chat/completions","input_file_id":"file-provider-owned","completion_window":"24h","status":"` + status + `","created_at":1700000000}`
	}
	response := func(id, previous string) string {
		return `{"id":"` + id + `","object":"response","created_at":1700000000,"expires_at":4102444800,` + previous + `"status":"completed","model":"provider-model","output":[{"id":"msg_2","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	}
	created := response(fmt.Sprintf("resp_provider-owned-%d", nth), "")
	if nth == 2 {
		created = response("resp_provider-owned-2", `"previous_response_id":"resp_provider-owned-1",`)
	}
	replies := map[string]string{
		"POST /files":                                      file,
		"GET /files/file-provider-owned":                   file,
		"GET /files/file-provider-owned/content":           "CONTENT",
		"DELETE /files/file-provider-owned":                `{"id":"file-provider-owned","object":"file","deleted":true}`,
		"POST /batches":                                    batch("validating"),
		"GET /batches/batch_provider-owned":                batch("in_progress"),
		"POST /batches/batch_provider-owned/cancel":        batch("cancelling"),
		"POST /responses":                                  created,
		"POST /v1/responses":                               created,
		"GET /responses/resp_provider-owned-1":             response("resp_provider-owned-1", ""),
		"GET /responses/resp_provider-owned-1/input_items": `{"object":"list","data":[{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"first_id":"msg_1","last_id":"msg_1","has_more":false}`,
		"DELETE /responses/resp_provider-owned-1":          `{"id":"resp_provider-owned-1","object":"response.deleted","deleted":true}`,
	}
	for _, request := range []string{"GET /responses/resp_provider-owned-1", "GET /responses/resp_provider-owned-1/input_items", "DELETE /responses/resp_provider-owned-1"} {
		method, path, _ := strings.Cut(request, " ")
		replies[method+" /v1"+path] = replies[request]
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
	ID, Status, Model  string
	InputFileID        string `json:"input_file_id"`
	OutputFileID       string `json:"output_file_id"`
	ProcessingStatus   string `json:"processing_status"`
	VectorStoreID      string `json:"vector_store_id"`
	PreviousResponseID string `json:"previous_response_id"`
	Deleted            bool
	Data               []struct{ ID string }
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

func TestABatchUploadToAVendorThatRefusesBatchesAnswersWithTheVendorsOwnReason(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const form = "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n{}\n\r\n--b--\r\n"
	for _, vendor := range vendors.All() {
		if vendor.BatchRefusal == "" {
			continue
		}
		upstream := &retainedUpstream{replies: openAIReplies}
		g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
			slug: "refused-" + vendor.ID, model: "batch-model", kind: vendor.Connector, vendor: vendor.ID, operations: []string{"generation", "batch"}, upstream: upstream,
		})
		g.key(t, "owner")
		_, raw := g.send(t, "POST", "/v1/files", map[string]string{"Authorization": "Bearer owner", "Content-Type": "multipart/form-data; boundary=b"}, form, 422)
		var refusal struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Error.Code != "target_capability" || refusal.Error.Message != vendor.BatchRefusal {
			t.Fatalf("%s refused a batch upload with %s, want its contract's reason %q", vendor.ID, raw, vendor.BatchRefusal)
		}
		if sent := upstream.sent("POST /files"); len(sent) != 0 {
			t.Fatalf("%s received %d uploads, want none", vendor.ID, len(sent))
		}
	}
}

func TestFilesAndBatchesHideProviderIdentifiersIsolateKeysAndSpendTheProviderCredential(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "stored-batch", "batch-model"
	const output = "file-" + providerOwned + "-output"
	upstream := &retainedUpstream{replies: func(request string, nth int) map[string]string {
		replies := openAIReplies(request, nth)
		const read = "GET /batches/batch_provider-owned"
		if request == read && nth > 1 {
			batch := strings.Replace(replies[read], "in_progress", "completed", 1)
			replies[read] = strings.TrimSuffix(batch, "}") + `,"output_file_id":"` + output + `"}`
		}
		if request == "POST /files" && nth == 3 {
			replies[request] = strings.Replace(replies[request], "file-"+providerOwned, output, 1)
		}
		return replies
	}}
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
	replacement := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if replacement.ID == "" || replacement.ID == file.ID {
		t.Fatalf("upload with a reused provider ID returned %q, want a new gateway ID", replacement.ID)
	}
	g.call(t, "owner", "GET", "/v1/files/"+file.ID, "", "", 404)
	if got := g.call(t, "owner", "GET", "/v1/files", "", "", 200).identifiers(); !slices.Equal(got, []string{replacement.ID}) {
		t.Fatalf("owner lists files %v, want only %s", got, replacement.ID)
	}

	batchPath := "/v1/batches/" + batch.ID
	finished := g.call(t, "owner", "GET", batchPath, "", "", 200)
	if finished.OutputFileID == "" {
		t.Fatal("completed batch returned no output file identifier")
	}
	if err := g.store.Tombstone(t.Context(), finished.OutputFileID); err != nil {
		t.Fatal(err)
	}
	g.call(t, "owner", "GET", "/v1/batches", "", "", 200)
	if got := g.call(t, "owner", "GET", "/v1/files", "", "", 200).identifiers(); !slices.Equal(got, []string{replacement.ID}) {
		t.Fatalf("reading batch history recreated its deleted output, listed files %v", got)
	}
	newUpload := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if newUpload.ID == "" || newUpload.ID == finished.OutputFileID {
		t.Fatalf("upload with the batch output's old provider ID returned %q, want a new gateway ID", newUpload.ID)
	}
	if got := g.call(t, "owner", "GET", batchPath, "", "", 200); got.OutputFileID != finished.OutputFileID {
		t.Fatalf("batch output changed to %q, want the original gateway ID %q", got.OutputFileID, finished.OutputFileID)
	}
}

func TestAStoredResponseKeepsItsCitedFileWhenTheProviderReusesTheFileID(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug = "cited-files"
	upstream := &retainedUpstream{replies: func(request string, nth int) map[string]string {
		replies := openAIReplies(request, 1)
		first := strings.Replace(replies["POST /responses"], `"annotations":[]`,
			`"annotations":[{"type":"file_citation","index":0,"file_id":"file-`+providerOwned+`","filename":"batch.jsonl"}]`, 1)
		later := strings.ReplaceAll(first, "resp_provider-owned-1", "resp_provider-owned-2")
		replies["GET /responses/resp_provider-owned-1"], replies["GET /responses/resp_provider-owned-2"] = first, later
		replies["POST /responses"] = first
		if nth > 1 {
			replies["POST /responses"] = later
		}
		return replies
	}}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: "citing-model", kind: "azure_openai", operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	ask := func(method, path, body string) (string, string) {
		t.Helper()
		_, raw := g.send(t, method, path, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}, body, 200)
		var response struct {
			ID     string
			Output []struct {
				Content []struct {
					Annotations []struct {
						FileID string `json:"file_id"`
					}
				}
			}
		}
		if err := json.Unmarshal(raw, &response); err != nil || len(response.Output) != 1 || len(response.Output[0].Content) != 1 || len(response.Output[0].Content[0].Annotations) != 1 {
			t.Fatalf("%s %s returned %s, want one cited file: %v", method, path, raw, err)
		}
		return response.ID, response.Output[0].Content[0].Annotations[0].FileID
	}
	form := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n" +
		`{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + slug + `","messages":[{"role":"user","content":"hi"}]}}` + "\n\r\n--b--\r\n"
	create := `{"model":"` + slug + `","input":"hi","store":true}`

	file := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	first, cited := ask("POST", "/v1/responses", create)
	if file.ID == "" || cited != file.ID {
		t.Fatalf("the first response cites %q, want the uploaded file %q", cited, file.ID)
	}
	g.call(t, "owner", "DELETE", "/v1/files/"+file.ID, "", "", 200)
	replacement := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if replacement.ID == "" || replacement.ID == file.ID {
		t.Fatalf("upload with a reused provider ID returned %q, want a new gateway ID", replacement.ID)
	}

	if _, cited := ask("GET", "/v1/responses/"+first, ""); cited != file.ID {
		t.Fatalf("the first response now cites %q, want the file it was created with %q", cited, file.ID)
	}
	second, cited := ask("POST", "/v1/responses", create)
	if cited != replacement.ID {
		t.Fatalf("a response created after the reuse cites %q, want the replacement %q", cited, replacement.ID)
	}
	if _, cited := ask("GET", "/v1/responses/"+second, ""); cited != replacement.ID {
		t.Fatalf("the later response reads back citing %q, want the replacement %q", cited, replacement.ID)
	}
}

func TestAFineTuningJobKeepsItsFilesWhenTheProviderReusesTheFileID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		field      string
		uploadedBy bool
	}{
		"training file": {field: `"training_file":"file-` + providerOwned + `"`, uploadedBy: true},
		"result file":   {field: `"result_files":["file-` + providerOwned + `"]`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool, _ := harness.Database(t)
			const (
				slug  = "tuned-files"
				model = "tuning-model"
			)
			upstream := &retainedUpstream{replies: func(request string, _ int) map[string]string {
				replies := openAIReplies(request, 1)
				replies["GET /fine_tuning/jobs/ftjob-reused"] = `{"id":"ftjob-reused","object":"fine_tuning.job","created_at":1,"status":"succeeded","model":"` + model + `",` + tc.field + `}`
				return replies
			}}
			g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
				slug: slug, model: model, kind: "azure_openai", operations: []string{"batch", "fine_tuning"}, upstream: upstream,
			})
			keyID := g.key(t, "owner")
			form := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n" +
				`{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + slug + `","messages":[{"role":"user","content":"hi"}]}}` + "\n\r\n--b--\r\n"

			uploaded := func() string {
				if !tc.uploadedBy {
					return ""
				}
				return g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200).ID
			}()
			seeded := g.fixture.resource("fine_tuning_job", keyID, "ftjob-reused")
			seeded.RouteSlug, seeded.ProviderRevisionID, seeded.RouteRevisionID = slug, g.providerRevisionID, g.routeRevisionID
			seeded.SlotID, seeded.TargetID, seeded.CredentialID = g.slotID, g.targetID, &g.credentialID
			seeded.State, seeded.ContractVersion = "running", new(retainedapi.Revision)
			seeded.Metadata = json.RawMessage(`{"upstream_model":"` + model + `"}`)
			job, err := g.store.Put(t.Context(), seeded)
			if err != nil {
				t.Fatal(err)
			}
			read := func() string {
				t.Helper()
				_, raw := g.send(t, "GET", "/v1/fine_tuning/jobs/"+job.ID, map[string]string{"Authorization": "Bearer owner"}, "", 200)
				var decoded struct {
					TrainingFile string   `json:"training_file"`
					ResultFiles  []string `json:"result_files"`
				}
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatalf("job read returned %s: %v", raw, err)
				}
				return decoded.TrainingFile + strings.Join(decoded.ResultFiles, ",")
			}
			original := read()
			if original == "" || uploaded != "" && original != uploaded {
				t.Fatalf("the job names file %q, want the uploaded file %q", original, uploaded)
			}
			g.call(t, "owner", "DELETE", "/v1/files/"+original, "", "", 200)
			replacement := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
			if replacement.ID == "" || replacement.ID == original {
				t.Fatalf("upload with a reused provider ID returned %q, want a new gateway ID", replacement.ID)
			}
			if got := read(); got != original {
				t.Fatalf("the job now names file %q, want the file it named before the reuse %q", got, original)
			}
		})
	}
}

func TestStrictBatchKeepsItsOriginalInputWhenTheProviderReusesAFileID(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "strict-batch", "batch-model"
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", profile: "azure-v1-chat", version: "/openai/v1", fidelity: routing.FidelityStrict,
		operations: []string{"generation", "batch"}, upstream: http.StripPrefix("/openai", &retainedUpstream{replies: openAIReplies}),
	})
	g.key(t, "owner")
	line := `{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}}`
	form := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n" + line + "\n\r\n--b--\r\n"
	file := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	submission := `{"input_file_id":"` + file.ID + `","endpoint":"/v1/chat/completions","completion_window":"24h"}`
	batch := g.call(t, "owner", "POST", "/v1/batches", "application/json", submission, 200)
	g.call(t, "owner", "DELETE", "/v1/files/"+file.ID, "", "", 200)
	replacement := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if replacement.ID == "" || replacement.ID == file.ID {
		t.Fatalf("replacement ID = %q, want a new gateway ID", replacement.ID)
	}
	if got := g.call(t, "owner", "GET", "/v1/batches/"+batch.ID, "", "", 200); got.InputFileID != file.ID {
		t.Fatalf("batch input changed to %q, want the original gateway ID %q", got.InputFileID, file.ID)
	}
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
	if first.Model != slug {
		t.Fatalf("response creation model = %q, want route %q", first.Model, slug)
	}
	providerExpiry := time.Unix(4102444800, 0)
	if kept := storedResponses(t, pool, g, "owner"); len(kept) != 1 || kept[0] != (storedResponse{"response", "completed", model, providerExpiry}) {
		t.Fatalf("the gateway kept %+v, want the response with the provider's state, model and expiry", kept)
	}
	created := upstream.sent("POST /responses")
	if len(created) != 1 || created[0].key != "listed-secret" || created[0].authorization != "" || !strings.Contains(created[0].body, `"model":"`+model+`"`) {
		t.Fatalf("response reached the provider as %+v, want the provider model and credential only", created)
	}
	if got := g.call(t, "owner", "GET", "/v1/responses/"+first.ID, "", "", 200); got.ID != first.ID || got.Model != first.Model {
		t.Fatalf("response read returned %q of model %q, want %q of model %q", got.ID, got.Model, first.ID, first.Model)
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
	if second.ID == "" || second.ID == first.ID || second.PreviousResponseID != first.ID {
		t.Fatalf("continuation returned %q after %q naming %q, want a new identifier that names the first by its gateway identifier", second.ID, first.ID, second.PreviousResponseID)
	}
	if sent := upstream.sent("POST /responses"); len(sent) != 2 || !strings.Contains(sent[1].body, `"previous_response_id":"resp_provider-owned-1"`) {
		t.Fatalf("continuation reached the provider as %+v, want the provider's identifier of the first response", sent)
	}
	unkept, err := http.NewRequestWithContext(t.Context(), "POST", g.server.URL+"/v1/responses", strings.NewReader(`{"model":"`+slug+`","input":"once more","previous_response_id":"`+first.ID+`","store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	unkept.Header.Set("Authorization", "Bearer owner")
	unkept.Header.Set("Content-Type", json)
	answered, err := g.server.Client().Do(unkept)
	if err != nil {
		t.Fatal(err)
	}
	_ = answered.Body.Close()
	if kept := storedResponses(t, pool, g, "owner"); answered.StatusCode != 200 || len(kept) != 2 {
		t.Fatalf("a response the caller asked not to store answered %d and left %d stored, want 200 and the two stored before", answered.StatusCode, len(kept))
	}

	provider := g.snapshot.Providers[g.fixture.providerID]
	provider.ProjectID = &g.fixture.projectID
	provider.SharedTokenQuotas = []routing.SharedTokenQuota{{Model: model, Group: "responses", TokensPerMinute: 100000}}
	g.snapshot.Providers[g.fixture.providerID] = provider
	g.republish(t, func(*routing.Route) {})
	_, refused := g.send(t, "POST", "/v1/responses", map[string]string{"Authorization": "Bearer owner", "Content-Type": json}, continuation, 400)
	if !strings.Contains(string(refused), `"unbounded_work_budget"`) || len(upstream.sent("POST /responses")) != 3 {
		t.Fatalf("continuation with a shared quota answered %s, want a budget refusal before the provider", refused)
	}
	t.Logf("POST /v1/responses with previous_response_id and a shared token quota: HTTP 400 %s", refused)

	if got := g.call(t, "owner", "DELETE", "/v1/responses/"+first.ID, "", "", 200); !got.Deleted || got.ID != first.ID {
		t.Fatalf("response delete = %+v", got)
	}
	if sent := upstream.sent("DELETE /responses/resp_provider-owned-1"); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want only the owner's", len(sent))
	}
	g.call(t, "owner", "GET", "/v1/responses/"+first.ID, "", "", 404)

	native := &retainedUpstream{replies: openAIReplies}
	strict := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "strict-responses", model: model, kind: "azure_openai", profile: "azure-v1-responses", fidelity: routing.FidelityStrict,
		operations: []string{"generation"}, upstream: native,
	})
	strict.key(t, "strict-owner")
	strict.key(t, "strict-stranger")
	asked := time.Now()
	bound := strict.call(t, "strict-owner", "POST", "/v1/responses", json, `{"model":"strict-responses","input":"hi","store":true}`, 200)
	kept := storedResponses(t, pool, strict, "strict-owner")
	if !strings.HasPrefix(bound.ID, "strict_response_") || len(kept) != 1 || kept[0].kind != "strict_response" || kept[0].state != "completed" || kept[0].model != model {
		t.Fatalf("a strict route answered %q and kept %+v, want a strict response with the provider's state and model", bound.ID, kept)
	}
	if expiry := kept[0].expires; !expiry.After(asked) || expiry.After(asked.Add(resources.ContinuationLifetime)) {
		t.Fatalf("a strict response expires at %s, want within the gateway's continuation lifetime rather than the provider's %s", expiry, providerExpiry)
	}
	if got := strict.call(t, "strict-owner", "GET", "/v1/responses/"+bound.ID, "", "", 200); got.ID != bound.ID || got.Model != "strict-responses" {
		t.Fatalf("a strict response read returned %q of model %q, want %q of the route", got.ID, got.Model, bound.ID)
	}
	_, unstreamed := strict.send(t, "GET", "/v1/responses/"+bound.ID+"?stream=true", map[string]string{"Authorization": "Bearer strict-owner"}, "", 400)
	if !strings.Contains(string(unstreamed), `"target_capability"`) || len(native.sent("GET /v1/responses/resp_provider-owned-1")) != 1 {
		t.Fatalf("a strict response whose provider has no stream contract answered a streamed read with %s, want a capability refusal before the provider", unstreamed)
	}
	if got := strict.call(t, "strict-owner", "GET", "/v1/responses/"+bound.ID+"/input_items", "", "", 200); len(got.Data) != 1 {
		t.Fatalf("a strict response's input items = %+v, want the provider's one item", got)
	}
	following := `{"model":"strict-responses","input":"again","previous_response_id":"` + bound.ID + `","store":true}`
	strict.call(t, "strict-stranger", "GET", "/v1/responses/"+bound.ID, "", "", 404)
	strict.call(t, "strict-stranger", "POST", "/v1/responses", json, following, 400)
	strict.call(t, "strict-stranger", "DELETE", "/v1/responses/"+bound.ID, "", "", 404)
	next := strict.call(t, "strict-owner", "POST", "/v1/responses", json, following, 200)
	sent := native.sent("POST /v1/responses")
	if !strings.HasPrefix(next.ID, "strict_response_") || next.ID == bound.ID || next.PreviousResponseID != bound.ID || len(sent) != 2 || !strings.Contains(sent[1].body, `"previous_response_id":"resp_provider-owned-1"`) {
		t.Fatalf("a strict continuation returned %q after %q and reached the provider as %+v, want a new strict response that names the first by its gateway identifier and one provider request naming the provider's", next.ID, next.PreviousResponseID, sent)
	}
	if got := strict.call(t, "strict-owner", "DELETE", "/v1/responses/"+bound.ID, "", "", 200); !got.Deleted || got.ID != bound.ID {
		t.Fatalf("a strict response delete = %+v", got)
	}
	if sent := native.sent("DELETE /v1/responses/resp_provider-owned-1"); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes of the strict response, want only the owner's", len(sent))
	}
	strict.call(t, "strict-owner", "GET", "/v1/responses/"+bound.ID, "", "", 404)
}

func TestMissingStoredResponseReconcilesBackgroundUsage(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug = "missing-responses"
	var providerStatus atomic.Int32
	upstream := &retainedUpstream{
		status: func(request string) int {
			if request == "POST /responses" {
				return http.StatusOK
			}
			return int(providerStatus.Load())
		},
		replies: func(request string, nth int) map[string]string {
			if request == "POST /responses" {
				return map[string]string{request: fmt.Sprintf(`{"id":"resp_%s-%d","object":"response","background":true,"status":"queued","model":"provider-model","output":[]}`, providerOwned, nth)}
			}
			return map[string]string{request: `{"error":{"type":"server_error","message":"provider error"}}`}
		},
	}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: "responses-model", kind: "azure_openai", operations: []string{"generation"}, upstream: upstream,
	})
	keyID := g.key(t, "owner")
	headers := map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}
	for i, op := range []struct{ name, method, suffix string }{
		{"read", http.MethodGet, ""},
		{"cancel", http.MethodPost, "/cancel"},
		{"input items", http.MethodGet, "/input_items"},
	} {
		t.Run(op.name, func(t *testing.T) {
			header, raw := g.send(t, http.MethodPost, "/v1/responses", headers, `{"model":"`+slug+`","input":"hi","background":true,"store":true}`, http.StatusOK)
			var created retainedObject
			if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" || created.Status != "queued" {
				t.Fatalf("background response creation: %s, %v", raw, err)
			}
			assertUsage := func(want int) {
				t.Helper()
				var count, uncertain int
				err := pool.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE charge_status = 'billing_uncertain' AND NOT usage_observed AND NOT usage_complete)
					FROM olp.attempt_usage_facts WHERE request_id=$1 AND api_key_id=$2`, header.Get("X-Request-Id"), keyID).Scan(&count, &uncertain)
				if err != nil || count != want || uncertain != want {
					t.Fatalf("creation usage: %d facts, %d unobserved and uncertain, want %d of each (error: %v)", count, uncertain, want, err)
				}
			}

			path := "/v1/responses/" + created.ID + op.suffix
			native := fmt.Sprintf("%s /responses/resp_%s-%d%s", op.method, providerOwned, i+1, op.suffix)
			providerStatus.Store(http.StatusInternalServerError)
			g.send(t, op.method, path, headers, "", http.StatusInternalServerError)
			assertUsage(0)

			providerStatus.Store(http.StatusNotFound)
			for range 2 {
				_, raw := g.send(t, op.method, path, headers, "", http.StatusNotFound)
				var missing struct{ Error struct{ Code string } }
				if err := json.Unmarshal(raw, &missing); err != nil || missing.Error.Code != "not_found" {
					t.Fatalf("missing response: %s, %v", raw, err)
				}
				if got := len(upstream.sent(native)); got != 2 {
					t.Errorf("provider calls after missing response: got %d, want 2", got)
				}
				assertUsage(1)
			}
		})
	}
}

type storedResponse struct {
	kind, state, model string
	expires            time.Time
}

func storedResponses(t *testing.T, pool *pgxpool.Pool, g *storedGateway, key string) []storedResponse {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT kind, state, metadata->>'upstream_model', expires_at FROM olp.provider_resources WHERE api_key_id=$1 ORDER BY created_at`, g.runtime.keys[key].ID)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedResponse, error) {
		var r storedResponse
		err := row.Scan(&r.kind, &r.state, &r.model, &r.expires)
		r.expires = r.expires.Local()
		return r, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return kept
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

func TestGeminiUploadsResumeInChunksFromTheStoredOffsetAndFailTheSessionOnAProviderMismatch(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	upstream := &retainedUpstream{replies: geminiReplies, headers: func(r *http.Request) map[string]string {
		if r.Header.Get("X-Goog-Upload-Command") == "upload" && r.ContentLength == 3 {
			return map[string]string{"X-Goog-Upload-Size-Received": "9"}
		}
		return geminiUploadHeaders(r)
	}}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "gemini-chunks", model: "gemini-model", kind: "gemini", surface: "gemini", version: "/v1beta", secure: true,
		operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	start := func() string {
		t.Helper()
		started, _ := g.send(t, "POST", "/gemini/upload/v1beta/files", geminiKey("owner", map[string]string{
			"Content-Type": "application/json", "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start",
			"X-Goog-Upload-Header-Content-Length": "10", "X-Goog-Upload-Header-Content-Type": "text/plain",
		}), `{"file":{"display_name":"notes"}}`, 200)
		session, _ := strings.CutPrefix(started.Get("X-Goog-Upload-URL"), g.server.URL)
		return session
	}
	command := func(session, command, offset, body string, want int) http.Header {
		t.Helper()
		reply, _ := g.send(t, "POST", session, geminiKey("owner", map[string]string{"X-Goog-Upload-Command": command, "X-Goog-Upload-Offset": offset}), body, want)
		return reply
	}
	received := func(session, want, status string) {
		t.Helper()
		reply := command(session, "query", "", "", 200)
		if got := reply.Get("X-Goog-Upload-Size-Received"); got != want || reply.Get("X-Goog-Upload-Status") != status {
			t.Fatalf("session reports %q bytes with status %q, want %q and %q", got, reply.Get("X-Goog-Upload-Status"), want, status)
		}
	}

	session := start()
	received(session, "0", "active")
	command(session, "append", "0", "hello", 400)
	command(session, "query", "", "hello", 400)
	if reply := command(session, "upload", "0", "hello", 200); reply.Get("X-Goog-Upload-Size-Received") != "5" || reply.Get("X-Goog-Upload-Status") != "active" {
		t.Fatalf("first chunk acknowledged as %v, want five bytes on an active session", reply)
	}
	received(session, "5", "active")
	command(session, "upload", "0", "hello", 400)
	command(session, "upload", "5", "far too long", 400)
	command(session, "upload, finalize", "5", "wor", 400)
	received(session, "5", "active")
	if reply := command(session, "upload, finalize", "5", "world", 200); reply.Get("X-Goog-Upload-Size-Received") != "10" || reply.Get("X-Goog-Upload-Status") != "final" {
		t.Fatalf("last chunk acknowledged as %v, want ten bytes on a final session", reply)
	}
	sent := upstream.sent("POST /upload/session-" + providerOwned)
	if len(sent) != 2 || sent[0].body != "hello" || sent[0].upload != "upload@0" || sent[1].body != "world" || sent[1].upload != "upload, finalize@5" {
		t.Fatalf("the provider session received %+v, want the two accepted chunks at offsets 0 and 5", sent)
	}

	broken := start()
	command(broken, "upload", "0", "abc", 502)
	command(broken, "query", "", "", 409)
	command(broken, "upload", "0", "hello", 409)
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
	if sent := upstream.sent("DELETE /vector_stores/vs_" + providerOwned); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want 1", len(sent))
	}
	replacement := submit("owner", stores, "application/json", `{"name":"notes"}`, uuid.NewString(), 200)
	if replacement.ID == "" || replacement.ID == store.ID {
		t.Fatalf("creation with a reused provider ID returned %q, want a new gateway ID", replacement.ID)
	}
	g.call(t, "owner", "GET", own, "", "", 404)
	g.call(t, "owner", "GET", own+"/files/"+attached.ID, "", "", 404)
	if got := g.call(t, "owner", "GET", stores, "", "", 200).identifiers(); !slices.Equal(got, []string{replacement.ID}) {
		t.Fatalf("owner lists stores %v, want only %s", got, replacement.ID)
	}
	replayedAfterDelete, _ := g.send(t, "POST", stores, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json", "Idempotency-Key": creation}, `{"name":"notes"}`, 409)
	if got := replayedAfterDelete.Get("X-OLP-Resource-ID"); got != store.ID {
		t.Fatalf("replaying the first creation returned %q, want the original %q", got, store.ID)
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
	if strings.Contains(string(arguments), "fail") {
		return nil, &mcp.WorkflowError{Status: http.StatusBadGateway, Code: "tool_failed", Message: "The tool failed."}
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"forty-two"}]}`), nil
}

func (*routeTools) CallApproved(context.Context, keyauthority.Authority, *release.Release, kernel.WorkflowTool, string, int, string, json.RawMessage, runs.WorkflowApproval) (json.RawMessage, error) {
	return nil, errors.New("the route tool needs no approval")
}

type chatWorkflowUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CompletionDetail struct {
		ReasoningTokens          int64 `json:"reasoning_tokens"`
		AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens"`
	} `json:"completion_tokens_details"`
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
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"answer\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8,"completion_tokens_details":{"reasoning_tokens":1}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"It is forty-two."},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13,"completion_tokens_details":{"reasoning_tokens":2,"accepted_prediction_tokens":1}}}`)
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
		Model   string            `json:"model"`
		Usage   chatWorkflowUsage `json:"usage"`
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
	want := chatWorkflowUsage{PromptTokens: inputTokens, CompletionTokens: outputTokens, TotalTokens: inputTokens + outputTokens}
	want.CompletionDetail.ReasoningTokens = 1 + 2
	want.CompletionDetail.AcceptedPredictionTokens = 1
	if reply.Usage != want {
		t.Errorf("response usage = %+v, want %+v for both model turns", reply.Usage, want)
	}
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

func interactionEvent(id int, body string) string {
	return fmt.Sprintf("id: %d\ndata: %s\n\n", id, body)
}

func interactionStreamReplies(request string, nth int) map[string]string {
	native := "interaction-" + providerOwned + "-1"
	lifecycle := func(event, id, status string) string {
		return `{"event_type":"` + event + `","interaction":{"id":"` + id + `","status":"` + status + `","model":"gemini-model","usage":{"total_input_tokens":3,"total_output_tokens":2,"total_tokens":5}}}`
	}
	start := interactionEvent(2, `{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`)
	rest := interactionEvent(3, `{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"hello"}}`) +
		interactionEvent(4, `{"event_type":"step.stop","index":0}`)
	created := map[int]string{
		1: interactionEvent(1, lifecycle("interaction.created", native, "in_progress")) + start + rest + interactionEvent(5, lifecycle("interaction.completed", native, "completed")),
		2: start + rest,
	}
	resumed := map[int]string{
		1: rest + interactionEvent(5, lifecycle("interaction.completed", native, "completed")),
		2: rest + interactionEvent(5, lifecycle("interaction.completed", "interaction-"+providerOwned+"-2", "completed")),
	}
	return map[string]string{
		"POST /interactions":                       created[nth],
		"GET /interactions/" + native:              resumed[nth],
		"POST /interactions/" + native + "/cancel": `{"id":"` + native + `","status":"cancelled","model":"gemini-model","steps":[]}`,
	}
}

func TestNewGeminiInteractionsHoldTheirReplyBoundAgainstOutputTokenLimits(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "gemini-interactions", "gemini-model"
	upstream := &retainedUpstream{replies: geminiReplies}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "gemini", surface: "gemini", profile: "gemini-interactions", version: "/v1beta", secure: true,
		operations: []string{"generation"}, upstream: upstream, limits: &routing.Limits{OutputTokensPerMinute: new(int64(100))},
	})
	g.key(t, "owner")
	const interactions = "/gemini/v1beta/interactions"
	headers := geminiKey("owner", map[string]string{"Content-Type": "application/json"})
	create := func(replyBound string) string {
		return `{"model":"` + slug + `","input":"hi","generation_config":{"max_output_tokens":` + replyBound + `}}`
	}

	g.send(t, "POST", interactions, headers, create("500"), 429)
	if sent := upstream.sent("POST /interactions"); len(sent) != 0 {
		t.Fatalf("an Interaction allowed 500 output tokens reached the provider under a 100 token output limit: %+v", sent)
	}
	g.send(t, "POST", interactions, headers, create("50"), 200)
}

func TestFreeFileControlsPassAnExhaustedSharedTokenQuota(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "shared-quota-files", "files-model"
	fixture := newUsageFixture(t, pool)
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "vector_store"},
		upstream: &retainedUpstream{replies: openAIReplies},
	})
	g.key(t, "owner")
	upload := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nassistants\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	_, raw := g.send(t, "POST", "/v1/files", map[string]string{"Authorization": "Bearer owner", "Content-Type": "multipart/form-data; boundary=b", "Idempotency-Key": uuid.NewString()}, upload, 200)
	var file retainedObject
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}

	shared := []routing.SharedTokenQuota{{Model: model, Group: "files", TokensPerMinute: 1}}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.provider_revisions SET configuration=jsonb_set(configuration::jsonb,'{options,shared_token_quotas}',$2::jsonb)::json WHERE id=$1`, g.providerRevisionID, encoded(t, shared)); err != nil {
		t.Fatal(err)
	}
	before := g.runtime.release.Load().Snapshot()
	provider := g.snapshot.Providers[fixture.providerID]
	provider.ProjectID, provider.SharedTokenQuotas = &fixture.projectID, shared
	g.snapshot.Providers[fixture.providerID] = provider
	g.republish(t, func(*routing.Route) {})
	if err := quota.SyncSharedQuotas(t.Context(), g.limiter, before, g.runtime.release.Load().Snapshot()); err != nil {
		t.Fatal(err)
	}
	g.send(t, "POST", "/v1/chat/completions", map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"},
		`{"model":"`+slug+`","messages":[{"role":"user","content":"hi"}]}`, 429)
	g.call(t, "owner", "GET", "/v1/files/"+file.ID, "", "", 200)
	g.call(t, "owner", "DELETE", "/v1/files/"+file.ID, "", "", 200)
}

type streamedInteraction struct {
	ids    []string
	events []struct {
		Type        string `json:"event_type"`
		Interaction struct{ ID, Model, Status string }
	}
}

func readInteractionStream(t *testing.T, header http.Header, raw []byte) streamedInteraction {
	t.Helper()
	if kind := header.Get("Content-Type"); kind != "text/event-stream" {
		t.Fatalf("content type = %q, want an event stream: %s", kind, raw)
	}
	var stream streamedInteraction
	for line := range strings.SplitSeq(string(raw), "\n") {
		if id, found := strings.CutPrefix(line, "id: "); found {
			stream.ids = append(stream.ids, id)
		}
		data, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		stream.events = append(stream.events, struct {
			Type        string `json:"event_type"`
			Interaction struct{ ID, Model, Status string }
		}{})
		if err := json.Unmarshal([]byte(data), &stream.events[len(stream.events)-1]); err != nil {
			t.Fatalf("event %q: %v", data, err)
		}
	}
	return stream
}

func TestGeminiInteractionStreamsBindTheirResourceResumeFromACursorCancelAndRefuseBrokenStreams(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, native = "gemini-streams", "interaction-" + providerOwned + "-1"
	upstream := &retainedUpstream{replies: interactionStreamReplies, headers: func(r *http.Request) map[string]string {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/interactions") || r.URL.Query().Get("stream") == "true" {
			return map[string]string{"Content-Type": "text/event-stream"}
		}
		return nil
	}}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: "gemini-model", kind: "gemini", surface: "gemini", profile: "gemini-interactions", version: "/v1beta", secure: true, streams: true,
		operations: []string{"generation"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const interactions = "/gemini/v1beta/interactions"
	body := `{"model":"` + slug + `","input":"hi","stream":true,"store":true}`
	json := map[string]string{"Content-Type": "application/json"}

	header, raw := g.send(t, "POST", interactions, geminiKey("owner", json), body, 200)
	created := readInteractionStream(t, header, raw)
	if len(created.events) != 5 || !slices.Equal(created.ids, []string{"1", "2", "3", "4", "5"}) {
		t.Fatalf("stream = %s, want the provider's five events under their event identifiers", raw)
	}
	first, last := created.events[0], created.events[4]
	if !strings.HasPrefix(first.Interaction.ID, "interaction_") || last.Interaction.ID != first.Interaction.ID ||
		first.Interaction.Model != slug || last.Type != "interaction.completed" || last.Interaction.Status != "completed" {
		t.Fatalf("stream = %s, want one gateway identifier under the route name through to completion", raw)
	}
	local := interactions + "/" + first.Interaction.ID

	for _, refused := range []struct{ method, path string }{
		{"GET", local + "?last_event_id=3"},
		{"GET", local + "?stream=maybe"},
		{"GET", local + "?stream=true&last_event_id=" + strings.Repeat("9", 513)},
		{"POST", local + "/cancel?stream=true"},
		{"DELETE", local + "?include_input=true"},
	} {
		g.send(t, refused.method, refused.path, geminiKey("owner", nil), "", 400)
	}
	g.send(t, "GET", local+"?stream=true&last_event_id=3", geminiKey("stranger", nil), "", 404)
	g.send(t, "POST", local+"/cancel", geminiKey("stranger", nil), "", 404)
	if sent := upstream.sent("GET /interactions/" + native); len(sent) != 0 {
		t.Fatalf("a refused retrieval reached the provider: %+v", sent)
	}

	header, raw = g.send(t, "GET", local+"?stream=true&last_event_id=3&include_input=false", geminiKey("owner", nil), "", 200)
	resumed := readInteractionStream(t, header, raw)
	if len(resumed.events) != 3 || !slices.Equal(resumed.ids, []string{"3", "4", "5"}) || resumed.events[2].Interaction.ID != first.Interaction.ID || resumed.events[2].Interaction.Model != slug {
		t.Fatalf("resumed stream = %s, want the remaining events for %s", raw, first.Interaction.ID)
	}
	if sent := upstream.sent("GET /interactions/" + native); len(sent) != 1 || sent[0].query != "include_input=false&last_event_id=3&stream=true" || sent[0].key != "listed-secret" {
		t.Fatalf("resume reached the provider as %+v, want the cursor on the provider's interaction", sent)
	}
	header, raw = g.send(t, "GET", local+"?stream=true&last_event_id=3", geminiKey("owner", nil), "", 200)
	if cut := readInteractionStream(t, header, raw); !slices.Equal(cut.ids, []string{"3", "4"}) {
		t.Fatalf("stream = %s, want it cut before the event that names another interaction", raw)
	}

	_, raw = g.send(t, "POST", local+"/cancel", geminiKey("owner", nil), "", 200)
	if !strings.Contains(string(raw), `"id":"`+first.Interaction.ID+`"`) || !strings.Contains(string(raw), `"status":"cancelled"`) || !strings.Contains(string(raw), `"model":"`+slug+`"`) {
		t.Fatalf("cancel = %s, want the cancelled interaction under its gateway identifier", raw)
	}
	if sent := upstream.sent("POST /interactions/" + native + "/cancel"); len(sent) != 1 || sent[0].body != "" {
		t.Fatalf("cancel reached the provider as %+v, want one bodiless call", sent)
	}

	g.send(t, "POST", interactions, geminiKey("owner", json), body, 502)
	var stored int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources`).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored interactions = %d, %v, want only the one whose stream opened correctly", stored, err)
	}
}

func TestFailedGeminiInteractionsReachTheCallerWithoutTheProviderCredentialOrARetryHint(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, native = "gemini-failures", "interaction-" + providerOwned + "-1"
	failure := `"status":"failed","model":"gemini-model","error":{"code":"internal","message":"credential listed-secret was rejected"}`
	upstream := &retainedUpstream{
		replies: func(string, int) map[string]string {
			return map[string]string{"POST /interactions": `{"id":"` + native + `",` + failure + `,"steps":[]}`}
		},
		headers: func(*http.Request) map[string]string { return nil },
	}
	streamed := &retainedUpstream{
		replies: func(string, int) map[string]string {
			return map[string]string{"POST /interactions": interactionEvent(1, `{"event_type":"interaction.created","interaction":{"id":"`+native+`","status":"in_progress","model":"gemini-model"}}`) +
				interactionEvent(2, `{"event_type":"interaction.completed","interaction":{"id":"`+native+`",`+failure+`}}`)}
		},
		headers: func(*http.Request) map[string]string { return map[string]string{"Content-Type": "text/event-stream"} },
	}
	plan := storedPlan{
		model: "gemini-model", kind: "gemini", surface: "gemini", profile: "gemini-interactions", version: "/v1beta", secure: true, streams: true,
		operations: []string{"generation"},
	}
	headers := geminiKey("owner", map[string]string{"Content-Type": "application/json"})

	plan.slug, plan.upstream = slug, upstream
	whole := newStoredGateway(t, newUsageFixture(t, pool), plan)
	whole.key(t, "owner")
	header, raw := whole.send(t, "POST", "/gemini/v1beta/interactions", headers, `{"model":"`+slug+`","input":"hi"}`, 200)
	var failed struct{ ID, Model, Status string }
	if err := json.Unmarshal(raw, &failed); err != nil || failed.Status != "failed" || failed.Model != slug || failed.ID == "" {
		t.Fatalf("failed interaction = %s, want the provider's failure under the route name", raw)
	}
	if strings.Contains(string(raw), "listed-secret") || header.Get("X-Should-Retry") != "false" {
		t.Fatalf("failed interaction = %s with retry hint %q, want no credential and no retry", raw, header.Get("X-Should-Retry"))
	}

	plan.slug, plan.upstream = slug+"-stream", streamed
	live := newStoredGateway(t, newUsageFixture(t, pool), plan)
	live.key(t, "owner")
	header, raw = live.send(t, "POST", "/gemini/v1beta/interactions", headers, `{"model":"`+slug+`-stream","input":"hi","stream":true}`, 200)
	stream := readInteractionStream(t, header, raw)
	if len(stream.events) != 2 || stream.events[1].Interaction.Status != "failed" || stream.events[1].Interaction.Model != slug+"-stream" {
		t.Fatalf("failed stream = %s, want the provider's terminal failure under the route name", raw)
	}
	if strings.Contains(string(raw), "listed-secret") || header.Get("X-Should-Retry") != "false" {
		t.Fatalf("failed stream = %s with retry hint %q, want no credential and no retry", raw, header.Get("X-Should-Retry"))
	}
}

func geminiBatchReplies(request string, nth int) map[string]string {
	const native = "batches/batch-" + providerOwned
	batch := func(state, extra string) string {
		return `{"name":"` + native + `","metadata":{"state":"` + state + `","model":"models/gemini-model","displayName":"nightly","createTime":"2026-01-01T00:00:00Z","batchStats":{"requestCount":"2"},"inputConfig":{"fileName":"files/` + providerOwned + `-2"}` + extra + `}}`
	}
	replies := geminiReplies(request, nth)
	const finalize = "POST /upload/session-" + providerOwned
	replies[finalize] = strings.Replace(replies[finalize], "files/"+providerOwned, fmt.Sprintf("files/%s-%d", providerOwned, nth), 1)
	replies["POST /models/gemini-model:batchGenerateContent"] = batch("BATCH_STATE_PENDING", "")
	replies["GET /"+native] = batch("BATCH_STATE_SUCCEEDED", `,"output":{"responsesFile":"files/output-`+providerOwned+`"}`)
	replies["POST /"+native+":cancel"] = `{}`
	replies["DELETE /"+native] = `{}`
	return replies
}

type geminiBatch struct {
	Name     string
	Metadata struct {
		Model, State string
		InputConfig  struct{ FileName string }
		Output       struct{ ResponsesFile string }
	}
}

func geminiBatchLine(key string) string {
	return `{"key":"` + key + `","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`
}

func (g *storedGateway) uploadGeminiFile(t *testing.T, key, contentType, content string) geminiFile {
	t.Helper()
	started, _ := g.send(t, "POST", "/gemini/upload/v1beta/files", geminiKey(key, map[string]string{
		"Content-Type": "application/json", "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start",
		"X-Goog-Upload-Header-Content-Length": strconv.Itoa(len(content)), "X-Goog-Upload-Header-Content-Type": contentType,
	}), `{"file":{"display_name":"notes"}}`, 200)
	session, found := strings.CutPrefix(started.Get("X-Goog-Upload-URL"), g.server.URL)
	if !found {
		t.Fatalf("upload session is %q, want a session on the gateway", started.Get("X-Goog-Upload-URL"))
	}
	_, raw := g.send(t, "POST", session, geminiKey(key, map[string]string{"X-Goog-Upload-Command": "upload, finalize", "X-Goog-Upload-Offset": "0"}), content, 200)
	var uploaded struct{ File geminiFile }
	if err := json.Unmarshal(raw, &uploaded); err != nil {
		t.Fatal(err)
	}
	return uploaded.File
}

func TestGeminiBatchesBindTheirInputAndOutputFilesHideProviderIdentifiersAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug = "gemini-batches"
	const create = "/gemini/v1beta/models/" + slug + ":batchGenerateContent"
	const native = "batches/batch-" + providerOwned
	upstream := &retainedUpstream{replies: geminiBatchReplies, headers: geminiUploadHeaders}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: "gemini-model", kind: "gemini", surface: "gemini", version: "/v1beta", secure: true,
		operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	owner := geminiKey("owner", map[string]string{"Content-Type": "application/json"})
	stranger := geminiKey("stranger", map[string]string{"Content-Type": "application/json"})
	fromFile := func(name string) string {
		return `{"batch":{"display_name":"nightly","input_config":{"file_name":"` + name + `"}}}`
	}
	inline := func(first, second string) string {
		item := func(key string) string {
			return `{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]},"metadata":{"key":"` + key + `"}}`
		}
		return `{"batch":{"display_name":"nightly","input_config":{"requests":{"requests":[` + item(first) + `,` + item(second) + `]}}}}`
	}

	g.send(t, "POST", create, owner, inline("a", "a"), 400)
	g.send(t, "POST", create, geminiKey("owner", map[string]string{"X-OLP-Route": "another-route"}), inline("a", "b"), 400)
	prose := g.uploadGeminiFile(t, "owner", "text/plain", "hello")
	g.send(t, "POST", create, owner, fromFile(prose.Name), 422)
	input := g.uploadGeminiFile(t, "owner", "application/jsonl", geminiBatchLine("a")+"\n"+geminiBatchLine("b")+"\n")
	g.send(t, "POST", create, stranger, fromFile(input.Name), 404)
	repeated := g.uploadGeminiFile(t, "owner", "application/jsonl", geminiBatchLine("a")+"\n"+geminiBatchLine("a")+"\n")
	g.send(t, "POST", create, owner, fromFile(repeated.Name), 422)
	if sent := upstream.sent("POST /models/gemini-model:batchGenerateContent"); len(sent) != 0 {
		t.Fatalf("refused batches reached the provider: %+v", sent)
	}

	var created geminiBatch
	_, raw := g.send(t, "POST", create, owner, fromFile(input.Name), 200)
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Name, "batches/gemini_batch_") || created.Metadata.Model != "models/"+slug || created.Metadata.InputConfig.FileName != input.Name {
		t.Fatalf("created batch is %s, want a gateway batch on the route naming the caller's input file", raw)
	}
	sent := upstream.sent("POST /models/gemini-model:batchGenerateContent")
	if len(sent) != 1 || sent[0].key != "listed-secret" || !strings.Contains(sent[0].body, `"fileName":"files/`+providerOwned+`-2"`) || strings.Contains(sent[0].body, input.Name) {
		t.Fatalf("provider received %+v, want one batch under the provider credential naming the provider's file", sent)
	}

	batch := "/gemini/v1beta/" + created.Name
	g.send(t, "GET", batch, stranger, "", 404)
	g.send(t, "POST", batch+":cancel", stranger, "", 404)
	g.send(t, "DELETE", batch, stranger, "", 404)
	if _, listed := g.send(t, "GET", "/gemini/v1beta/batches", stranger, "", 200); strings.Contains(string(listed), created.Name) {
		t.Fatalf("another key listed the batch: %s", listed)
	}

	var finished geminiBatch
	_, raw = g.send(t, "GET", batch, owner, "", 200)
	if err := json.Unmarshal(raw, &finished); err != nil {
		t.Fatal(err)
	}
	output := finished.Metadata.Output.ResponsesFile
	if finished.Name != created.Name || finished.Metadata.State != "BATCH_STATE_SUCCEEDED" || !strings.HasPrefix(output, "files/gemini_file_") || output == input.Name {
		t.Fatalf("finished batch is %s, want the same batch with a gateway output file of its own", raw)
	}
	var again geminiBatch
	_, raw = g.send(t, "GET", batch, owner, "", 200)
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	if again.Metadata.Output.ResponsesFile != output {
		t.Fatalf("output file changed from %q to %q between reads", output, again.Metadata.Output.ResponsesFile)
	}
	g.send(t, "GET", "/gemini/v1beta/"+output, stranger, "", 404)
	if _, listed := g.send(t, "GET", "/gemini/v1beta/batches", owner, "", 200); !strings.Contains(string(listed), created.Name) || !strings.Contains(string(listed), output) {
		t.Fatalf("owner listing is %s, want the batch and its output file", listed)
	}

	g.send(t, "POST", batch+":cancel", owner, "", 200)
	g.send(t, "DELETE", batch, owner, "", 409)
	if cancelled, deleted := upstream.sent("POST /"+native+":cancel"), upstream.sent("DELETE /"+native); len(cancelled) != 1 || len(deleted) != 0 {
		t.Fatalf("provider saw %d cancels and %d deletes, want one cancel and no delete while the batch is unsettled", len(cancelled), len(deleted))
	}
}

type terminals struct {
	mu   sync.Mutex
	seen []events.Envelope
}

func (s *terminals) Terminal(e events.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, e)
}

func (s *terminals) all() []events.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

func probePass(t *testing.T, g *storedGateway) int {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	total := -1
	healthprobes.Run(ctx, g.kernel, func(_ context.Context, probed int, err error) {
		if err != nil {
			t.Error(err)
		}
		total = probed
		cancel()
	})
	return total
}

func TestHealthProbesAskEachOptedInProviderOncePerIntervalAndOpenItsCircuitOnlyWhenTheProviderFails(t *testing.T) {
	t.Parallel()
	const slug, model = "probed", "probe-model"
	interval := &routing.HealthProbe{IntervalSeconds: 60}
	for _, test := range []struct {
		name           string
		probe          *routing.HealthProbe
		upstream       int
		probed         int
		status         fleet.ProbeStatus
		class          string
		circuit        bool
		terminalStatus int
	}{
		{name: "healthy provider", probe: interval, upstream: http.StatusOK, probed: 1, status: fleet.ProbeHealthy, terminalStatus: http.StatusOK},
		{name: "failing provider", probe: interval, upstream: http.StatusInternalServerError, probed: 1, status: fleet.ProbeUnhealthy, class: "upstream_server", circuit: true, terminalStatus: http.StatusBadGateway},
		{name: "refused credential", probe: interval, upstream: http.StatusUnauthorized, probed: 1, status: fleet.ProbeHealthy, class: "credential", terminalStatus: http.StatusBadGateway},
		{name: "provider that did not opt in", upstream: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pool, _ := harness.Database(t)
			fixture := newUsageFixture(t, pool)
			var mu sync.Mutex
			var asked []string
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				asked = append(asked, string(raw))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if test.upstream != http.StatusOK {
					w.WriteHeader(test.upstream)
					_, _ = io.WriteString(w, `{"error":{"message":"refused","type":"error"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
			})
			sink := &terminals{}
			g := newStoredGateway(t, fixture, storedPlan{slug: slug, model: model, kind: "openai_compatible", operations: []string{"generation"},
				probe: test.probe, sink: sink, upstream: upstream})
			started := time.Now()
			if probed := probePass(t, g); probed != test.probed {
				t.Fatalf("first pass probed %d targets, want %d", probed, test.probed)
			}
			if probed := probePass(t, g); probed != 0 {
				t.Fatalf("a pass inside the interval probed %d targets", probed)
			}
			mu.Lock()
			requests := slices.Clone(asked)
			mu.Unlock()
			if len(requests) != test.probed {
				t.Fatalf("provider received %d requests, want %d: %v", len(requests), test.probed, requests)
			}
			results, err := g.fleet.Probes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			circuits, err := g.fleet.Circuits(t.Context(), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			seen := sink.all()
			if test.probed == 0 {
				if len(results) != 0 || len(circuits) != 0 || len(seen) != 0 {
					t.Fatalf("an unprobed provider left results %v, circuits %v, records %d", results, circuits, len(seen))
				}
				return
			}
			var sent struct {
				Model     string `json:"model"`
				MaxTokens int64  `json:"max_tokens"`
				Messages  []any  `json:"messages"`
			}
			if err := json.Unmarshal([]byte(requests[0]), &sent); err != nil {
				t.Fatal(err)
			}
			if sent.Model != model || sent.MaxTokens <= 0 || len(sent.Messages) != 1 {
				t.Fatalf("probe request = %s", requests[0])
			}
			result, ok := results[fixture.providerID]
			if !ok || len(results) != 1 || result.Status != test.status || result.Class != test.class || result.Model != model || result.ObservedAt.Before(started) {
				t.Fatalf("shared probe results = %+v", results)
			}
			until, open := circuits[fixture.providerID]
			if open != test.circuit || (open && until.Before(started.Truncate(time.Millisecond).Add(test.probe.Interval()))) {
				t.Fatalf("fleet circuits = %v, want open %t for at least the probe interval", circuits, test.circuit)
			}
			if len(seen) != 1 {
				t.Fatalf("probe wrote %d usage records", len(seen))
			}
			record := seen[0]
			if record.Actor != "system" || record.Origin != usage.OriginProbe || record.KeyID != "" || record.Route != slug ||
				record.Status != test.terminalStatus || len(record.Attempts) != 1 || record.Attempts[0].ProviderID != fixture.providerID {
				t.Fatalf("probe usage record = %+v", record)
			}
		})
	}
}

func TestAProviderOverItsOwnLimitIsRefusedByTheGatewayWithARetryHintAndIsNotCalled(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "limited", "limited-model"
	var calls atomic.Int64
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"`+model+`","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	})
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "openai_compatible", operations: []string{"generation"},
		upstream: upstream, limits: &routing.Limits{RequestsPerMinute: new(int64(1))}, sink: sink,
	})
	g.key(t, "owner")
	headers := map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}
	const body = `{"model":"` + slug + `","messages":[{"role":"user","content":"hi"}]}`

	g.send(t, "POST", "/v1/chat/completions", headers, body, 200)
	refused, raw := g.send(t, "POST", "/v1/chat/completions", headers, body, 429)

	wait, err := strconv.Atoi(refused.Get("Retry-After"))
	if err != nil || wait < 1 || wait > 60 || !strings.Contains(string(raw), `"code":"rate_limit_exceeded"`) {
		t.Fatalf("the refusal carried Retry-After %q and %s, want a wait inside the minute and rate_limit_exceeded", refused.Get("Retry-After"), raw)
	}
	if calls.Load() != 1 {
		t.Fatalf("the provider was called %d times with one request per minute allowed", calls.Load())
	}
	seen := sink.all()
	last := seen[len(seen)-1]
	if len(seen) != 2 || len(last.Attempts) != 1 || last.Attempts[0].Class != "rate_limit" || last.Attempts[0].Status != 0 {
		t.Fatalf("recorded %d requests, the refused one with attempts %+v; want one rate limited attempt that got no provider status", len(seen), last.Attempts)
	}
}

type toolModel struct {
	mu    sync.Mutex
	turns map[string]int
}

func (m *toolModel) asked(script string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turns[script]
}

func (m *toolModel) wants(script string, answered bool) string {
	m.mu.Lock()
	m.turns[script]++
	turn := m.turns[script]
	m.mu.Unlock()
	call := func(id, name, arguments string) string {
		return `{"index":0,"id":"` + id + `","type":"function","function":{"name":"` + name + `","arguments":` + strconv.Quote(arguments) + `}}`
	}
	switch {
	case strings.HasPrefix(script, "keep asking"):
		return call(fmt.Sprintf("call_%d", turn), "lookup", `{"q":"again"}`)
	case answered:
		return ""
	case script == "unknown tool":
		return call("call_1", "missing", `{}`)
	case script == "broken arguments":
		return call("call_1", "lookup", `{`)
	case strings.HasPrefix(script, "two at once"):
		return call("call_1", "lookup", `{"q":"a"}`) + "," + strings.Replace(call("call_2", "lookup", `{"q":"b"}`), `"index":0`, `"index":1`, 1)
	case script == "failing tool":
		return call("call_1", "lookup", `{"q":"fail"}`)
	}
	return call("call_1", "lookup", `{"q":"answer"}`)
}

func (m *toolModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var sent struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&sent); err != nil || len(sent.Messages) == 0 {
		http.Error(w, "unreadable request", http.StatusBadRequest)
		return
	}
	asked := map[string]bool{}
	for _, message := range sent.Messages {
		for _, call := range message.ToolCalls {
			asked[call.ID] = true
		}
		if message.Role == "tool" && !asked[message.ToolCallID] {
			http.Error(w, "a tool result answers no earlier tool call", http.StatusBadRequest)
			return
		}
	}
	script, _ := sent.Messages[0].Content.(string)
	calls := m.wants(script, sent.Messages[len(sent.Messages)-1].Role == "tool")
	const usage = `"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}`
	head := `{"id":"turn","created":1,"model":"` + vendor.Model + `",`
	if !sent.Stream {
		message, finish := `{"role":"assistant","content":"It is forty-two."}`, "stop"
		if calls != "" {
			message, finish = `{"role":"assistant","content":null,"tool_calls":[`+calls+`]}`, "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, head+`"object":"chat.completion","choices":[{"index":0,"message":`+message+`,"finish_reason":"`+finish+`"}],`+usage+`}`)
		return
	}
	chunk := func(delta, finish string) string {
		return "data: " + head + `"object":"chat.completion.chunk","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + finish + "}]}\n\n"
	}
	frames := chunk(`{"role":"assistant","content":"It is "}`, "null") + chunk(`{"content":"forty-two."}`, "null") + chunk(`{}`, `"stop"`)
	if calls != "" {
		frames = chunk(`{"role":"assistant","tool_calls":[`+calls+`]}`, "null") + chunk(`{}`, `"tool_calls"`)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, frames+"data: "+head+`"object":"chat.completion.chunk","choices":[],`+usage+"}\n\ndata: [DONE]\n\n")
}

func TestGatewayToolLoopStreamsOnlyTheFinalAnswerStopsAtItsRunLimitsAndRefusesCallsItCannotRun(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	var pricingID string
	if err := pool.QueryRow(t.Context(), `SELECT pricing_revision_id::text FROM olp.prices WHERE provider_id=$1`,
		fixture.providerID).Scan(&pricingID); err != nil {
		t.Fatal(err)
	}
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price: rates.Price{ProviderKind: "openai_compatible", Model: vendor.Model, Operation: "generation",
			InputPerMillion: new("1"), OutputPerMillion: new("2"), Currency: "USD"},
		RevisionID: pricingID, Revision: 1, EffectiveAt: fixture.observedAt.Add(-time.Hour),
	})
	model, tools := &toolModel{turns: map[string]int{}}, &routeTools{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: "tools", model: vendor.Model, kind: "openai_compatible", operations: []string{"generation"}, streams: true,
		prices: []rates.RoutingPrice{*price}, upstream: model, toolsets: []string{uuid.NewString()}, tools: tools,
	})
	g.key(t, "owner")
	headers := map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}
	turns := 0
	for _, tc := range []struct {
		script, settings string
		status           int
		says             string
		turns, tools     int
		usage            *chatWorkflowUsage
	}{
		{script: "streamed", settings: `,"stream":true`, status: 200, says: `"content":"forty-two."`, turns: 2, tools: 1},
		{script: "streamed with usage", settings: `,"stream":true,"stream_options":{"include_usage":true}`, status: 200,
			says: `"content":"forty-two."`, turns: 2, tools: 1,
			usage: &chatWorkflowUsage{PromptTokens: 5 * 2, CompletionTokens: 3 * 2, TotalTokens: 8 * 2}},
		{script: "two at once", status: 200, says: "It is forty-two.", turns: 2, tools: 2},
		{script: "keep asking until the iteration limit", settings: `,"olp_run_limits":{"max_iterations":3}`,
			status: 400, says: "tool_iteration_limit", turns: 3, tools: 2},
		{script: "keep asking until the call limit", settings: `,"olp_run_limits":{"max_tool_calls":2}`,
			status: 400, says: "tool_call_limit", turns: 3, tools: 2},
		{script: "two at once over the call limit", settings: `,"olp_run_limits":{"max_tool_calls":1}`,
			status: 400, says: "tool_call_limit", turns: 1},
		{script: "unknown tool", status: 400, says: "invalid_tool_call", turns: 1},
		{script: "broken arguments", status: 400, says: "invalid_tool_call", turns: 1},
		{script: "failing tool", status: 502, says: "tool_failed", turns: 1, tools: 1},
		{script: "unreadable limits", settings: `,"olp_run_limits":{"max_iterations":"many"}`, status: 400, says: "invalid_run_limits"},
	} {
		called := len(tools.calls)
		_, raw := g.send(t, "POST", "/v1/chat/completions", headers,
			`{"model":"tools","messages":[{"role":"user","content":"`+tc.script+`"}]`+tc.settings+`}`, tc.status)
		if !strings.Contains(string(raw), tc.says) || strings.Contains(string(raw), "tool_calls") && tc.status == 200 {
			t.Fatalf("%s: the caller got %s, want %s and no tool call", tc.script, raw, tc.says)
		}
		if strings.Contains(tc.settings, `"stream":true`) {
			assertChatWorkflowStreamUsage(t, raw, tc.usage)
		}
		if asked, ran := model.asked(tc.script), len(tools.calls)-called; asked != tc.turns || ran != tc.tools {
			t.Fatalf("%s: %d model turns and %d tool calls, want %d and %d", tc.script, asked, ran, tc.turns, tc.tools)
		}
		turns += tc.turns
	}
	var settled int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.execution_debits WHERE api_key_id=$1 AND kind='model' AND state='settled'`,
		g.runtime.keys["owner"].ID).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if settled != turns {
		t.Fatalf("%d model debits settled for %d model turns, including the runs that were refused", settled, turns)
	}
}

func assertChatWorkflowStreamUsage(t *testing.T, raw []byte, want *chatWorkflowUsage) {
	t.Helper()
	var usage []chatWorkflowUsage
	for line := range strings.SplitSeq(string(raw), "\n") {
		data, found := strings.CutPrefix(line, "data:")
		if !found || strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *chatWorkflowUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk.Usage != nil {
			usage = append(usage, *chunk.Usage)
		}
	}
	if want == nil && len(usage) != 0 || want != nil && (len(usage) != 1 || usage[0] != *want) {
		t.Errorf("stream usage chunks = %+v, want %v", usage, want)
	}
}

func twoStoreReplies(request string, nth int) map[string]string {
	replies := map[string]string{
		"POST /files":         `{"id":"file-` + providerOwned + `","object":"file","bytes":5,"created_at":1,"filename":"a.txt","purpose":"assistants","status":"processed"}`,
		"POST /vector_stores": `{"id":"vs_` + providerOwned + strconv.Itoa(nth) + `","object":"vector_store","name":"notes","status":"completed","created_at":1,"file_counts":{"total":0}}`,
	}
	for _, n := range []string{"1", "2"} {
		store, path := `{"id":"vs_`+providerOwned+n+`","object":"vector_store","name":"notes","status":"completed","created_at":1,"file_counts":{"total":0}}`, "/vector_stores/vs_"+providerOwned+n
		file := `{"id":"file-` + providerOwned + `","object":"vector_store.file","vector_store_id":"vs_` + providerOwned + n + `","status":"completed","created_at":1}`
		replies["GET "+path], replies["POST "+path] = store, store
		replies["DELETE "+path] = `{"id":"vs_` + providerOwned + n + `","object":"vector_store.deleted","deleted":true}`
		replies["POST "+path+"/files"], replies["GET "+path+"/files/file-"+providerOwned] = file, file
	}
	return replies
}

func TestVectorStoreRequestsNeedASubmissionKeyStayInsideTheirStoreAndLoseOnlyNewWorkWhenTheRouteDropsTheOperation(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	upstream := &retainedUpstream{replies: twoStoreReplies}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stateless")
	g.key(t, "elsewhere")
	stateless, elsewhere := g.runtime.keys["stateless"], g.runtime.keys["elsewhere"]
	stateless.Policy.AllowProviderState, elsewhere.Policy.AllowedRoutes = false, []string{"another-route"}
	g.runtime.keys["stateless"], g.runtime.keys["elsewhere"] = stateless, elsewhere

	const stores, named, paid = "/v1/vector_stores", `{"name":"notes"}`, `{"expires_after":{"anchor":"last_active_at","days":1}}`
	ask := func(key, method, path, body string, headers map[string]string, want int) (http.Header, retainedObject, string) {
		t.Helper()
		sent := map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}
		maps.Copy(sent, headers)
		header, raw := g.send(t, method, path, sent, body, want)
		var decoded retainedObject
		var refusal struct{ Error struct{ Code string } }
		_ = json.Unmarshal(raw, &decoded)
		_ = json.Unmarshal(raw, &refusal)
		return header, decoded, refusal.Error.Code
	}
	submitted := func() map[string]string { return map[string]string{"Idempotency-Key": uuid.NewString()} }
	refused := func(situation, key, method, path, body string, headers map[string]string, status int, code string) http.Header {
		t.Helper()
		header, _, got := ask(key, method, path, body, headers, status)
		if got != code {
			t.Fatalf("%s is refused with %q, want %q", situation, got, code)
		}
		return header
	}

	for situation, attempt := range map[string]struct {
		key     string
		headers map[string]string
		status  int
		code    string
	}{
		"a creation without a submission key":    {"owner", nil, 400, "missing_required_parameter"},
		"a key that may not keep provider state": {"stateless", submitted(), 403, "provider_state_forbidden"},
		"a key that may not use the route":       {"elsewhere", submitted(), 403, "route_forbidden"},
	} {
		if header := refused(situation, attempt.key, "POST", stores, named, attempt.headers, attempt.status, attempt.code); header.Get("X-Should-Retry") != "false" {
			t.Fatalf("%s carries retry hint %q, want a creation refusal that says not to retry", situation, header.Get("X-Should-Retry"))
		}
	}
	if created := upstream.sent("POST /vector_stores"); len(created) != 0 {
		t.Fatalf("refused creations reached the provider %d times", len(created))
	}

	upload := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nassistants\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	_, file, _ := ask("owner", "POST", "/v1/files", upload, map[string]string{"Content-Type": "multipart/form-data; boundary=b", "Idempotency-Key": uuid.NewString()}, 200)
	_, first, _ := ask("owner", "POST", stores, named, submitted(), 200)
	_, second, _ := ask("owner", "POST", stores, named, submitted(), 200)
	if created := upstream.sent("POST /vector_stores"); len(created) != 2 || !strings.Contains(created[0].body, `"olp_submission_id":"`) {
		t.Fatalf("the provider saw creations %+v, want two that carry the gateway's submission identifier", created)
	}

	inFirst, inSecond := stores+"/"+first.ID, stores+"/"+second.ID
	attachment := submitted()
	_, attached, _ := ask("owner", "POST", inFirst+"/files", `{"file_id":"`+file.ID+`"}`, attachment, 200)
	refused("the same submission key in another store", "owner", "POST", inSecond+"/files", `{"file_id":"`+file.ID+`"}`, attachment, 409, "idempotency_conflict")
	refused("an attachment read through another store", "owner", "GET", inSecond+"/files/"+attached.ID, "", nil, 404, "not_found")
	if _, other, _ := ask("owner", "POST", inSecond+"/files", `{"file_id":"`+file.ID+`"}`,
		map[string]string{"X-OLP-Route": "another-route", "Idempotency-Key": uuid.NewString()}, 200); other.VectorStoreID != second.ID {
		t.Fatalf("an attachment under a foreign route header landed in %q, want its store %q whatever the header says", other.VectorStoreID, second.ID)
	}

	g.republish(t, func(route *routing.Route) { route.Operations = []string{"generation", "batch"} })
	if header := refused("a creation on a route without the operation", "owner", "POST", stores, named, submitted(), 422, "operation_unavailable"); header.Get("X-Should-Retry") != "false" {
		t.Fatalf("a refused creation carries retry hint %q", header.Get("X-Should-Retry"))
	}
	refused("paid work on a store whose route dropped the operation", "owner", "POST", inFirst, paid, submitted(), 409, "provider_resource_credential_unavailable")
	if _, read, _ := ask("owner", "GET", inFirst, "", nil, 200); read.ID != first.ID {
		t.Fatalf("a store whose route dropped the operation reads as %q, want %q", read.ID, first.ID)
	}
	if _, renamed, _ := ask("owner", "POST", inFirst, `{"name":"renamed"}`, nil, 200); renamed.ID != first.ID {
		t.Fatalf("renaming a store whose route dropped the operation returned %q, want %q", renamed.ID, first.ID)
	}
	g.republish(t, func(route *routing.Route) { route.Operations = []string{"generation", "batch", "vector_store"} })
	refused("paid work without a submission key", "owner", "POST", inFirst, paid, nil, 400, "invalid_idempotency_key")
	if _, worked, _ := ask("owner", "POST", inFirst, paid, submitted(), 200); worked.ID != first.ID {
		t.Fatalf("paid work on a store returned %q, want %q", worked.ID, first.ID)
	}

	before := len(upstream.sent("GET /vector_stores/vs_" + providerOwned + "2"))
	if _, err := pool.Exec(t.Context(), `UPDATE olp.provider_resources SET state='submission_unknown' WHERE replace(id::text,'-','')=$1`,
		strings.TrimPrefix(second.ID, "vector_store_")); err != nil {
		t.Fatal(err)
	}
	refused("a store whose creation was never reconciled", "owner", "GET", inSecond, "", nil, 409, "submission_unknown")
	if after := len(upstream.sent("GET /vector_stores/vs_" + providerOwned + "2")); after != before {
		t.Fatalf("a refused read reached the provider %d times", after-before)
	}
}

type storeProvider struct {
	mu       sync.Mutex
	issues   string
	objects  func(issues string) map[string]string
	scripted map[string]string
	statuses map[string]int
}

func providerStore(n string) string {
	return `{"id":"vs_` + providerOwned + n + `","object":"vector_store","name":"notes","status":"completed","created_at":1,"file_counts":{"total":0}}`
}

func providerStorePath(n string) string { return "/vector_stores/vs_" + providerOwned + n }

func (p *storeProvider) answers(request string, status int, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.scripted[request], p.statuses[request] = body, status
}

func (p *storeProvider) recovers(request string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.scripted, request)
	delete(p.statuses, request)
}

func (p *storeProvider) creates(n string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.issues = n
}

func (p *storeProvider) replies(string, int) map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	replies := p.objects(p.issues)
	maps.Copy(replies, p.scripted)
	return replies
}

func storeObjects(issues string) map[string]string {
	replies := map[string]string{"POST /vector_stores": providerStore(issues)}
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		replies["GET "+providerStorePath(n)], replies["POST "+providerStorePath(n)] = providerStore(n), providerStore(n)
		replies["DELETE "+providerStorePath(n)] = `{"id":"vs_` + providerOwned + n + `","object":"vector_store.deleted","deleted":true}`
	}
	return replies
}

func (p *storeProvider) status(request string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statuses[request]
}

func TestAVectorStoreProviderAnswerDecidesWhatIsKeptWhatIsForgottenAndWhatTheCallerMayRepeat(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	provider := &storeProvider{issues: "1", objects: storeObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	ask := func(method, path, body, submission string, status int, code, class string) (http.Header, retainedObject) {
		t.Helper()
		before := len(sink.all())
		header, raw := g.send(t, method, path, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json", "Idempotency-Key": submission}, body, status)
		var reply struct {
			retainedObject
			Error struct{ Code string }
		}
		if err := json.Unmarshal(raw, &reply); err != nil || reply.Error.Code != code {
			t.Fatalf("%s %s answered %s (%v), want code %q", method, path, raw, err, code)
		}
		seen := sink.all()
		if len(seen) != before+1 {
			t.Fatalf("%s %s recorded %d requests, want one", method, path, len(seen)-before)
		}
		attempts := seen[before].Attempts
		if class == "" && len(attempts) != 0 || class != "" && (len(attempts) != 1 || attempts[0].Class != class) {
			t.Fatalf("%s %s recorded attempts %+v, want class %q", method, path, attempts, class)
		}
		return header, reply.retainedObject
	}
	kept := func(id string) string {
		t.Helper()
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT state FROM olp.provider_resources WHERE 'vector_store_'||replace(id::text,'-','')=$1`, id).Scan(&state); err != nil {
			t.Fatalf("read kept store %s: %v", id, err)
		}
		return state
	}
	change := func(id, submission string) string {
		t.Helper()
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT m.state FROM olp.retained_mutations m WHERE 'vector_store_'||replace(m.resource_id::text,'-','')=$1 AND m.submission_key=$2`, id, submission).Scan(&state); err != nil {
			t.Fatalf("read change %s of %s: %v", submission, id, err)
		}
		return state
	}
	const stores, named, paid = "/v1/vector_stores", `{"name":"notes"}`, `{"expires_after":{"anchor":"last_active_at","days":1}}`
	rejection, outage := `{"error":{"message":"The provider refused.","type":"invalid_request_error"}}`, `{"error":{"message":"The provider broke.","type":"server_error"}}`

	provider.answers("POST /vector_stores", http.StatusBadRequest, rejection)
	header, _ := ask("POST", stores, named, "refused", http.StatusBadRequest, "provider_resource_error", "upstream_client")
	refused := header.Get("X-OLP-Resource-ID")
	if header.Get("X-Should-Retry") != "false" || kept(refused) != "not_accepted" {
		t.Fatalf("a refused creation answered retry %q and is kept as %q", header.Get("X-Should-Retry"), kept(refused))
	}
	provider.recovers("POST /vector_stores")
	header, _ = ask("POST", stores, named, "refused", http.StatusConflict, "submission_exists", "")
	if header.Get("X-OLP-Resource-ID") != refused || len(upstream.sent("POST /vector_stores")) != 1 {
		t.Fatalf("repeating a refused creation named %q and reached the provider %d times", header.Get("X-OLP-Resource-ID"), len(upstream.sent("POST /vector_stores")))
	}

	provider.answers("POST /vector_stores", http.StatusInternalServerError, outage)
	header, _ = ask("POST", stores, named, "unknown", http.StatusInternalServerError, "provider_resource_error", "ambiguous")
	unknown := header.Get("X-OLP-Resource-ID")
	if kept(unknown) != "submission_unknown" {
		t.Fatalf("a creation the provider may have run is kept as %q", kept(unknown))
	}
	provider.recovers("POST /vector_stores")
	ask("GET", stores+"/"+unknown, "", "", http.StatusConflict, "submission_unknown", "")

	_, store := ask("POST", stores, named, "first", http.StatusOK, "", "success")
	for _, broken := range []struct {
		name, body string
		status     int
		code       string
		class      string
		want       int
	}{
		{"another store", providerStore("2"), 0, "provider_protocol_error", "protocol", http.StatusBadGateway},
		{"no object", `[]`, 0, "provider_protocol_error", "protocol", http.StatusBadGateway},
		{"an outage", outage, http.StatusInternalServerError, "provider_resource_error", "upstream_server", http.StatusInternalServerError},
	} {
		provider.answers("GET "+providerStorePath("1"), broken.status, broken.body)
		ask("GET", stores+"/"+store.ID, "", "", broken.want, broken.code, broken.class)
		if kept(store.ID) != "completed" {
			t.Fatalf("a read answered with %s left the store %q", broken.name, kept(store.ID))
		}
	}
	provider.recovers("GET " + providerStorePath("1"))

	ask("POST", stores+"/"+store.ID, paid, "accepted", http.StatusOK, "", "success")
	ask("POST", stores+"/"+store.ID, paid, "accepted", http.StatusConflict, "idempotency_conflict", "")
	provider.answers("POST "+providerStorePath("1"), http.StatusBadRequest, rejection)
	ask("POST", stores+"/"+store.ID, paid, "rejected", http.StatusBadRequest, "provider_resource_error", "upstream_client")
	provider.answers("POST "+providerStorePath("1"), http.StatusInternalServerError, outage)
	ask("POST", stores+"/"+store.ID, paid, "uncertain", http.StatusInternalServerError, "provider_resource_error", "ambiguous")
	provider.recovers("POST " + providerStorePath("1"))
	ask("POST", stores+"/"+store.ID, paid, "uncertain", http.StatusConflict, "idempotency_conflict", "")
	for submission, want := range map[string]string{"accepted": "accepted", "rejected": "not_accepted", "uncertain": "submission_unknown"} {
		if got := change(store.ID, submission); got != want {
			t.Fatalf("paid change %q is kept as %q, want %q", submission, got, want)
		}
	}
	if kept(store.ID) != "completed" {
		t.Fatalf("failed paid changes left the store %q", kept(store.ID))
	}

	provider.answers("DELETE "+providerStorePath("1"), 0, `{"id":"vs_`+providerOwned+`2","object":"vector_store.deleted","deleted":true}`)
	ask("DELETE", stores+"/"+store.ID, "", "", http.StatusBadGateway, "provider_protocol_error", "protocol")
	if kept(store.ID) != "deleted" {
		t.Fatalf("a deletion the provider accepted under another name left the store %q", kept(store.ID))
	}

	provider.creates("2")
	_, second := ask("POST", stores, named, "second", http.StatusOK, "", "success")
	ask("DELETE", stores+"/"+second.ID, "", "", http.StatusOK, "", "success")
	if kept(second.ID) != "deleted" {
		t.Fatalf("a deleted store is kept as %q", kept(second.ID))
	}

	provider.creates("3")
	_, gone := ask("POST", stores, named, "third", http.StatusOK, "", "success")
	provider.answers("GET "+providerStorePath("3"), http.StatusNotFound, rejection)
	ask("GET", stores+"/"+gone.ID, "", "", http.StatusNotFound, "not_found", "upstream_client")
	provider.recovers("GET " + providerStorePath("3"))
	ask("GET", stores+"/"+gone.ID, "", "", http.StatusNotFound, "not_found", "")
	if kept(gone.ID) != "deleted" || len(upstream.sent("GET "+providerStorePath("3"))) != 1 {
		t.Fatalf("a store the provider no longer has is kept as %q after %d provider reads", kept(gone.ID), len(upstream.sent("GET "+providerStorePath("3"))))
	}
}

type storePage struct {
	status      int
	stores      string
	more        bool
	first, last string
	code        string
	reads       int
	attempts    string
}

func TestAVectorStoreListPagesNewestFirstRefillsPastLostStoresAndRefusesWhatItCannotPage(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	provider := &storeProvider{issues: "1", objects: storeObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	numbered, issued := map[string]string{}, map[string]string{}
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		provider.creates(n)
		_, raw := g.send(t, "POST", "/v1/vector_stores", map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json", "Idempotency-Key": "store-" + n}, `{"name":"notes"}`, http.StatusOK)
		var store retainedObject
		if err := json.Unmarshal(raw, &store); err != nil || store.ID == "" {
			t.Fatalf("create store %s answered %s: %v", n, raw, err)
		}
		numbered[store.ID], issued[n] = n, store.ID
	}
	reads := func() int {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		return len(slices.DeleteFunc(slices.Clone(upstream.calls), func(call upstreamCall) bool { return !strings.HasPrefix(call.request, "GET ") }))
	}
	list := func(query, route string, want int) storePage {
		t.Helper()
		recorded, read := len(sink.all()), reads()
		_, raw := g.send(t, "GET", "/v1/vector_stores"+query, map[string]string{"Authorization": "Bearer owner", "X-OLP-Route": route}, "", want)
		var reply struct {
			Data    []struct{ ID string }
			HasMore bool   `json:"has_more"`
			FirstID string `json:"first_id"`
			LastID  string `json:"last_id"`
			Error   struct{ Code string }
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("list %s answered %s: %v", query, raw, err)
		}
		page := storePage{status: want, more: reply.HasMore, first: numbered[reply.FirstID], last: numbered[reply.LastID], code: reply.Error.Code, reads: reads() - read}
		for _, store := range reply.Data {
			page.stores += numbered[store.ID]
		}
		seen := sink.all()
		if len(seen) != recorded+1 {
			t.Fatalf("list %s recorded %d requests, want one", query, len(seen)-recorded)
		}
		for _, attempt := range seen[recorded].Attempts {
			page.attempts += fmt.Sprintf("%s:%v ", attempt.Class, attempt.Committed)
		}
		return page
	}
	delivered := func(n int) string { return strings.Repeat("success:true ", n) }
	refused := storePage{status: http.StatusBadRequest, code: "invalid_parameter"}

	for _, page := range []struct {
		name, query string
		want        storePage
	}{
		{"the whole list, newest first", "", storePage{status: 200, stores: "54321", first: "5", last: "1", reads: 5, attempts: delivered(5)}},
		{"a first page", "?limit=2", storePage{status: 200, stores: "54", more: true, first: "5", last: "4", reads: 2, attempts: delivered(2)}},
		{"the page after a store", "?limit=2&after=" + issued["4"], storePage{status: 200, stores: "32", more: true, first: "3", last: "2", reads: 2, attempts: delivered(2)}},
		{"the last page", "?limit=2&after=" + issued["2"], storePage{status: 200, stores: "1", first: "1", last: "1", reads: 1, attempts: delivered(1)}},
		{"a page that ends with the list", "?limit=5", storePage{status: 200, stores: "54321", first: "5", last: "1", reads: 5, attempts: delivered(5)}},
		{"oldest first", "?order=asc&limit=3", storePage{status: 200, stores: "123", more: true, first: "1", last: "3", reads: 3, attempts: delivered(3)}},
		{"an unknown parameter", "?filter=notes", refused},
		{"a repeated parameter", "?limit=2&limit=3", refused},
		{"an unknown order", "?order=sideways", refused},
		{"an empty page", "?limit=0", refused},
		{"a page over the largest size", "?limit=101", refused},
		{"a cursor that is no store of this key", "?after=vector_store_00000000000000000000000000000000", refused},
	} {
		if got := list(page.query, slug, page.want.status); got != page.want {
			t.Fatalf("%s is %+v, want %+v", page.name, got, page.want)
		}
	}
	if got, want := list("", "another-route", 200), (storePage{status: 200}); got != want {
		t.Fatalf("the stores of another route are %+v, want none and no provider read", got)
	}

	for _, n := range []string{"3", "4"} {
		provider.answers("GET "+providerStorePath(n), http.StatusNotFound, `{"error":{"message":"No such store.","type":"invalid_request_error"}}`)
	}
	lost := storePage{status: 200, stores: "52", more: true, first: "5", last: "2", reads: 4, attempts: "success:true upstream_client:false upstream_client:false success:true "}
	if got := list("?limit=2", slug, 200); got != lost {
		t.Fatalf("a page over two stores the provider lost is %+v, want %+v", got, lost)
	}
	forgotten := storePage{status: 200, stores: "52", more: true, first: "5", last: "2", reads: 2, attempts: delivered(2)}
	if got := list("?limit=2", slug, 200); got != forgotten {
		t.Fatalf("the page after the lost stores were forgotten is %+v, want %+v", got, forgotten)
	}

	for _, broken := range []struct {
		name, body string
		status     int
		want       storePage
	}{
		{"another store", providerStore("1"), 0, storePage{status: http.StatusBadGateway, code: "provider_protocol_error", reads: 2, attempts: "success:false protocol:false "}},
		{"an outage", `{"error":{"message":"The provider broke.","type":"server_error"}}`, http.StatusInternalServerError, storePage{status: http.StatusInternalServerError, code: "provider_resource_error", reads: 2, attempts: "success:false upstream_server:false "}},
	} {
		provider.answers("GET "+providerStorePath("2"), broken.status, broken.body)
		if got := list("", slug, broken.want.status); got != broken.want {
			t.Fatalf("a list whose second store is answered with %s is %+v, want %+v", broken.name, got, broken.want)
		}
	}
	provider.recovers("GET " + providerStorePath("2"))

	if _, err := pool.Exec(t.Context(), `UPDATE olp.provider_resources SET state='submission_unknown' WHERE 'vector_store_'||replace(id::text,'-','')=$1`, issued["2"]); err != nil {
		t.Fatal(err)
	}
	unsettled := storePage{status: 200, stores: "51", first: "5", last: "1", reads: 2, attempts: delivered(2)}
	if got := list("", slug, 200); got != unsettled {
		t.Fatalf("a list over a store whose creation is unknown is %+v, want %+v", got, unsettled)
	}
}

func TestStoreSearchAndCompactionTranslateFileIdentifiersBothWaysAccountTheirWorkAndRefuseWhatTheyCannotBind(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	const slug, model = "stores", "stores-model"
	price := rates.NewRoutingPrice(rates.RoutingPrice{
		Price:       rates.Price{ProviderKind: "azure_openai", Model: model, Operation: "vector_store", StoragePerGiBDay: new("0.10"), Currency: "USD"},
		EffectiveAt: fixture.observedAt,
	})
	provider := &storeProvider{issues: "1", objects: storeObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch", "vector_store", "retrieval"},
		prices: []rates.RoutingPrice{*price}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	g.key(t, "stateless")
	g.key(t, "elsewhere")
	stateless, elsewhere := g.runtime.keys["stateless"], g.runtime.keys["elsewhere"]
	stateless.Policy.AllowProviderState, elsewhere.Policy.AllowedRoutes = false, []string{"another-route"}
	g.runtime.keys["stateless"], g.runtime.keys["elsewhere"] = stateless, elsewhere
	const providerFile = "file-" + providerOwned
	provider.answers("POST /files", 200, `{"id":"`+providerFile+`","object":"file","bytes":5,"created_at":1,"filename":"a.txt","purpose":"assistants","status":"processed"}`)
	provider.answers("POST "+providerStorePath("1")+"/files", 200, `{"id":"`+providerFile+`","object":"vector_store.file","vector_store_id":"vs_`+providerOwned+`1","status":"completed","created_at":1}`)
	created := func(path, contentType, body, submission string) string {
		t.Helper()
		_, raw := g.send(t, "POST", path, map[string]string{"Authorization": "Bearer owner", "Content-Type": contentType, "Idempotency-Key": submission}, body, http.StatusOK)
		var object retainedObject
		if err := json.Unmarshal(raw, &object); err != nil || object.ID == "" {
			t.Fatalf("POST %s answered %s: %v", path, raw, err)
		}
		return object.ID
	}
	const form = "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nassistants\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	file := created("/v1/files", "multipart/form-data; boundary=b", form, "file")
	store := created("/v1/vector_stores", "application/json", `{"name":"notes"}`, "store")
	created("/v1/vector_stores/"+store+"/files", "application/json", `{"file_id":"`+file+`"}`, "attachment")

	const search, compact = "/search", "/v1/responses/compact"
	searched, compacted := "POST "+providerStorePath("1")+search, "POST /responses/compact"
	found := func(fileID string) string {
		return `{"object":"vector_store.search_results.page","search_query":["q"],"data":[{"file_id":"` + fileID + `","score":0.5,"content":[{"type":"text","text":"hello"}]}],"has_more":false}`
	}
	const usage = `,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}`
	summary := func(item, usage string) string {
		return `{"object":"response.compaction","id":"cmp_1","output":[` + item + `]` + usage + `}`
	}
	const kept = `{"type":"compaction","id":"item_1","encrypted_content":"abc"}`
	attached := func(fileID string) string { return `{"type":"input_file","file_id":"` + fileID + `"}` }
	about := func(fileID string) string {
		return `{"model":"` + slug + `","input":[{"role":"user","content":[` + attached(fileID) + `]}]}`
	}
	const question, text = `{"query":"q"}`, `{"model":"` + slug + `","input":"shorten this"}`
	type work struct {
		name, key, path, query, route, body string
		answer                              string
		answers                             int
		status                              int
		code                                string
		reply, sent                         string
		attempt                             string
		tokens                              int64
	}
	for _, tc := range []work{
		{name: "a search names the caller's file", path: search, body: question, answer: found(providerFile), status: 200, reply: `"file_id":"` + file + `"`, sent: question, attempt: "success committed tokenless"},
		{name: "a search naming a file the caller never bound", path: search, body: question, answer: found("file-elsewhere"), status: 502, code: "provider_protocol_error", attempt: "protocol"},
		{name: "a search the provider cannot answer", path: search, body: question, answer: `{"error":{"message":"down"}}`, answers: 500, status: 500, code: "provider_resource_error", attempt: "ambiguous"},
		{name: "a search the provider throttles", path: search, body: question, answer: `{"error":{"message":"slow"}}`, answers: 429, status: 429, code: "provider_resource_error", attempt: "rate_limit"},
		{name: "a search answered with a list", path: search, body: question, answer: `[]`, status: 502, code: "provider_protocol_error", attempt: "protocol"},
		{name: "a search with a query parameter", path: search, query: "?limit=1", body: question, status: 400, code: "invalid_parameter"},
		{name: "a search that is not an object", path: search, body: `[1]`, status: 400, code: "invalid_request"},
		{name: "another key's search", key: "stranger", path: search, body: question, status: 404, code: "not_found"},
		{name: "a compaction is accounted by its tokens", path: compact, route: slug, body: text, answer: summary(kept, usage), status: 200, reply: `"encrypted_content":"abc"`, sent: `{"model":"` + model + `","input":"shorten this"}`, attempt: "success committed", tokens: 15},
		{name: "a compaction over the caller's file", path: compact, route: slug, body: about(file), answer: summary(kept, usage), status: 200, sent: attached(providerFile), attempt: "success committed", tokens: 15},
		{name: "a compaction returning the caller's file", path: compact, route: slug, body: text, answer: summary(attached(providerFile), usage), status: 200, reply: attached(file), attempt: "success committed", tokens: 15},
		{name: "a compaction returning a file the caller never bound keeps its usage", path: compact, route: slug, body: text, answer: summary(attached("file-elsewhere"), usage), status: 502, code: "provider_protocol_error", attempt: "protocol", tokens: 15},
		{name: "a compaction with usage that cannot be read", path: compact, route: slug, body: text, answer: summary(kept, `,"usage":{"input_tokens":-1}`), status: 502, code: "provider_protocol_error", attempt: "protocol"},
		{name: "a compaction the provider cannot answer", path: compact, route: slug, body: text, answer: `{"error":{"message":"down"}}`, answers: 500, status: 500, code: "provider_resource_error", attempt: "ambiguous"},
		{name: "a compaction over a file the caller does not own", path: compact, route: slug, body: about("file_00000000000040008000000000000000"), status: 400, code: "invalid_resource_reference"},
		{name: "a compaction after a response the caller does not own", path: compact, route: slug, body: `{"input":"x","previous_response_id":"response_00000000000040008000000000000000"}`, status: 400, code: "invalid_resource_reference"},
		{name: "a compaction by a key that may not keep provider state", key: "stateless", path: compact, route: slug, body: text, status: 403, code: "provider_state_forbidden"},
		{name: "a compaction by a key that may not use the route", key: "elsewhere", path: compact, route: slug, body: text, status: 403, code: "route_forbidden"},
		{name: "a compaction on an unknown route", path: compact, route: "elsewhere", body: text, status: 404, code: "route_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, request := tc.path, compacted
			if tc.path == search {
				path, request = "/v1/vector_stores/"+store+search, searched
			}
			if tc.answer != "" {
				provider.answers(request, cmp.Or(tc.answers, 200), tc.answer)
			}
			recorded, asked := len(sink.all()), len(upstream.sent(request))
			_, raw := g.send(t, "POST", path+tc.query, map[string]string{"Authorization": "Bearer " + cmp.Or(tc.key, "owner"), "Content-Type": "application/json", "X-OLP-Route": tc.route}, tc.body, tc.status)
			var reply struct{ Error struct{ Code string } }
			if err := json.Unmarshal(raw, &reply); err != nil || reply.Error.Code != tc.code || !strings.Contains(string(raw), tc.reply) {
				t.Fatalf("reply = %s (%v), want code %q containing %q", raw, err, tc.code, tc.reply)
			}
			calls := upstream.sent(request)[asked:]
			if (tc.answer != "") != (len(calls) == 1) || (tc.sent != "" && !strings.Contains(calls[0].body, tc.sent)) {
				t.Fatalf("the provider received %+v, want a request only when it answers, containing %q", calls, tc.sent)
			}
			for _, call := range calls {
				if call.key != "listed-secret" || strings.Contains(call.body, file) || strings.Contains(call.body, store) {
					t.Fatalf("the provider received %+v, want the provider credential and no gateway identifier", call)
				}
			}
			seen := sink.all()
			if len(seen) != recorded+1 {
				t.Fatalf("recorded %d requests, want one", len(seen)-recorded)
			}
			got, tokens := "", int64(0)
			for _, attempt := range seen[recorded].Attempts {
				got += attempt.Class
				if attempt.Committed {
					got += " committed"
				}
				if attempt.TokenlessUsage {
					got += " tokenless"
				}
				if attempt.Usage != nil {
					tokens += attempt.Usage.TotalTokens
				}
			}
			if got != tc.attempt || tokens != tc.tokens {
				t.Fatalf("attempts = %q with %d tokens, want %q with %d", got, tokens, tc.attempt, tc.tokens)
			}
		})
	}

	t.Run("a fresh search uses the live mapping after provider ID reuse", func(t *testing.T) {
		provider.answers("DELETE /files/"+providerFile, 200, `{"id":"`+providerFile+`","deleted":true}`)
		g.call(t, "owner", "DELETE", "/v1/files/"+file, "", "", 200)
		replacement := created("/v1/files", "multipart/form-data; boundary=b", form, "replacement-file")
		if replacement == file {
			t.Fatalf("replacement reused deleted gateway file ID %q", file)
		}
		provider.answers(searched, 200, found(providerFile))
		_, raw := g.send(t, "POST", "/v1/vector_stores/"+store+search, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}, question, 200)
		if !strings.Contains(string(raw), `"file_id":"`+replacement+`"`) {
			t.Fatalf("search returned %s, want replacement file %q", raw, replacement)
		}
	})

	unasked := func(path, route, want string) {
		t.Helper()
		asked := len(upstream.sent(searched)) + len(upstream.sent(compacted))
		request, err := http.NewRequest("POST", g.server.URL+path, strings.NewReader(text))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer owner")
		request.Header.Set("Content-Type", "application/json")
		if route != "" {
			request.Header.Set("X-OLP-Route", route)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if !strings.Contains(string(raw), `"code":"`+want+`"`) || len(upstream.sent(searched))+len(upstream.sent(compacted)) != asked {
			t.Fatalf("%s on route %q answered %d %s, want %s and no provider request", path, route, response.StatusCode, raw, want)
		}
	}
	unasked(compact, "", "missing_required_parameter")
	g.republish(t, func(route *routing.Route) { route.Operations = []string{"vector_store", "batch"} })
	unasked(compact, slug, "operation_unavailable")
	unasked("/v1/vector_stores/"+store+search, slug, "provider_resource_credential_unavailable")
}

func providerAgent(n, base string) string {
	if base != "" {
		return `{"id":"agent-` + providerOwned + n + `","description":"helper","base_agent":"agent-` + providerOwned + base + `"}`
	}
	return `{"id":"agent-` + providerOwned + n + `","description":"helper"}`
}

func providerAgentPath(n string) string { return "/agents/agent-" + providerOwned + n }

func agentObjects(issues string) map[string]string {
	replies := map[string]string{"POST /agents": providerAgent(issues, "")}
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		replies["GET "+providerAgentPath(n)], replies["DELETE "+providerAgentPath(n)] = providerAgent(n, ""), `{}`
	}
	return replies
}

func TestAgentResourcesBindTheirBaseAgentPageNewestFirstAndKeepOrForgetWhatEachProviderAnswerDecides(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	provider := &storeProvider{issues: "1", objects: agentObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "agents", model: "agents-model", kind: "gemini", surface: "gemini", profile: "gemini-agents", version: "/v1beta", secure: true,
		fidelity: routing.FidelityStrict, operations: []string{"agent_resource"}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	g.key(t, "stateless")
	stateless := g.runtime.keys["stateless"]
	stateless.Policy.AllowProviderState = false
	g.runtime.keys["stateless"] = stateless
	type agentReply struct {
		ID            string
		BaseAgent     string `json:"base_agent"`
		Agents        []struct{ ID string }
		NextPageToken string `json:"next_page_token"`
	}
	ask := func(key, method, path, body, submission string, status int, reason string, classes ...string) (http.Header, agentReply) {
		t.Helper()
		before := len(sink.all())
		contentType := ""
		if body != "" {
			contentType = "application/json"
		}
		header, raw := g.send(t, method, path, map[string]string{"x-goog-api-key": key, "Content-Type": contentType, "Idempotency-Key": submission}, body, status)
		var reply struct {
			agentReply
			Error struct{ Details []struct{ Reason string } }
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("%s %s answered %s: %v", method, path, raw, err)
		}
		got := ""
		if len(reply.Error.Details) > 0 {
			got = reply.Error.Details[0].Reason
		}
		if got != reason {
			t.Fatalf("%s %s answered %s, want reason %q", method, path, raw, reason)
		}
		seen := sink.all()
		if len(seen) != before+1 {
			t.Fatalf("%s %s recorded %d requests, want one", method, path, len(seen)-before)
		}
		recorded := make([]string, 0, len(seen[before].Attempts))
		for _, attempt := range seen[before].Attempts {
			recorded = append(recorded, attempt.Class)
		}
		if !slices.Equal(recorded, classes) {
			t.Fatalf("%s %s recorded attempts %v, want %v", method, path, recorded, classes)
		}
		return header, reply.agentReply
	}
	kept := func(id string) string {
		t.Helper()
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT state FROM olp.provider_resources WHERE 'agent_resource_'||replace(id::text,'-','')=$1`, id).Scan(&state); err != nil {
			t.Fatalf("read kept agent %s: %v", id, err)
		}
		return state
	}
	listed := func(reply agentReply) []string {
		ids := make([]string, 0, len(reply.Agents))
		for _, agent := range reply.Agents {
			ids = append(ids, agent.ID)
		}
		return ids
	}
	const agents, helper = "/gemini/v1beta/agents", `{"description":"helper"}`
	rejection, outage, lost := `{"error":{"code":400,"message":"The provider refused.","status":"INVALID_ARGUMENT"}}`, `{"error":{"code":500,"message":"The provider broke.","status":"INTERNAL"}}`, `{"error":{"code":404,"message":"The provider lost it.","status":"NOT_FOUND"}}`

	for _, refused := range []struct {
		key, method, path, body, submission string
		status                              int
		reason                              string
	}{
		{"owner", "POST", agents, helper, "", http.StatusBadRequest, "missing_required_parameter"},
		{"stateless", "POST", agents, helper, "refused", http.StatusForbidden, "provider_state_forbidden"},
		{"owner", "POST", agents, `{"name":"helper"}`, "refused", http.StatusBadRequest, "unsupported_field"},
		{"owner", "POST", agents, `{"base_environment":{"env":{"TOKEN":{"credential":"secret"}}}}`, "refused", http.StatusUnprocessableEntity, "unsupported_stateful_reference"},
		{"owner", "POST", agents, `[]`, "refused", http.StatusBadRequest, "invalid_request"},
		{"owner", "GET", agents + "?filter=helper", "", "", http.StatusBadRequest, "unsupported_parameter"},
		{"owner", "GET", agents + "?page_size=0", "", "", http.StatusBadRequest, "invalid_parameter"},
		{"owner", "GET", agents + "?page_token=agent_resource_unknown", "", "", http.StatusBadRequest, "invalid_parameter"},
	} {
		header, _ := ask(refused.key, refused.method, refused.path, refused.body, refused.submission, refused.status, refused.reason)
		if refused.method == "POST" && refused.key == "owner" && header.Get("X-Should-Retry") != "false" {
			t.Fatalf("refused creation %s answered retry hint %q", refused.body, header.Get("X-Should-Retry"))
		}
	}
	if sent := upstream.sent("POST /agents"); len(sent) != 0 {
		t.Fatalf("refused requests reached the provider: %+v", sent)
	}

	header, first := ask("owner", "POST", agents, `{"description":"helper","base_agent":""}`, "first", http.StatusOK, "", "success")
	sent := upstream.sent("POST /agents")
	if !strings.HasPrefix(first.ID, "agent_resource_") || header.Get("X-OLP-Resource-ID") != first.ID || len(sent) != 1 || sent[0].key != "listed-secret" || sent[0].body != helper || kept(first.ID) != "active" {
		t.Fatalf("creation answered %+v with %q, sent %+v and is kept as %q", first, header.Get("X-OLP-Resource-ID"), sent, kept(first.ID))
	}
	header, _ = ask("owner", "POST", agents, `{"description":"helper","base_agent":""}`, "first", http.StatusConflict, "submission_exists")
	ask("owner", "POST", agents, `{"description":"another"}`, "first", http.StatusConflict, "idempotency_conflict")
	if header.Get("X-OLP-Resource-ID") != first.ID || len(upstream.sent("POST /agents")) != 1 {
		t.Fatalf("a repeated creation named %q and reached the provider %d times", header.Get("X-OLP-Resource-ID"), len(upstream.sent("POST /agents")))
	}

	provider.answers("POST /agents", 0, providerAgent("2", "1"))
	provider.answers("GET "+providerAgentPath("2"), 0, providerAgent("2", "1"))
	based := `{"description":"child","base_agent":"` + first.ID + `"}`
	ask("stranger", "POST", agents, based, "derived", http.StatusBadRequest, "invalid_resource_reference")
	_, derived := ask("owner", "POST", agents, based, "derived", http.StatusOK, "", "success")
	sent = upstream.sent("POST /agents")
	if derived.BaseAgent != first.ID || len(sent) != 2 || sent[1].body != `{"description":"child","base_agent":"agent-`+providerOwned+`1"}` {
		t.Fatalf("an agent built on another answered %+v and sent %+v", derived, sent)
	}
	if _, read := ask("owner", "GET", agents+"/"+derived.ID, "", "", http.StatusOK, "", "success"); read.ID != derived.ID || read.BaseAgent != first.ID {
		t.Fatalf("reading the derived agent answered %+v", read)
	}
	ask("stranger", "GET", agents+"/"+derived.ID, "", "", http.StatusNotFound, "not_found")
	ask("stranger", "DELETE", agents+"/"+derived.ID, "", "", http.StatusNotFound, "not_found")

	_, page := ask("owner", "GET", agents, "", "", http.StatusOK, "", "success", "success")
	if !slices.Equal(listed(page), []string{derived.ID, first.ID}) || page.NextPageToken != "" {
		t.Fatalf("the list answered %+v, want the newest agent first", page)
	}
	_, page = ask("owner", "GET", agents+"?page_size=1", "", "", http.StatusOK, "", "success")
	if !slices.Equal(listed(page), []string{derived.ID}) || page.NextPageToken != derived.ID {
		t.Fatalf("the first page answered %+v", page)
	}
	_, page = ask("owner", "GET", agents+"?page_size=1&page_token="+page.NextPageToken, "", "", http.StatusOK, "", "success")
	if !slices.Equal(listed(page), []string{first.ID}) || page.NextPageToken != "" {
		t.Fatalf("the second page answered %+v", page)
	}
	if _, page = ask("stranger", "GET", agents, "", "", http.StatusOK, ""); len(page.Agents) != 0 {
		t.Fatalf("another key's list answered %+v", page)
	}

	for _, broken := range []struct {
		name, body, reason, class string
		status, want              int
	}{
		{"another agent", providerAgent("2", ""), "provider_protocol_error", "protocol", 0, http.StatusBadGateway},
		{"an unknown base agent", providerAgent("1", "5"), "provider_protocol_error", "protocol", 0, http.StatusBadGateway},
		{"an outage", outage, "provider_resource_error", "upstream_server", http.StatusInternalServerError, http.StatusInternalServerError},
	} {
		provider.answers("GET "+providerAgentPath("1"), broken.status, broken.body)
		ask("owner", "GET", agents+"/"+first.ID, "", "", broken.want, broken.reason, broken.class)
		ask("owner", "GET", agents, "", "", broken.want, broken.reason, "success", broken.class)
		if kept(first.ID) != "active" {
			t.Fatalf("a read answered with %s left the agent %q", broken.name, kept(first.ID))
		}
	}
	provider.recovers("GET " + providerAgentPath("1"))
	provider.answers("DELETE "+providerAgentPath("1"), http.StatusInternalServerError, outage)
	ask("owner", "DELETE", agents+"/"+first.ID, "", "", http.StatusInternalServerError, "provider_resource_error", "upstream_server")
	if kept(first.ID) != "active" {
		t.Fatalf("a deletion the provider failed left the agent %q", kept(first.ID))
	}

	provider.answers("POST /agents", http.StatusBadRequest, rejection)
	header, _ = ask("owner", "POST", agents, helper, "refused", http.StatusBadRequest, "provider_resource_error", "upstream_client")
	refused := header.Get("X-OLP-Resource-ID")
	if state := kept(refused); state != "not_accepted" {
		t.Fatalf("a refused agent creation is kept as %q, want not_accepted", state)
	}
	provider.answers("POST /agents", http.StatusInternalServerError, outage)
	header, _ = ask("owner", "POST", agents, helper, "unknown", http.StatusInternalServerError, "provider_resource_error", "ambiguous")
	unknown := header.Get("X-OLP-Resource-ID")
	provider.answers("POST /agents", 0, `{"description":"helper"}`)
	header, _ = ask("owner", "POST", agents, helper, "unnamed", http.StatusBadGateway, "provider_protocol_error", "protocol")
	unnamed := header.Get("X-OLP-Resource-ID")
	provider.recovers("POST /agents")
	before := len(upstream.sent("POST /agents"))
	for submission, id := range map[string]string{"refused": refused, "unknown": unknown, "unnamed": unnamed} {
		header, _ = ask("owner", "POST", agents, helper, submission, http.StatusConflict, "submission_exists")
		if id == "" || header.Get("X-OLP-Resource-ID") != id || header.Get("X-Should-Retry") != "false" {
			t.Fatalf("repeating creation %q named %q, want %q", submission, header.Get("X-OLP-Resource-ID"), id)
		}
		ask("owner", "GET", agents+"/"+id, "", "", http.StatusConflict, "submission_unknown")
		ask("owner", "DELETE", agents+"/"+id, "", "", http.StatusConflict, "submission_unknown")
	}
	if len(upstream.sent("POST /agents")) != before || kept(unknown) != "submission_unknown" || kept(unnamed) != "submission_unknown" {
		t.Fatalf("unsettled creations reached the provider %d more times and are kept as %q and %q", len(upstream.sent("POST /agents"))-before, kept(unknown), kept(unnamed))
	}

	provider.answers("POST /agents", 0, providerAgent("3", "5"))
	header, _ = ask("owner", "POST", agents, helper, "orphan", http.StatusBadGateway, "provider_protocol_error", "protocol")
	orphan := header.Get("X-OLP-Resource-ID")
	if kept(orphan) != "active" {
		t.Fatalf("an agent the provider created on an unknown base is kept as %q, so its owner could not delete it", kept(orphan))
	}
	ask("owner", "DELETE", agents+"/"+orphan, "", "", http.StatusOK, "", "success")
	ask("owner", "GET", agents+"/"+orphan, "", "", http.StatusNotFound, "not_found")
	if len(upstream.sent("DELETE "+providerAgentPath("3"))) != 1 || kept(orphan) != "deleted" {
		t.Fatalf("deleting the agent reached the provider %d times and left it %q", len(upstream.sent("DELETE "+providerAgentPath("3"))), kept(orphan))
	}

	provider.answers("GET "+providerAgentPath("1"), http.StatusNotFound, lost)
	if _, page = ask("owner", "GET", agents, "", "", http.StatusOK, "", "success", "upstream_client"); !slices.Equal(listed(page), []string{derived.ID}) || kept(first.ID) != "deleted" {
		t.Fatalf("a list past an agent the provider lost answered %+v and left it %q", page, kept(first.ID))
	}
	ask("owner", "GET", agents+"/"+first.ID, "", "", http.StatusNotFound, "not_found")
	provider.answers("GET "+providerAgentPath("2"), http.StatusNotFound, lost)
	ask("owner", "GET", agents+"/"+derived.ID, "", "", http.StatusNotFound, "not_found", "upstream_client")
	if _, page = ask("owner", "GET", agents, "", "", http.StatusOK, ""); len(page.Agents) != 0 || kept(derived.ID) != "deleted" {
		t.Fatalf("after the provider lost every agent the list answered %+v and the last is %q", page, kept(derived.ID))
	}
}

func TestABatchFileUploadIsCheckedBeforeTheProviderKeptOnlyWhenTheProviderNamesItAndRefusedOffABatchRoute(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "upload-route", "batch-model"
	provider := &storeProvider{issues: "1", scripted: map[string]string{}, statuses: map[string]int{}, objects: func(issues string) map[string]string {
		return map[string]string{"POST /files": `{"id":"file-` + providerOwned + issues + `","object":"file","bytes":7,"created_at":1700000000,"filename":"batch.jsonl","purpose":"batch","status":"processed"}`}
	}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch"}, upstream: upstream, sink: sink,
	})
	owner := g.key(t, "owner")
	g.key(t, "elsewhere")
	elsewhere := g.runtime.keys["elsewhere"]
	elsewhere.Policy.AllowedRoutes = []string{"another"}
	g.runtime.keys["elsewhere"] = elsewhere
	part := func(name, filename, content string) string {
		disposition := `form-data; name="` + name + `"`
		if filename != "" {
			disposition += `; filename="` + filename + "\"\r\nContent-Type: application/jsonl"
		}
		return "--b\r\nContent-Disposition: " + disposition + "\r\n\r\n" + content + "\r\n"
	}
	line := func(model string) string {
		return `{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}}` + "\n"
	}
	kept := func() []string {
		t.Helper()
		rows, err := pool.Query(t.Context(), `SELECT upstream_id FROM olp.provider_resources WHERE api_key_id=$1 ORDER BY upstream_id`, owner)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	upload := func(name, key, route, form string, status int, code string, classes ...string) retainedObject {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), "POST", g.server.URL+"/v1/files", strings.NewReader(form+"--b--\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		if route != "" {
			request.Header.Set("X-OLP-Route", route)
		}
		recorded, sent, stored := len(sink.all()), len(upstream.sent("POST /files")), kept()
		response, err := g.server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		var reply struct {
			retainedObject
			Error struct{ Code string }
		}
		if err := json.Unmarshal(raw, &reply); err != nil || response.StatusCode != status || reply.Error.Code != code {
			t.Fatalf("%s answered %d %s (%v), want %d with code %q", name, response.StatusCode, raw, err, status, code)
		}
		if strings.Contains(string(raw)+fmt.Sprint(response.Header), providerOwned) {
			t.Fatalf("%s exposed a provider identifier: %v %s", name, response.Header, raw)
		}
		seen := sink.all()
		if len(seen) != recorded+1 {
			t.Fatalf("%s recorded %d requests, want one", name, len(seen)-recorded)
		}
		attempted := make([]string, len(seen[recorded].Attempts))
		for i, attempt := range seen[recorded].Attempts {
			attempted[i] = attempt.Class
		}
		if !slices.Equal(attempted, classes) {
			t.Fatalf("%s recorded attempts %v, want %v", name, attempted, classes)
		}
		if asked := len(upstream.sent("POST /files")) - sent; asked != len(classes) {
			t.Fatalf("%s reached the provider %d times, want %d", name, asked, len(classes))
		}
		if status != http.StatusOK && !slices.Equal(kept(), stored) {
			t.Fatalf("%s failed but changed the kept files from %v to %v", name, stored, kept())
		}
		return reply.retainedObject
	}
	batch := part("purpose", "", "batch") + part("file", "batch.jsonl", line(slug))
	for _, refused := range []struct {
		name, key, route, form string
		status                 int
		code                   string
	}{
		{"an upload naming no route", "owner", "", batch, http.StatusBadRequest, "missing_required_parameter"},
		{"an upload naming an unknown route", "owner", "nowhere", batch, http.StatusNotFound, "route_not_found"},
		{"an upload by a key without the route", "elsewhere", slug, batch, http.StatusForbidden, "route_forbidden"},
		{"an upload without a file", "owner", slug, part("purpose", "", "batch"), http.StatusBadRequest, "missing_required_parameter"},
		{"an upload without a purpose", "owner", slug, part("file", "batch.jsonl", line(slug)), http.StatusBadRequest, "invalid_request"},
		{"an upload of two files", "owner", slug, batch + part("file", "more.jsonl", line(slug)), http.StatusBadRequest, "invalid_request"},
		{"a batch file that is not JSON lines", "owner", slug, part("purpose", "", "batch") + part("file", "batch.jsonl", "not json\n"), http.StatusBadRequest, "unsupported_parameter"},
		{"a batch line for another model", "owner", slug, part("purpose", "", "batch") + part("file", "batch.jsonl", line("another")), http.StatusBadRequest, "unsupported_parameter"},
	} {
		upload(refused.name, refused.key, refused.route, refused.form, refused.status, refused.code)
	}

	provider.answers("POST /files", http.StatusInternalServerError, `{"error":{"message":"The provider broke.","type":"server_error"}}`)
	upload("an upload the provider fails", "owner", slug, batch, http.StatusInternalServerError, "", "ambiguous")
	provider.answers("POST /files", http.StatusBadRequest, `{"error":{"message":"The provider refused.","type":"invalid_request_error"}}`)
	upload("an upload the provider refuses", "owner", slug, batch, http.StatusBadRequest, "", "upstream_client")
	provider.answers("POST /files", http.StatusOK, `{"object":"file"}`)
	upload("an upload the provider does not name", "owner", slug, batch, http.StatusBadGateway, "upstream_error", "protocol")
	provider.answers("POST /files", http.StatusOK, `not json`)
	upload("an upload the provider answers unreadably", "owner", slug, batch, http.StatusBadGateway, "upstream_error", "protocol")
	provider.recovers("POST /files")

	file := upload("a batch upload", "owner", slug, batch+part("note", "", "kept"), http.StatusOK, "", "success")
	sent := upstream.sent("POST /files")
	if body := sent[len(sent)-1].body; !strings.Contains(body, `"model":"`+model+`"`) || strings.Contains(body, slug) || !strings.Contains(body, `name="note"`) {
		t.Fatalf("the provider received %q, want the provider model and the caller's extra field", body)
	}
	var state string
	var manifest struct {
		Adapter, Endpoint string
		LineCount         int    `json:"line_count"`
		Operation         string `json:"line_operation"`
	}
	if err := pool.QueryRow(t.Context(), `SELECT state, metadata->'batch_manifest' FROM olp.provider_resources WHERE 'file_'||replace(id::text,'-','')=$1 AND upstream_id=$2 AND metadata->>'upstream_model'=$3`,
		file.ID, "file-"+providerOwned+"1", model).Scan(&state, &manifest); err != nil {
		t.Fatalf("the uploaded file %s is not kept under the provider's name and model: %v", file.ID, err)
	}
	if state != "processed" || manifest.Endpoint != "/v1/chat/completions" || manifest.LineCount != 1 || manifest.Operation != "generation" {
		t.Fatalf("the uploaded file is kept as %q with manifest %+v, want the provider's status and one generation line", state, manifest)
	}

	g.republish(t, func(route *routing.Route) { route.Operations = []string{"generation"} })
	upload("a batch upload on a route without batches", "owner", slug, batch, http.StatusBadRequest, "invalid_request")
}

type responseStreams struct {
	*retainedUpstream
	mu      sync.Mutex
	queries []string
	body    string
	kind    string
	status  int
}

func (p *responseStreams) answers(status int, kind string, events ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries, p.body, p.kind, p.status = nil, strings.Join(events, ""), kind, status
}

func (p *responseStreams) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.queries)
}

func (p *responseStreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || r.URL.Query().Get("stream") != "true" {
		p.retainedUpstream.ServeHTTP(w, r)
		return
	}
	p.mu.Lock()
	p.queries = append(p.queries, r.URL.RawQuery)
	body, kind, status := p.body, p.kind, p.status
	p.mu.Unlock()
	w.Header().Set("Content-Type", kind)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func responseEvent(sequence int, kind, id, status, extra string) string {
	return fmt.Sprintf("event: %s\ndata: {\"type\":%q,\"sequence_number\":%d,\"response\":{\"id\":%q,\"object\":\"response\",\"created_at\":1700000000,\"status\":%q,\"model\":\"provider-model\",\"output\":[]%s}}\n\n", kind, kind, sequence, id, status, extra)
}

type responseStreamEvent struct {
	Type     string
	Response struct {
		ID, Model          string
		PreviousResponseID string `json:"previous_response_id"`
	}
}

func readResponseStream(t *testing.T, raw []byte) (kinds []string, events []responseStreamEvent) {
	t.Helper()
	for line := range strings.SplitSeq(string(raw), "\n") {
		data, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		var event responseStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("event %q: %v", data, err)
		}
		kinds, events = append(kinds, strings.TrimPrefix(event.Type, "response.")), append(events, event)
	}
	return kinds, events
}

func TestAStoredResponseStreamCarriesGatewayIdentifiersForwardsTheCursorSettlesTheStoredStateAndEndsWhereTheProviderBreaks(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model, stream, sse = "stored-streams", "responses-model", "?stream=true", "text/event-stream"
	sink := &terminals{}
	upstream := &responseStreams{retainedUpstream: &retainedUpstream{replies: func(request string, nth int) map[string]string {
		replies := openAIReplies(request, nth)
		if nth == 3 {
			replies["POST /responses"] = `{"id":"resp_` + providerOwned + `-3","object":"response","created_at":1700000000,"background":true,"status":"queued","model":"provider-model","output":[]}`
		}
		return replies
	}}}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", streams: true, operations: []string{"generation"}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	create := func(previous string) string {
		return g.call(t, "owner", "POST", "/v1/responses", "application/json", `{"model":"`+slug+`","input":"hi",`+previous+`"store":true}`, 200).ID
	}
	first := create("")
	second := create(`"previous_response_id":"` + first + `",`)
	unfinished := create(`"background":true,`)
	provider := func(n int) string { return fmt.Sprintf("resp_%s-%d", providerOwned, n) }
	const usage = `,"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}`
	const delta = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"
	opened := responseEvent(0, "response.created", provider(1), "in_progress", "")
	completed := responseEvent(3, "response.completed", provider(1), "completed", usage)

	read := func(key, id, query string, status int) (http.Header, []byte, events.Envelope) {
		t.Helper()
		before := len(sink.all())
		header, raw := g.send(t, "GET", "/v1/responses/"+id+query, map[string]string{"Authorization": "Bearer " + key}, "", status)
		harness.Eventually(t, "the read's usage record", time.Second, func() (bool, string) { return len(sink.all()) > before, "" })
		recorded := sink.all()
		return header, raw, recorded[len(recorded)-1]
	}
	attempts := func(envelope events.Envelope) string {
		seen := make([]string, len(envelope.Attempts))
		for i, attempt := range envelope.Attempts {
			seen[i] = fmt.Sprintf("%s %s committed=%t", attempt.Mode, attempt.Class, attempt.Committed)
		}
		return strings.Join(seen, ", ")
	}

	for _, tc := range []struct {
		name, id, query string
		events          []string
		kinds, previous string
		asked           string
		outcome, class  string
		attempt         string
	}{
		{name: "a whole stream", id: first, query: stream, events: []string{opened, responseEvent(1, "response.in_progress", provider(1), "in_progress", ""), delta, completed},
			kinds: "created in_progress output_text.delta completed", asked: "api-version=&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a stream resumed after an event", id: first, query: stream + "&starting_after=1&include_obfuscation=false", events: []string{delta, completed},
			kinds: "output_text.delta completed", asked: "api-version=&include_obfuscation=false&starting_after=1&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a continuation", id: second, query: stream, events: []string{
			responseEvent(0, "response.created", provider(2), "in_progress", `,"previous_response_id":"`+provider(1)+`"`),
			responseEvent(1, "response.completed", provider(2), "completed", `,"previous_response_id":"`+provider(1)+`"`+usage),
		}, kinds: "created completed", previous: first, asked: "api-version=&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a response the provider cut short", id: first, query: stream, events: []string{opened, responseEvent(1, "response.incomplete", provider(1), "incomplete", `,"incomplete_details":{"reason":"max_output_tokens"}`)},
			kinds: "created incomplete", asked: "api-version=&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a response the provider failed", id: first, query: stream, events: []string{opened, responseEvent(1, "response.failed", provider(1), "failed", `,"error":{"code":"server_error","message":"refused listed-secret"}`)},
			kinds: "created failed", asked: "api-version=&stream=true", outcome: "failure", class: "upstream_response_failed", attempt: "streaming upstream_server committed=true"},
		{name: "a stream that turns to another response", id: first, query: stream, events: []string{opened, delta, responseEvent(3, "response.completed", provider(2), "completed", usage)},
			kinds: "created output_text.delta", asked: "api-version=&stream=true", outcome: "failure", class: "response_stream_incomplete", attempt: "streaming protocol committed=true"},
		{name: "a stream without its end", id: first, query: stream, events: []string{opened, delta},
			kinds: "created output_text.delta", asked: "api-version=&stream=true", outcome: "failure", class: "response_stream_incomplete", attempt: "streaming protocol committed=true"},
	} {
		upstream.answers(200, sse, tc.events...)
		header, raw, envelope := read("owner", tc.id, tc.query, 200)
		kinds, delivered := readResponseStream(t, raw)
		if header.Get("Content-Type") != sse || header.Get("Cache-Control") != "no-store" || strings.Join(kinds, " ") != tc.kinds || strings.Contains(string(raw), "listed-secret") {
			t.Errorf("%s reached the caller as %q with cache control %q carrying %q, want an uncached event stream of %q without the provider credential: %s",
				tc.name, header.Get("Content-Type"), header.Get("Cache-Control"), kinds, tc.kinds, raw)
		}
		for _, event := range delivered {
			if event.Response.ID != "" && (event.Response.ID != tc.id || event.Response.Model != slug || event.Response.PreviousResponseID != tc.previous) {
				t.Errorf("%s named response %q of model %q after %q, want %q of %q after %q", tc.name, event.Response.ID, event.Response.Model, event.Response.PreviousResponseID, tc.id, slug, tc.previous)
			}
		}
		if asked := upstream.asked(); len(asked) != 1 || asked[0] != tc.asked {
			t.Errorf("%s asked the provider %q, want one read with %q", tc.name, asked, tc.asked)
		}
		if got := attempts(envelope); envelope.Outcome != tc.outcome || envelope.ErrorClass != tc.class || !envelope.Committed || envelope.FirstByte == nil || got != tc.attempt {
			t.Errorf("%s was recorded as %s %q committed=%t first byte %v with attempts %q, want %s %q delivered with %q",
				tc.name, envelope.Outcome, envelope.ErrorClass, envelope.Committed, envelope.FirstByte, got, tc.outcome, tc.class, tc.attempt)
		}
	}

	if kept := storedResponses(t, pool, g, "owner"); len(kept) != 3 || kept[0].state != "completed" || kept[2].state != "queued" {
		t.Fatalf("the gateway kept %+v, want the first response completed and the third still queued", kept)
	}
	upstream.answers(200, sse, responseEvent(0, "response.created", provider(3), "in_progress", ""), responseEvent(1, "response.completed", provider(3), "completed", usage))
	read("owner", unfinished, stream, 200)
	if kept := storedResponses(t, pool, g, "owner"); kept[2].state != "completed" {
		t.Errorf("a stream that ended completed left the stored response %q, want completed", kept[2].state)
	}

	for _, tc := range []struct {
		name, key, query string
		provider         int
		kind             string
		events           []string
		status           int
		code, attempt    string
	}{
		{name: "a stream that opens on another response", key: "owner", query: stream, provider: 200, kind: sse, events: []string{responseEvent(0, "response.created", provider(2), "in_progress", "")},
			status: 502, code: "response_stream_incomplete", attempt: "streaming protocol committed=false"},
		{name: "an empty stream", key: "owner", query: stream, provider: 200, kind: sse, status: 502, code: "response_stream_incomplete", attempt: "streaming protocol committed=false"},
		{name: "a reply that is no stream", key: "owner", query: stream, provider: 200, kind: "application/json", events: []string{`{"id":"` + provider(1) + `"}`},
			status: 502, code: "fidelity_protocol_violation", attempt: "streaming protocol committed=false"},
		{name: "a provider outage", key: "owner", query: stream, provider: 500, kind: "application/json", events: []string{`{"error":{"message":"down"}}`}, status: 500, attempt: "streaming upstream_server committed=false"},
		{name: "another key", key: "stranger", query: stream, status: 404, code: "not_found"},
		{name: "a stream flag that is no boolean", key: "owner", query: "?stream=maybe", status: 400, code: "invalid_request"},
		{name: "a cursor without a stream", key: "owner", query: "?starting_after=1", status: 400, code: "invalid_request"},
		{name: "an unknown control", key: "owner", query: stream + "&verbose=1", status: 400, code: "invalid_request"},
		{name: "an include the gateway does not know", key: "owner", query: stream + "&include=everything", status: 400, code: "invalid_request"},
		{name: "a response the provider no longer has", key: "owner", query: stream, provider: 404, kind: "application/json", events: []string{`{"error":{"message":"gone"}}`},
			status: 404, code: "not_found", attempt: "streaming upstream_client committed=false"},
		{name: "a retry after the provider confirmed the response was missing", key: "owner", query: stream, status: 404, code: "not_found"},
	} {
		script := tc.events
		if tc.provider == 0 {
			script = []string{opened, completed}
		}
		upstream.answers(cmp.Or(tc.provider, 200), cmp.Or(tc.kind, sse), script...)
		_, raw, envelope := read(tc.key, first, tc.query, tc.status)
		var refusal struct{ Error struct{ Code string } }
		_ = json.Unmarshal(raw, &refusal)
		if got := attempts(envelope); refusal.Error.Code != tc.code || envelope.Committed || envelope.FirstByte != nil || got != tc.attempt {
			t.Errorf("%s answered %q recorded committed=%t with attempts %q, want %q undelivered with %q: %s", tc.name, refusal.Error.Code, envelope.Committed, got, tc.code, tc.attempt, raw)
		}
		if asked := upstream.asked(); (len(asked) != 0) != (tc.provider != 0) {
			t.Errorf("%s reached the provider as %q, want a provider read only when the request is one the owner may stream", tc.name, asked)
		}
	}

	native := &responseStreams{retainedUpstream: &retainedUpstream{replies: openAIReplies}}
	g = newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "strict-streams", model: model, kind: "azure_openai", profile: "azure-v1-responses", fidelity: routing.FidelityStrict, streams: true,
		operations: []string{"generation"}, upstream: native, sink: sink,
	})
	g.key(t, "owner")
	bound := g.call(t, "owner", "POST", "/v1/responses", "application/json", `{"model":"strict-streams","input":"hi","store":true}`, 200).ID
	for _, tc := range []struct {
		name, query, kinds, asked, outcome, class, attempt string
		events                                             []string
	}{
		{name: "a strict stream", query: stream + "&starting_after=1", events: []string{delta, completed}, kinds: "output_text.delta completed",
			asked: "starting_after=1&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a strict response the provider cut short", query: stream, events: []string{opened, responseEvent(1, "response.incomplete", provider(1), "incomplete", `,"incomplete_details":{"reason":"max_output_tokens"}`)},
			kinds: "created incomplete", asked: "stream=true", outcome: "failure", class: "response_incomplete", attempt: "streaming protocol committed=true"},
	} {
		native.answers(200, sse, tc.events...)
		_, raw, envelope := read("owner", bound, tc.query, 200)
		kinds, delivered := readResponseStream(t, raw)
		for _, event := range delivered {
			if event.Response.ID != "" && (event.Response.ID != bound || event.Response.Model != "strict-streams") {
				t.Errorf("%s named response %q of model %q, want %q of the route", tc.name, event.Response.ID, event.Response.Model, bound)
			}
		}
		if got, asked := attempts(envelope), native.asked(); strings.Join(kinds, " ") != tc.kinds || len(asked) != 1 || asked[0] != tc.asked || envelope.Outcome != tc.outcome || envelope.ErrorClass != tc.class || got != tc.attempt {
			t.Errorf("%s delivered %q after asking the provider %q and was recorded as %s %q with %q, want %q after %q recorded as %s %q with %q",
				tc.name, kinds, asked, envelope.Outcome, envelope.ErrorClass, got, tc.kinds, tc.asked, tc.outcome, tc.class, tc.attempt)
		}
	}
}

type contractProvider struct {
	mu     sync.Mutex
	bodies []string
}

func (p *contractProvider) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.bodies)
}

func anthropicEvents(events ...string) string {
	var out strings.Builder
	for i := 0; i < len(events); i += 2 {
		out.WriteString("event: " + events[i] + "\ndata: " + events[i+1] + "\n\n")
	}
	return out.String()
}

func (p *contractProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	text := string(raw)
	p.mu.Lock()
	p.bodies = append(p.bodies, text)
	p.mu.Unlock()
	answered := strings.Contains(text, "tool_result")
	opening := `{"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":"claude-model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`
	switch {
	case strings.Contains(text, "outage?"):
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"down"}}`)
	case strings.Contains(text, "cut?"):
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicEvents("message_start", opening,
			"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_cut","name":"lookup","input":{}}}`))
	case strings.Contains(text, `"stream":true`):
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicEvents("message_start", opening,
			"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_stream","name":"lookup","input":{}}}`,
			"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`,
			"content_block_stop", `{"type":"content_block_stop","index":0}`,
			"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`,
			"message_stop", `{"type":"message_stop"}`))
	case answered:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_2","type":"message","role":"assistant","model":"claude-model","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":2}}`)
	default:
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-model","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":3}}`)
	}
}

type contractReply struct {
	Model   string `json:"model"`
	Choices []struct {
		Message json.RawMessage `json:"message"`
	} `json:"choices"`
	OLP struct {
		Handle string `json:"handle"`
	} `json:"olp"`
	Error struct {
		Code  string `json:"code"`
		Param string `json:"param"`
	} `json:"error"`
}

func TestANegotiatedToolContinuationKeepsTheProvidersTurnReplaysItsDeliveryAndRefusesWhatItCannotContinue(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	provider := &contractProvider{}
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "tool-contract", model: "claude-model", kind: "anthropic", surface: "openai", profile: "anthropic-messages", fidelity: routing.FidelityStrict, streams: true,
		defaults: map[string]connectors.DefaultSet{"generation": {Dialect: "anthropic-messages", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage("64")}}},
		sink:     sink, operations: []string{"generation"}, upstream: provider,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	tools := `"tools":[{"type":"function","function":{"name":"lookup","description":"Looks up.","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}]`
	turn := func(question string, more ...string) string {
		return `{"model":"tool-contract","messages":[{"role":"user","content":"` + question + `"}` + strings.Join(more, "") + `],` + tools + `}`
	}
	submission := func() string { return resources.SubmissionID(time.Now(), uuid.New()) }
	ask := func(name, key, submission, handle, body string, status int, code string, attempts ...string) (contractReply, string, http.Header) {
		t.Helper()
		before, terminalsBefore := len(provider.asked()), len(sink.all())
		req, err := http.NewRequest(http.MethodPost, g.server.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-OLP-Continuation", "chat-anthropic-tools-v1")
		for header, value := range map[string]string{"X-OLP-Submission-ID": submission, "X-OLP-Continuation-Handle": handle} {
			if value != "" {
				req.Header.Set(header, value)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var reply contractReply
		_ = json.Unmarshal(raw, &reply)
		if resp.StatusCode != status || reply.Error.Code != code {
			t.Fatalf("%s: status %d, code %q, want %d %q: %s", name, resp.StatusCode, reply.Error.Code, status, code, raw)
		}
		harness.Eventually(t, name+" is recorded", time.Second, func() (bool, string) {
			return len(sink.all()) == terminalsBefore+1, ""
		})
		seen := []string{}
		for _, attempt := range sink.all()[terminalsBefore].Attempts {
			seen = append(seen, fmt.Sprintf("%s %s committed=%v", attempt.Mode, attempt.Class, attempt.Committed))
		}
		if !slices.Equal(seen, attempts) || len(provider.asked())-before != len(attempts) {
			t.Fatalf("%s: attempts %v with %d provider requests, want %v", name, seen, len(provider.asked())-before, attempts)
		}
		return reply, string(raw), resp.Header
	}
	replayed := func(name string, header http.Header, got, want string) {
		t.Helper()
		if got != want || header.Get("X-OLP-Delivery-Replay") != "true" || header.Get("X-Should-Retry") != "false" {
			t.Fatalf("%s: replay %q retry %q body %s, want the first delivery %s", name, header.Get("X-OLP-Delivery-Replay"), header.Get("X-Should-Retry"), got, want)
		}
	}

	first := submission()
	opening, openingBody, _ := ask("first turn", "owner", first, "", turn("weather?"), 200, "", "unary success committed=true")
	if opening.Model != "tool-contract" || !strings.HasPrefix(opening.OLP.Handle, "continuation_") || !strings.Contains(string(opening.Choices[0].Message), `"id":"toolu_1"`) {
		t.Fatalf("first turn: %s", openingBody)
	}
	if sent := provider.asked()[0]; !strings.Contains(sent, `"max_tokens":64`) || !strings.Contains(sent, `"model":"claude-model"`) || !strings.Contains(sent, `"input_schema"`) {
		t.Fatalf("first provider request: %s", sent)
	}
	_, again, header := ask("first turn again", "owner", first, "", turn("weather?"), 200, "")
	replayed("first turn again", header, again, openingBody)
	ask("submission reused for another request", "owner", first, "", turn("other?"), 409, "continuation_mismatch")

	result := `,` + string(opening.Choices[0].Message) + `,{"role":"tool","tool_call_id":"toolu_1","content":"sunny"}`
	second := submission()
	closing, closingBody, _ := ask("tool result", "owner", second, opening.OLP.Handle, turn("weather?", result), 200, "", "unary success committed=true")
	if !strings.Contains(string(closing.Choices[0].Message), `"content":"done"`) || closing.OLP.Handle == opening.OLP.Handle || !strings.HasPrefix(closing.OLP.Handle, "continuation_") {
		t.Fatalf("tool result turn: %s", closingBody)
	}
	if sent := provider.asked()[1]; !strings.Contains(sent, `"type":"tool_use"`) || !strings.Contains(sent, `"tool_use_id":"toolu_1"`) || !strings.Contains(sent, `"content":"sunny"`) {
		t.Fatalf("continued provider request: %s", sent)
	}
	_, again, header = ask("tool result again", "owner", second, opening.OLP.Handle, turn("weather?", result), 200, "")
	replayed("tool result again", header, again, closingBody)

	for _, tc := range []struct {
		name, key, submission, handle, body string
		status                              int
		code, param                         string
	}{
		{"another key's handle", "stranger", submission(), opening.OLP.Handle, turn("weather?", result), 409, "continuation_unavailable", ""},
		{"unknown handle", "owner", submission(), "continuation_" + strings.Repeat("0", 32), turn("weather?", result), 409, "continuation_unavailable", ""},
		{"no submission identity", "owner", "", "", turn("weather?"), 400, "state_carrier", ""},
		{"stale submission identity", "owner", resources.SubmissionID(time.Now().Add(-time.Hour), uuid.New()), "", turn("weather?"), 400, "invalid_submission_identity", ""},
		{"rewritten history", "owner", submission(), opening.OLP.Handle, turn("rain?", result), 400, "state_carrier", "/messages"},
	} {
		if reply, raw, _ := ask(tc.name, tc.key, tc.submission, tc.handle, tc.body, tc.status, tc.code); reply.Error.Param != tc.param {
			t.Fatalf("%s: %s", tc.name, raw)
		}
	}

	streamed := strings.Replace(turn("weather?"), `{"model"`, `{"stream":true,"model"`, 1)
	third := submission()
	_, frames, _ := ask("streamed turn", "owner", third, "", streamed, 200, "", "streaming success committed=true")
	if !strings.Contains(frames, `"id":"toolu_stream"`) || !strings.Contains(frames, `"handle":"continuation_`) || !strings.HasSuffix(frames, "data: [DONE]\n\n") || strings.Contains(frames, "msg_stream\"}}\n\nevent:") {
		t.Fatalf("streamed turn: %s", frames)
	}
	_, again, header = ask("streamed turn again", "owner", third, "", streamed, 200, "")
	replayed("streamed turn again", header, again, frames)

	lost := submission()
	if _, _, header := ask("provider outage", "owner", lost, "", turn("outage?"), 502, "ambiguous_upstream_result", "unary ambiguous committed=false"); header.Get("X-Should-Retry") != "false" {
		t.Fatalf("provider outage: retry hint %q", header.Get("X-Should-Retry"))
	}
	ask("provider outage again", "owner", lost, "", turn("outage?"), 409, "continuation_outcome_unknown")
	cut := strings.Replace(turn("cut?"), `{"model"`, `{"stream":true,"model"`, 1)
	broken := submission()
	ask("provider stream cut inside a tool call", "owner", broken, "", cut, 502, "provider_protocol_error", "streaming protocol committed=false")
	ask("cut stream again", "owner", broken, "", cut, 409, "continuation_outcome_unknown")
}

func videoObjects(issues string) map[string]string {
	replies := map[string]string{"POST /videos": providerVideo(issues, "queued")}
	for _, n := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"} {
		replies["GET /videos/video_"+providerOwned+n] = providerVideo(n, "completed")
		replies["GET /videos/video_"+providerOwned+n+"/content"] = "MP4DATA"
		replies["DELETE /videos/video_"+providerOwned+n] = `{"id":"video_` + providerOwned + n + `","object":"video.deleted","deleted":true}`
	}
	return replies
}

func providerVideo(n, status string) string {
	return `{"id":"video_` + providerOwned + n + `","object":"video","status":"` + status + `","model":"video-model","created_at":1,"seconds":"4","size":"720x1280","progress":0}`
}

type videoReply struct {
	ID, Status, Model string
	Deleted           bool
	HasMore           bool   `json:"has_more"`
	FirstID           string `json:"first_id"`
	LastID            string `json:"last_id"`
	Data              []struct{ ID, Status string }
	Error             struct{ Code string }
	raw               string
	contentType       string
}

func (r videoReply) listed() []string {
	ids := make([]string, len(r.Data))
	for i, item := range r.Data {
		ids[i] = item.ID
	}
	return ids
}

func videoForm(fields ...string) (string, string) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for i := 0; i+1 < len(fields); i += 2 {
		_ = form.WriteField(fields[i], fields[i+1])
	}
	_ = form.Close()
	return form.FormDataContentType(), body.String()
}

func TestVideoJobsHideProviderIdentifiersPageNewestFirstIsolateKeysAndKeepOrForgetWhatEachProviderAnswerDecides(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "studio", "video-model"
	provider := &storeProvider{issues: "1", objects: videoObjects, scripted: map[string]string{}, statuses: map[string]int{}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status, headers: func(r *http.Request) map[string]string {
		if strings.HasSuffix(r.URL.Path, "/content") {
			return map[string]string{"Content-Type": "video/mp4"}
		}
		return nil
	}}
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{slug: slug, model: model, kind: "openai_compatible", sink: sink,
		operations: []string{"video_create", "video_get", "video_content", "video_delete", "video_list"}, upstream: upstream})
	g.key(t, "owner")
	g.key(t, "stranger")
	providerVideoPath := func(n string) string { return "/videos/video_" + providerOwned + n }
	asked := func() int {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		return len(upstream.calls)
	}
	ask := func(name, key, method, path string, fields []string, status int, code string, attempts ...string) videoReply {
		t.Helper()
		settled := len(sink.all())
		contentType, body := "", ""
		if fields != nil {
			contentType, body = videoForm(fields...)
		}
		header, raw := g.send(t, method, path, map[string]string{"Authorization": "Bearer " + key, "Content-Type": contentType}, body, status)
		reply := videoReply{raw: string(raw), contentType: header.Get("Content-Type")}
		_ = json.Unmarshal(raw, &reply)
		if reply.Error.Code != code {
			t.Fatalf("%s: answered %s, want code %q", name, raw, code)
		}
		harness.Eventually(t, name+" is recorded", time.Second, func() (bool, string) { return len(sink.all()) > settled, "no terminal record" })
		seen := []string{}
		for _, attempt := range sink.all()[settled].Attempts {
			seen = append(seen, fmt.Sprintf("%s committed=%v uncertain=%v", attempt.Class, attempt.Committed, attempt.BillingUncertain))
		}
		if !slices.Equal(seen, attempts) {
			t.Fatalf("%s: provider attempts %v, want %v", name, seen, attempts)
		}
		return reply
	}
	unasked := func(name, key, method, path string, fields []string, status int, code string) videoReply {
		t.Helper()
		before := asked()
		reply := ask(name, key, method, path, fields, status, code)
		if asked() != before {
			t.Fatalf("%s: the provider was asked", name)
		}
		return reply
	}
	kept := func() []string {
		t.Helper()
		rows, err := pool.Query(t.Context(), `SELECT lifecycle_state||' '||state||' '||coalesce(upstream_job_id,'unnamed') FROM olp.media_jobs ORDER BY created_at, id`)
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	job := func(n, lifecycle, state string) string {
		return lifecycle + " " + state + " video_" + providerOwned + n
	}
	prompt := []string{"model", slug, "prompt", "a cat", "seconds", "4"}

	for _, refusal := range []struct {
		name, key, path string
		fields          []string
		status          int
		code            string
	}{
		{"a creation without a prompt", "owner", "/v1/videos", []string{"model", slug, "seconds", "4"}, 400, "invalid_request"},
		{"a creation of an unsupported length", "owner", "/v1/videos", []string{"model", slug, "prompt", "a cat", "seconds", "5"}, 400, "invalid_request"},
		{"a creation on an unknown route", "owner", "/v1/videos", []string{"model", "elsewhere", "prompt", "a cat"}, 404, "route_not_found"},
	} {
		unasked(refusal.name, refusal.key, http.MethodPost, refusal.path, refusal.fields, refusal.status, refusal.code)
	}
	if jobs := kept(); len(jobs) != 0 {
		t.Fatalf("refused creations kept %v", jobs)
	}

	first := ask("a creation", "owner", http.MethodPost, "/v1/videos", prompt, 201, "", "success committed=true uncertain=false")
	if _, err := uuid.Parse(first.ID); err != nil || first.Model != slug || first.Status != "queued" {
		t.Fatalf("created video = %s, want a gateway identifier under the route name", first.raw)
	}
	creation := upstream.sent("POST /videos")
	if len(creation) != 1 || creation[0].authorization != "Bearer listed-secret" || !strings.Contains(creation[0].body, model) || strings.Contains(creation[0].body, slug) || !strings.Contains(creation[0].body, "a cat") {
		t.Fatalf("provider creation = %+v, want the provider model and the prompt under the provider credential", creation)
	}

	for _, broken := range []struct {
		name, body string
		provider   int
		status     int
		code       string
		attempt    string
	}{
		{"a creation the provider refuses", `{"error":{"message":"no","type":"invalid_request_error"}}`, 400, 400, "upstream_rejected", "upstream_client committed=false uncertain=false"},
		{"a creation the provider fails", `{"error":{"message":"down"}}`, 500, 502, "ambiguous_upstream_result", "ambiguous committed=false uncertain=true"},
		{"a creation the provider answers without a job", `{"object":"video","status":"queued"}`, 200, 502, "provider_protocol_error", "success committed=false uncertain=true"},
		{"a creation the provider answers with a list", `[]`, 200, 502, "ambiguous_upstream_result", "ambiguous committed=false uncertain=true"},
	} {
		provider.answers("POST /videos", broken.provider, broken.body)
		ask(broken.name, "owner", http.MethodPost, "/v1/videos", prompt, broken.status, broken.code, broken.attempt)
	}
	provider.recovers("POST /videos")
	unsettled := []string{job("1", "active", "queued"), "deleted queued unnamed", "create_ambiguous queued unnamed", "create_ambiguous queued unnamed", "create_ambiguous queued unnamed"}
	if jobs := kept(); !slices.Equal(jobs, unsettled) {
		t.Fatalf("after one accepted, one refused and three uncertain creations the gateway keeps %v, want %v", jobs, unsettled)
	}

	provider.creates("2")
	second := ask("a second creation", "owner", http.MethodPost, "/v1/videos", prompt, 201, "", "success committed=true uncertain=false")
	provider.creates("3")
	third := ask("a third creation", "owner", http.MethodPost, "/v1/videos", prompt, 201, "", "success committed=true uncertain=false")

	refreshed := "success committed=true uncertain=false"
	listed := ask("a list of unfinished videos", "owner", http.MethodGet, "/v1/videos", nil, 200, "", refreshed, refreshed, refreshed)
	if want := []string{third.ID, second.ID, first.ID}; !slices.Equal(listed.listed(), want) || listed.HasMore || listed.Data[0].Status != "completed" {
		t.Fatalf("list = %s, want the accepted videos %v newest first, each asked of the provider and now completed", listed.raw, want)
	}
	for _, page := range []struct {
		name, query string
		want        []string
		more        bool
	}{
		{"the first page", "?limit=1", []string{third.ID}, true},
		{"the page after a cursor", "?limit=1&after=" + third.ID, []string{second.ID}, true},
		{"the oldest first", "?order=asc", []string{first.ID, second.ID, third.ID}, false},
	} {
		got := ask(page.name, "owner", http.MethodGet, "/v1/videos"+page.query, nil, 200, "")
		if !slices.Equal(got.listed(), page.want) || got.HasMore != page.more || got.FirstID != page.want[0] || got.LastID != page.want[len(page.want)-1] {
			t.Fatalf("%s = %s, want %v with more=%v", page.name, got.raw, page.want, page.more)
		}
	}
	for _, refusal := range []struct{ name, method, key, path, code string }{
		{"a list in an unknown order", http.MethodGet, "owner", "/v1/videos?order=up", "invalid_request"},
		{"a list of no videos", http.MethodGet, "owner", "/v1/videos?limit=0", "invalid_request"},
		{"a list after a cursor that is no identifier", http.MethodGet, "owner", "/v1/videos?after=nope", "invalid_request"},
		{"a list after a cursor that names no video", http.MethodGet, "owner", "/v1/videos?after=" + uuid.NewString(), "invalid_request"},
		{"a list with an unknown parameter", http.MethodGet, "owner", "/v1/videos?color=red", "invalid_request"},
		{"a content variant the gateway does not know", http.MethodGet, "owner", "/v1/videos/" + second.ID + "/content?variant=nope", "invalid_request"},
	} {
		unasked(refusal.name, refusal.key, refusal.method, refusal.path, nil, 400, refusal.code)
	}
	for _, hidden := range []struct{ name, method, key, path, code string }{
		{"another key's read", http.MethodGet, "stranger", "/v1/videos/" + first.ID, "video_not_found"},
		{"another key's download", http.MethodGet, "stranger", "/v1/videos/" + first.ID + "/content", "video_not_found"},
		{"another key's delete", http.MethodDelete, "stranger", "/v1/videos/" + first.ID, "video_not_found"},
		{"a read of no identifier", http.MethodGet, "owner", "/v1/videos/nope", "video_not_found"},
		{"a read of an unknown video", http.MethodGet, "owner", "/v1/videos/" + uuid.NewString(), "media_job_not_found"},
	} {
		unasked(hidden.name, hidden.key, hidden.method, hidden.path, nil, 404, hidden.code)
	}
	if strangers := unasked("another key's list", "stranger", http.MethodGet, "/v1/videos", nil, 200, ""); len(strangers.Data) != 0 {
		t.Fatalf("another key lists %s", strangers.raw)
	}

	read := ask("a read", "owner", http.MethodGet, "/v1/videos/"+first.ID, nil, 200, "", "success committed=true uncertain=false")
	if read.ID != first.ID || read.Model != slug || read.Status != "completed" {
		t.Fatalf("read = %s, want the completed video under its gateway identifier and route name", read.raw)
	}
	for _, broken := range []struct {
		name, body string
		provider   int
		status     int
		code       string
		attempt    string
	}{
		{"a read the provider fails", `{"error":{"message":"down"}}`, 500, 502, "upstream_unavailable", "upstream_server committed=false uncertain=true"},
		{"a read the provider cannot find", `{"error":{"message":"gone"}}`, 404, 404, "upstream_rejected", "upstream_client committed=false uncertain=false"},
		{"a read the provider answers with a list", `[]`, 200, 502, "provider_protocol_error", "protocol committed=false uncertain=true"},
	} {
		provider.answers("GET "+providerVideoPath("1"), broken.provider, broken.body)
		ask(broken.name, "owner", http.MethodGet, "/v1/videos/"+first.ID, nil, broken.status, broken.code, broken.attempt)
	}
	provider.recovers("GET " + providerVideoPath("1"))

	thumbnail := ask("a download", "owner", http.MethodGet, "/v1/videos/"+second.ID+"/content?variant=thumbnail", nil, 200, "", "success committed=true uncertain=false")
	downloads := upstream.sent("GET " + providerVideoPath("2") + "/content")
	if thumbnail.raw != "MP4DATA" || thumbnail.contentType != "video/mp4" || len(downloads) != 1 || downloads[0].query != "variant=thumbnail" {
		t.Fatalf("download = %q as %q through %+v, want the provider's bytes, type and the asked variant", thumbnail.raw, thumbnail.contentType, downloads)
	}
	provider.answers("GET "+providerVideoPath("2")+"/content", 404, `{"error":{"message":"gone"}}`)
	ask("a download the provider cannot find", "owner", http.MethodGet, "/v1/videos/"+second.ID+"/content", nil, 404, "upstream_rejected", "upstream_client committed=false uncertain=false")
	provider.answers("GET "+providerVideoPath("2")+"/content", 500, `{"error":{"message":"down"}}`)
	ask("a download the provider fails", "owner", http.MethodGet, "/v1/videos/"+second.ID+"/content", nil, 502, "upstream_unavailable", "upstream_server committed=false uncertain=true")

	provider.answers("DELETE "+providerVideoPath("2"), 500, `{"error":{"message":"down"}}`)
	ask("a delete the provider fails", "owner", http.MethodDelete, "/v1/videos/"+second.ID, nil, 502, "ambiguous_upstream_result", "ambiguous committed=false uncertain=true")
	provider.answers("DELETE "+providerVideoPath("2"), 200, `{"id":"video_`+providerOwned+`2","object":"video.deleted","deleted":false}`)
	ask("a delete the provider declines", "owner", http.MethodDelete, "/v1/videos/"+second.ID, nil, 502, "upstream_rejected", "success committed=true uncertain=false")
	if still := ask("a read after two failed deletes", "owner", http.MethodGet, "/v1/videos/"+second.ID, nil, 200, "", "success committed=true uncertain=false"); still.ID != second.ID {
		t.Fatalf("a video whose delete failed reads as %s", still.raw)
	}
	provider.answers("DELETE "+providerVideoPath("2"), 404, `{"error":{"message":"gone"}}`)
	gone := ask("a delete the provider has already forgotten", "owner", http.MethodDelete, "/v1/videos/"+second.ID, nil, 200, "", "upstream_client committed=false uncertain=false")
	again := unasked("a repeated delete", "owner", http.MethodDelete, "/v1/videos/"+second.ID, nil, 200, "")
	if !gone.Deleted || gone.ID != second.ID || !again.Deleted || again.ID != second.ID {
		t.Fatalf("deletes = %s then %s, want the gateway identifier deleted both times", gone.raw, again.raw)
	}
	unasked("a read of a deleted video", "owner", http.MethodGet, "/v1/videos/"+second.ID, nil, 404, "video_not_found")
	unasked("a download of a deleted video", "owner", http.MethodGet, "/v1/videos/"+second.ID+"/content", nil, 404, "video_not_found")
	if removed := ask("a delete", "owner", http.MethodDelete, "/v1/videos/"+third.ID, nil, 200, "", "success committed=true uncertain=false"); !removed.Deleted || len(upstream.sent("DELETE "+providerVideoPath("3"))) != 1 {
		t.Fatalf("delete = %s, want one provider delete", removed.raw)
	}
	if left := ask("a list after the deletes", "owner", http.MethodGet, "/v1/videos", nil, 200, ""); !slices.Equal(left.listed(), []string{first.ID}) {
		t.Fatalf("list after the deletes = %s, want only %s", left.raw, first.ID)
	}
	settled := append(slices.Clone(unsettled[1:]), job("1", "active", "succeeded"), job("2", "deleted", "succeeded"), job("3", "deleted", "succeeded"))
	slices.Sort(settled)
	jobs := kept()
	slices.Sort(jobs)
	if !slices.Equal(jobs, settled) {
		t.Fatalf("the gateway keeps %v, want %v", jobs, settled)
	}
}
