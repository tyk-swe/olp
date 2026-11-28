//go:build integration

package integration_test

import (
	"cmp"
	"context"
	"encoding/json"
	"encoding/pem"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	surfacemedia "github.com/tyk-swe/olp/internal/gateway/surface/media"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
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
	mediaDeps := surfacemedia.Deps{Transport: mediaTransport, Store: jobStore, Jobs: jobs.New(jobs.Deps{
		Store: jobStore, Transport: mediaTransport, Credentials: g.runtime, Gaps: &atomic.Uint64{}, Log: quiet, Now: time.Now,
	})}
	dependencies := kernel.Dependencies{
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
	}
	server := kernel.New(dependencies, kernel.Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxMediaBodyBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096})
	g.kernel = server
	g.mux = http.NewServeMux()
	gateway.Register(server, g.mux, dependencies, gateway.WithMedia(mediaDeps))
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

type storeProvider struct {
	mu       sync.Mutex
	issues   string
	objects  func(issues string) map[string]string
	scripted map[string]string
	statuses map[string]int
}

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

func (p *storeProvider) status(request string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statuses[request]
}
