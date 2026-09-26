//go:build integration

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

// mediaSystemEnv names a PostgreSQL admin connection the test creates and
// drops a scratch database on. The integration suite requires this service.
const mediaSystemEnv = "OLP_TEST_DATABASE_ADMIN_URL"

func systemPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv(mediaSystemEnv)
	if admin == "" {
		t.Fatalf("%s is required; run make integration", mediaSystemEnv)
	}
	cfg, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("olp_media_%s", strings.ReplaceAll(uuid.NewString()[:13], "-", ""))
	adminPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adminPool.Exec(t.Context(), "CREATE DATABASE "+dbName); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = dbName
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch database: %v", err)
		}
		adminPool.Close()
	})
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// videoUpstream is a scripted OpenAI-family media provider.
type videoUpstream struct {
	srv          *httptest.Server
	createCalls  atomic.Int64
	getCalls     atomic.Int64
	contentCalls atomic.Int64
	deleteCalls  atomic.Int64
	failCleanup  atomic.Bool
	delete404    atomic.Bool
	createStatus atomic.Int64 // non-zero: create responds with this status
	getStatus    atomic.Value // string
}

func newVideoUpstream(t *testing.T) *videoUpstream {
	u := &videoUpstream{}
	u.getStatus.Store("completed")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/videos", func(w http.ResponseWriter, r *http.Request) {
		ordinal := u.createCalls.Add(1)
		if code := u.createStatus.Load(); code != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(code))
			fmt.Fprintf(w, `{"error":{"message":"injected create failure","type":"server_error"}}`)
			return
		}
		id := "upstream-video-created"
		if ordinal > 1 {
			id = fmt.Sprintf("upstream-video-created-%d", ordinal)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"object":"video","status":"queued","model":"upstream-video-model","progress":0,"created_at":1800000000,"seconds":"8","size":"1280x720"}`, id)
	})
	mux.HandleFunc("GET /v1/videos/{id}", func(w http.ResponseWriter, r *http.Request) {
		u.getCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"object":"video","status":%q,"model":"upstream-video-model","progress":100,"created_at":1800000000,"completed_at":1800000060,"seconds":"8","size":"1280x720"}`,
			r.PathValue("id"), u.getStatus.Load().(string))
	})
	mux.HandleFunc("GET /v1/videos/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		u.contentCalls.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		io.WriteString(w, "video-content")
	})
	mux.HandleFunc("DELETE /v1/videos/{id}", func(w http.ResponseWriter, r *http.Request) {
		if u.delete404.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":{"message":"video not found","type":"invalid_request_error","code":"not_found"}}`)
			return
		}
		u.deleteCalls.Add(1)
		if u.failCleanup.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":{"message":"injected cleanup ambiguity","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"object":"video.deleted","deleted":true}`, r.PathValue("id"))
	})
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	return u
}

// mediaFixture is a seeded installation plus a live gateway/media service.
type mediaFixture struct {
	t            *testing.T
	pool         *pgxpool.Pool
	installation string
	auth         *secrets.AuthKey
	keys         *secrets.KeyRing
	ownerID      string
	apiKeyID     string
	bearer       string
	providerID   string
	revisionID   string
	slotID       string
	credentialID *string
	generationID string
	snapshot     *runtime.Snapshot
	spool        *media.Spool
	spoolDir     string
	service      *media.Service
	rt           *fakeRuntime
	upstream     *videoUpstream
	server       *httptest.Server
	gateway      *Server
	sink         *capture
	log          *slog.Logger
	completed    atomic.Int64 // HTTP handlers that returned after deferred settlement
}

const mediaVideoModel = "upstream-video-model"

func videoCapabilities() []runtime.Capability {
	out := []runtime.Capability{
		{Model: mediaVideoModel, Operation: media.OpVideoCreate, Surface: "openai", Mode: "async"},
		{Model: mediaVideoModel, Operation: media.OpVideoList, Surface: "openai", Mode: "unary"},
	}
	for _, op := range []string{media.OpVideoGet, media.OpVideoContent, media.OpVideoDelete} {
		out = append(out, runtime.Capability{Model: mediaVideoModel, Operation: op, Surface: "openai", Mode: "unary"})
	}
	return out
}

// seedMediaFixture provisions a fresh installation with one video-capable
// provider, one route covering the whole video lifecycle, and one inference
// key. authMode none means no credential; "api_key" pins a sealed credential.
func seedMediaFixture(t *testing.T, authMode string, withCredential bool) *mediaFixture {
	t.Helper()
	f := &mediaFixture{t: t, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f.pool = systemPool(t)
	f.upstream = newVideoUpstream(t)
	ctx := t.Context()

	var err error
	f.installation, err = database.Installation(ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	f.auth = secrets.NewAuthKey(bytes32(19), f.installation)
	f.keys, err = secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + hexKey(23) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	f.ownerID = uuid.NewString()
	exec := func(query string, args ...any) {
		if _, err := f.pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec("INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,$2,'Owner','owner',$3)",
		f.ownerID, "owner@example.test", uuid.NewString())
	f.apiKeyID = uuid.NewString()
	lookup := "mediaLookup" + uuid.NewString()[:8]
	f.bearer = "olp_" + lookup + "_" + secrets.Token()
	exec(`INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag)
		VALUES($1,$2,$3,'media test',$4,'{"scopes":["inference"],"allowed_routes":[]}', $5)`,
		f.apiKeyID, lookup, f.auth.Digest("api_key", f.bearer), f.ownerID, uuid.NewString())

	f.providerID = uuid.NewString()
	f.revisionID = uuid.NewString()
	f.slotID = uuid.NewString()
	endpoint := f.upstream.srv.URL + "/v1"
	configuration := map[string]any{
		"kind": "openai", "auth_mode": authMode, "endpoint": endpoint,
		"cloud_region": "", "cloud_project": "", "deployment": "", "api_version": "",
		"options": map[string]any{"models": map[string]any{}, "credential_headers": []string{},
			"parameter_defaults": map[string]any{}, "vendor_id": ""},
	}
	capabilities := []map[string]any{}
	for _, c := range videoCapabilities() {
		capabilities = append(capabilities, map[string]any{
			"operation": c.Operation, "surface": c.Surface, "mode": c.Mode, "source": "certified"})
	}
	models := []map[string]any{{
		"id": uuid.NewString(), "upstream_model": mediaVideoModel,
		"display_name": "Video model", "capabilities": capabilities,
	}}
	if withCredential {
		credential := uuid.NewString()
		f.credentialID = &credential
	}
	slot := map[string]any{
		"id": f.slotID, "name": "Default", "enabled": true, "priority": 0, "weight": 1,
		"credential_id": f.credentialID, "credential_version": nil, "default": true,
	}
	if withCredential {
		version := 1
		slot["credential_version"] = version
	}
	configJSON, _ := json.Marshal(configuration)
	modelsJSON, _ := json.Marshal(models)
	slotsJSON, _ := json.Marshal([]map[string]any{slot})
	var credentialVersion *int
	if withCredential {
		version := 1
		credentialVersion = &version
	}
	exec(`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,active_revision_id)
		VALUES($1,'media-provider','openai','active',$2,$3,$4,$5,$6)`,
		f.providerID, configJSON, uuid.NewString(), uuid.NewString(), f.ownerID, f.revisionID)
	exec(`INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by)
		VALUES($1,$2,1,'media-provider',$3,$4,$5,$6,$7,$8)`,
		f.revisionID, f.providerID, configJSON, modelsJSON, slotsJSON, credentialVersion, uuid.NewString(), f.ownerID)
	if withCredential {
		exec("INSERT INTO olp.provider_credentials(id,provider_id,version) VALUES($1,$2,1)",
			*f.credentialID, f.providerID)
	}

	// The sealed provider credential is historical media authority: it must
	// remain decryptable for job reconciliation even after the API key and the
	// credential row itself move on.
	if withCredential {
		exec("UPDATE olp.installation SET active_key_version = $1 WHERE singleton", f.keys.Active)
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.keys.Store(ctx, tx, f.installation, *f.credentialID, "provider_credential", []byte("upstream-secret-value"), nil); err != nil {
			tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	f.generationID = uuid.NewString()
	routeID := uuid.NewString()
	provider := runtime.Provider{
		ID: f.providerID, Name: "media-provider", Kind: "openai", Enabled: true,
		RevisionID: f.revisionID, Endpoint: endpoint, AuthMode: authMode,
		Capabilities: videoCapabilities(),
		Slots: []runtime.Slot{{
			ID: f.slotID, Name: "Default", Enabled: true, Weight: 1,
			CredentialID: f.credentialID,
		}},
	}
	if withCredential {
		version := 1
		provider.Slots[0].CredentialVersion = &version
		provider.ActiveCredential = f.credentialID
		provider.DefaultSlotID = f.slotID
	}
	f.snapshot = &runtime.Snapshot{
		Generation: runtime.Generation{ID: f.generationID, Ordinal: 1, ActivatedAt: time.Now()},
		Providers:  map[string]runtime.Provider{f.providerID: provider},
		Routes: map[string]runtime.Route{"video-default": {
			ID: routeID, Slug: "video-default", RevisionID: uuid.NewString(), Revision: 1, PublishedAt: time.Now(),
			Operations:     []string{media.OpVideoCreate, media.OpVideoList, media.OpVideoGet, media.OpVideoContent, media.OpVideoDelete},
			OverallTimeout: 8000, MaxAttempts: 1, RoutingID: routeID,
			Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityTransformed},
			Targets: []runtime.Target{{
				ID: uuid.NewString(), ProviderID: f.providerID, ProviderModel: mediaVideoModel,
				Priority: 0, Weight: 1, Timeout: 6000, RoutingID: uuid.NewString(),
			}},
		}},
	}
	// Job authorization reads the durable route even after it is retired
	// from the active snapshot.
	route := f.snapshot.Routes["video-default"]
	exec(`INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag)
		VALUES($1,$2,$3,$4,$5,$6)`,
		route.ID, route.Slug, f.ownerID, route.Revision, route.RevisionID, uuid.NewString())
	digest, err := f.snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO olp.runtime_releases(id,sequence,sha256,snapshot,created_by) VALUES($1,1,$2,$3,$4)",
		f.generationID, digest, encoded, f.ownerID)

	creds := map[string][]byte{}
	if withCredential {
		creds[*f.credentialID] = []byte("upstream-secret-value")
	}
	release, err := runtime.NewRelease(f.generationID, 1, f.snapshot, creds)
	if err != nil {
		t.Fatal(err)
	}
	f.rt = &fakeRuntime{
		release: release,
		revoked: map[string]bool{},
		keys: map[string]access.Authority{
			f.bearer: {ID: f.apiKeyID, Policy: access.KeyPolicy{Scopes: []string{"inference"}}},
		},
	}

	f.spoolDir = t.TempDir()
	f.spool, err = media.NewSpool(f.spoolDir, media.MinCapacityBytes, f.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.spool.Close() })
	policy := &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	f.service = &media.Service{
		Pool: f.pool, Keys: f.keys, Installation: f.installation,
		Transport: &media.Transport{
			Client:           policy.Client(5 * time.Minute),
			Auth:             connectors.NewAuth(policy),
			Egress:           policy,
			Spool:            f.spool,
			MaxResponseBytes: 16 << 20,
		},
		Revoked: f.rt.Revoked,
		Log:     f.log,
	}
	gw := New(f.rt, policy, Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096}, f.log)
	gw.Media = &MediaDeps{Jobs: f.service, Admission: media.NewAdmissionState(media.MinCapacityBytes)}
	f.gateway = gw
	f.sink = &capture{}
	gw.Sink = f.sink
	mux := http.NewServeMux()
	gw.Register(mux)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer f.completed.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func bytes32(seed byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed
	}
	return b
}

func hexKey(seed byte) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = digits[(seed+byte(i))%16]
	}
	return string(b)
}

func (f *mediaFixture) call(t *testing.T, method, path, contentType string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.bearer)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	return out
}

func (f *mediaFixture) lifecycle(t *testing.T, upstreamID string) string {
	t.Helper()
	var state string
	if err := f.pool.QueryRow(t.Context(),
		"SELECT lifecycle_state FROM olp.media_jobs WHERE upstream_job_id = $1", upstreamID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (f *mediaFixture) spoolFiles(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(f.spoolDir, "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	artifacts := matches[:0]
	for _, path := range matches {
		if filepath.Base(path) != ".owner.lock" {
			artifacts = append(artifacts, path)
		}
	}
	return artifacts
}

const videoCreateBody = "--video-boundary\r\n" +
	"Content-Disposition: form-data; name=\"model\"\r\n\r\n" +
	"video-default\r\n" +
	"--video-boundary\r\n" +
	"Content-Disposition: form-data; name=\"prompt\"\r\n\r\n" +
	"a test video\r\n" +
	"--video-boundary--\r\n"

const videoCreateContentType = "multipart/form-data; boundary=video-boundary"

// TestVideoJobLifecycleEndToEnd ports the Rust media-jobs system journey:
// durable create, client-facing refresh, content download, two-phase delete,
// idempotent re-delete, and session-authorized management views.
func TestVideoJobLifecycleEndToEnd(t *testing.T) {
	f := seedMediaFixture(t, "none", false)
	ctx := t.Context()

	resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	created := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d body %v", resp.StatusCode, created)
	}
	videoID, _ := created["id"].(string)
	if videoID == "" || videoID == "upstream-video-created" || created["model"] != "video-default" {
		t.Fatalf("create response must carry the local job id and route slug: %v", created)
	}
	if env := f.sink.last(t); env.Outcome != "success" || len(env.Attempts) != 1 ||
		env.Attempts[0].ProviderID != f.providerID || !env.Attempts[0].Committed {
		t.Fatalf("create accounting evidence %+v", env)
	}

	resp = f.call(t, http.MethodGet, "/v1/videos?limit=20&order=desc", "", nil)
	listed := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d body %v", resp.StatusCode, listed)
	}
	data, _ := listed["data"].([]any)
	var entry map[string]any
	for _, item := range data {
		if m, _ := item.(map[string]any); m["id"] == videoID {
			entry = m
		}
	}
	if entry == nil || entry["status"] != "completed" || entry["model"] != "video-default" {
		t.Fatalf("listed job should refresh to completed: %v", listed)
	}
	if _, prompt := entry["prompt"]; prompt {
		t.Fatal("list entries must stay metadata-only")
	}

	resp = f.call(t, http.MethodGet, "/v1/videos/"+videoID, "", nil)
	got := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK || got["status"] != "completed" {
		t.Fatalf("get: status %d body %v", resp.StatusCode, got)
	}

	resp = f.call(t, http.MethodGet, "/v1/videos/"+videoID+"/content", "", nil)
	content, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "video/mp4" || string(content) != "video-content" {
		t.Fatalf("content: status %d type %q body %q", resp.StatusCode, resp.Header.Get("Content-Type"), content)
	}

	// Two-phase delete: durable intent, upstream confirmation, tombstone.
	resp = f.call(t, http.MethodDelete, "/v1/videos/"+videoID, "", nil)
	deleted := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK || deleted["deleted"] != true {
		t.Fatalf("delete: status %d body %v", resp.StatusCode, deleted)
	}
	if got := f.upstream.deleteCalls.Load(); got != 1 {
		t.Fatalf("upstream delete calls %d", got)
	}
	var lifecycle string
	if err := f.pool.QueryRow(ctx, "SELECT lifecycle_state FROM olp.media_jobs WHERE id = $1", videoID).Scan(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "deleted" {
		t.Fatalf("lifecycle %q", lifecycle)
	}

	// The tombstone answers idempotently without a second upstream call.
	resp = f.call(t, http.MethodDelete, "/v1/videos/"+videoID, "", nil)
	again := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK || again["deleted"] != true {
		t.Fatalf("repeat delete: status %d body %v", resp.StatusCode, again)
	}
	if got := f.upstream.deleteCalls.Load(); got != 1 {
		t.Fatalf("repeat delete must not reissue upstream work: %d", got)
	}

	// A deleted job stops being readable through the other verbs.
	resp = f.call(t, http.MethodGet, "/v1/videos/"+videoID, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted job get: status %d", resp.StatusCode)
	}

	if files := f.spoolFiles(t); len(files) != 0 {
		t.Fatalf("spool leaked artifacts: %v", files)
	}

	// Revoking the API key isolates the client immediately while the durable
	// job history remains for reconciliation and management views.
	f.rt.mu.Lock()
	f.rt.keys = map[string]access.Authority{}
	f.rt.mu.Unlock()
	resp = f.call(t, http.MethodGet, "/v1/videos", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked key list: status %d", resp.StatusCode)
	}
	var count int
	if err := f.pool.QueryRow(ctx, "SELECT count(*) FROM olp.media_jobs WHERE id = $1", videoID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("durable media job history must survive API-key revocation")
	}
}

// TestMediaJobManagementSessionAuthorized checks the console's read surface:
// session-only, metadata-only, etag-carrying, and lifecycle-aware.
func TestMediaJobManagementSessionAuthorized(t *testing.T) {
	f := seedMediaFixture(t, "none", false)
	ctx := t.Context()

	control, err := access.New(ctx, f.pool, f.installation, "http://127.0.0.1", f.auth, f.keys, "test-bootstrap-token")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	control.Register(mux)
	(&media.Management{Access: control, Pool: f.pool}).Register(mux)
	management := httptest.NewServer(mux)
	t.Cleanup(management.Close)

	setupBody := `{"email":"admin@example.test","password":"correct horse battery staple","display_name":"Owner","installation_name":"Media test"}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, management.URL+"/api/v1/setup", strings.NewReader(setupBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1")
	req.Header.Set("X-OLP-Setup-Token", "test-bootstrap-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "__Host-olp_session" {
			cookie = c
		}
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || cookie == nil {
		t.Fatalf("setup: status %d cookie %v", resp.StatusCode, cookie)
	}

	// Reserve + attach a durable job the management surface can report.
	reserved, err := media.ReserveJob(ctx, f.pool, media.Reservation{
		ID: uuid.NewString(), RuntimeGenerationID: f.generationID, ProviderRevisionID: f.revisionID,
		APIKeyID: f.apiKeyID, ProviderID: f.providerID, UpstreamModel: mediaVideoModel,
		RouteSlug: "video-default", Operation: media.OpVideoCreate, Surface: "openai", SlotID: &f.slotID,
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := media.AttachUpstream(ctx, f.pool, reserved.ID, "upstream-video-http", media.JobUpdate{
		State: media.StateQueued, ContentAvailable: false, LastPolledAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	managementGet := func(path string) *http.Response {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, management.URL+path, nil)
		req.AddCookie(cookie)
		req.Header.Set("Origin", "http://127.0.0.1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp = managementGet("/api/v1/media-jobs?state=queued&limit=50")
	listed := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("management list: status %d body %v", resp.StatusCode, listed)
	}
	items, _ := listed["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("management list items %v", listed)
	}
	item, _ := items[0].(map[string]any)
	if item["id"] != job.ID || item["surface"] != "openai" || item["state"] != "queued" || item["lifecycle"] != "active" {
		t.Fatalf("management item %v", item)
	}
	if _, prompt := item["prompt"]; prompt {
		t.Fatal("management list must stay metadata-only")
	}
	if _, content := item["content"]; content {
		t.Fatal("management list must never embed content")
	}

	resp = managementGet("/api/v1/media-jobs/" + job.ID)
	etag := resp.Header.Get("ETag")
	detail := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK || etag == "" || detail["surface"] != "openai" {
		t.Fatalf("management detail: status %d etag %q body %v", resp.StatusCode, etag, detail)
	}

	// No session: the surface refuses authentication instead of listing.
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, management.URL+"/api/v1/media-jobs", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: status %d", resp.StatusCode)
	}
}

// TestVideoDeleteAmbiguityRetainsIntent ports the injected-finalize-failure
// path: the upstream delete lands, the tombstone write fails, the client sees
// a retryable 503 and the row stays delete-pending for a second attempt.
func TestVideoDeleteAmbiguityRetainsIntent(t *testing.T) {
	f := seedMediaFixture(t, "none", false)
	ctx := t.Context()

	resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	created := decodeJSON(t, resp)
	videoID, _ := created["id"].(string)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d body %v", resp.StatusCode, created)
	}

	exec := func(query string) {
		if _, err := f.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE FUNCTION olp.fail_test_media_finalize() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.lifecycle_state = 'deleted' THEN
				RAISE EXCEPTION 'injected finalization failure';
			END IF;
			RETURN NEW;
		END; $$`)
	exec(`CREATE TRIGGER fail_test_media_finalize BEFORE UPDATE ON olp.media_jobs
		FOR EACH ROW EXECUTE FUNCTION olp.fail_test_media_finalize()`)

	resp = f.call(t, http.MethodDelete, "/v1/videos/"+videoID, "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ambiguous delete: status %d", resp.StatusCode)
	}
	if got := f.upstream.deleteCalls.Load(); got != 1 {
		t.Fatalf("upstream delete calls %d", got)
	}
	var lifecycle string
	if err := f.pool.QueryRow(ctx, "SELECT lifecycle_state FROM olp.media_jobs WHERE id = $1", videoID).Scan(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "delete_pending" {
		t.Fatalf("the durable delete intent must survive a failed tombstone: %q", lifecycle)
	}

	exec("DROP TRIGGER fail_test_media_finalize ON olp.media_jobs")
	exec("DROP FUNCTION olp.fail_test_media_finalize()")

	// Upstream reports the object already gone; durable intent accepts it.
	f.upstream.delete404.Store(true)
	resp = f.call(t, http.MethodDelete, "/v1/videos/"+videoID, "", nil)
	deleted := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusOK || deleted["deleted"] != true {
		t.Fatalf("delete-missing-is-success: status %d body %v", resp.StatusCode, deleted)
	}
	if err := f.pool.QueryRow(ctx, "SELECT lifecycle_state FROM olp.media_jobs WHERE id = $1", videoID).Scan(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "deleted" {
		t.Fatalf("lifecycle %q", lifecycle)
	}
}

// TestVideoCreateAttachFailureCompensates ports the two-phase create
// failure: the attach write fails after the provider accepted the job, so the
// gateway persists cleanup intent, deletes the orphan upstream, and only then
// answers 503.
func TestVideoCreateAttachFailureCompensates(t *testing.T) {
	f := seedMediaFixture(t, "none", false)
	ctx := t.Context()

	exec := func(query string) {
		if _, err := f.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE FUNCTION olp.fail_test_media_attach() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.lifecycle_state = 'creating' AND NEW.lifecycle_state = 'active' THEN
				RAISE EXCEPTION 'injected attach failure';
			END IF;
			RETURN NEW;
		END; $$`)
	exec(`CREATE TRIGGER fail_test_media_attach BEFORE UPDATE ON olp.media_jobs
		FOR EACH ROW EXECUTE FUNCTION olp.fail_test_media_attach()`)

	resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("compensated create: status %d", resp.StatusCode)
	}
	if env := f.sink.last(t); env.Outcome != "failure" || env.ErrorClass == "" ||
		len(env.Attempts) != 1 || !env.Attempts[0].Committed {
		t.Fatalf("a committed upstream create must stay accounted on local failure: %+v", env)
	}
	if got := f.lifecycle(t, "upstream-video-created"); got != "deleted" {
		t.Fatalf("orphaned upstream job must be compensated to deleted: %q", got)
	}

	// When the compensation delete itself fails, the reservation survives as
	// create_cleanup_pending with its pinned authority intact for the worker.
	f.upstream.failCleanup.Store(true)
	resp = f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unresolved create: status %d", resp.StatusCode)
	}
	if got := f.lifecycle(t, "upstream-video-created-2"); got != "create_cleanup_pending" {
		t.Fatalf("unresolved create lifecycle %q", got)
	}
	var genID, revisionID *string
	if err := f.pool.QueryRow(ctx,
		"SELECT runtime_generation_id::text, provider_revision_id::text FROM olp.media_jobs WHERE upstream_job_id = 'upstream-video-created-2'",
	).Scan(&genID, &revisionID); err != nil {
		t.Fatal(err)
	}
	if genID == nil || *genID != f.generationID || revisionID == nil || *revisionID != f.revisionID {
		t.Fatalf("pinned authority lost: %v %v", genID, revisionID)
	}

	exec("DROP TRIGGER fail_test_media_attach ON olp.media_jobs")
	exec("DROP FUNCTION olp.fail_test_media_attach()")

	// The autonomous pass finishes the cleanup without the creating key.
	f.rt.mu.Lock()
	f.rt.keys = map[string]access.Authority{}
	f.rt.mu.Unlock()
	f.upstream.failCleanup.Store(false)
	pass, err := f.service.ReconcileOnce(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Claimed < 1 || pass.Completed < 1 {
		t.Fatalf("reconciliation pass %+v", pass)
	}
	if got := f.lifecycle(t, "upstream-video-created-2"); got != "deleted" {
		t.Fatalf("reconciled lifecycle %q", got)
	}
}

// TestMediaReconciliationRefreshesAndExpires covers the worker-side state
// machine: queued jobs refresh upstream, expired jobs are deleted.
func TestMediaReconciliationRefreshesAndExpires(t *testing.T) {
	f := seedMediaFixture(t, "none", false)
	ctx := t.Context()

	reserve := func() media.JobRecord {
		reserved, err := media.ReserveJob(ctx, f.pool, media.Reservation{
			ID: uuid.NewString(), RuntimeGenerationID: f.generationID, ProviderRevisionID: f.revisionID,
			APIKeyID: f.apiKeyID, ProviderID: f.providerID, UpstreamModel: mediaVideoModel,
			RouteSlug: "video-default", Operation: media.OpVideoCreate, Surface: "openai", SlotID: &f.slotID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return reserved
	}

	// A stale creating reservation with no upstream identity is ambiguous. The
	// worker only adopts creating rows that stopped changing, so age the row
	// past the staleness gate without firing the lifecycle guard.
	reserved := reserve()
	for _, query := range []string{
		"ALTER TABLE olp.media_jobs DISABLE TRIGGER ALL",
		"UPDATE olp.media_jobs SET updated_at = now() - interval '10 minutes' WHERE id = '" + reserved.ID + "'",
		"ALTER TABLE olp.media_jobs ENABLE TRIGGER ALL",
	} {
		if _, err := f.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	pass, err := f.service.ReconcileOnce(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Claimed < 1 {
		t.Fatalf("reconciliation pass %+v", pass)
	}
	var lifecycle string
	if err := f.pool.QueryRow(ctx, "SELECT lifecycle_state FROM olp.media_jobs WHERE id = $1", reserved.ID).Scan(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "create_ambiguous" {
		t.Fatalf("stale reservation lifecycle %q", lifecycle)
	}

	// An active queued job refreshes through the pinned historical target.
	active, err := media.AttachUpstream(ctx, f.pool, reserve().ID, "upstream-video-polled", media.JobUpdate{
		State: media.StateQueued, ContentAvailable: false, LastPolledAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	pass, err = f.service.ReconcileOnce(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Claimed < 1 {
		t.Fatalf("second pass %+v", pass)
	}
	var state string
	if err := f.pool.QueryRow(ctx, "SELECT state FROM olp.media_jobs WHERE id = $1", active.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" {
		t.Fatalf("refreshed job state %q", state)
	}

	// Worker restart: a fresh service instance adopts the durable delete
	// intent and finishes it without any in-process state from the first.
	if _, err := f.pool.Exec(ctx,
		`UPDATE olp.media_jobs SET lifecycle_state = 'delete_pending',
			next_reconciliation_at = now(), reconciliation_claim_id = NULL,
			reconciliation_claimed_until = NULL WHERE id = $1`, active.ID); err != nil {
		t.Fatal(err)
	}
	restarted := &media.Service{
		Pool: f.pool, Keys: f.keys, Installation: f.installation,
		Transport: f.service.Transport,
		Revoked:   f.rt.Revoked,
		Log:       f.log,
	}
	pass, err = restarted.ReconcileOnce(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Claimed < 1 {
		t.Fatalf("restarted worker pass %+v", pass)
	}
	if err := f.pool.QueryRow(ctx, "SELECT lifecycle_state FROM olp.media_jobs WHERE id = $1", active.ID).Scan(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle != "deleted" {
		t.Fatalf("restarted worker lifecycle %q", lifecycle)
	}
}

// TestMediaJobCredentialRevocation ports the revocation guard: historical
// credentials keep working while retained, and an explicit revocation always
// wins — the job records a durable class instead of touching upstream.
func TestMediaJobCredentialRevocation(t *testing.T) {
	f := seedMediaFixture(t, "api_key", true)
	ctx := t.Context()

	reserved, err := media.ReserveJob(ctx, f.pool, media.Reservation{
		ID: uuid.NewString(), RuntimeGenerationID: f.generationID, ProviderRevisionID: f.revisionID,
		APIKeyID: f.apiKeyID, ProviderID: f.providerID, UpstreamModel: mediaVideoModel,
		RouteSlug: "video-default", Operation: media.OpVideoCreate, Surface: "openai", SlotID: &f.slotID,
		CredentialVersionID: f.credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reserved.CredentialVersionID == nil || *reserved.CredentialVersionID != *f.credentialID {
		t.Fatalf("credential pin lost: %+v", reserved.CredentialVersionID)
	}
	job, err := media.AttachUpstream(ctx, f.pool, reserved.ID, "upstream-video-credential", media.JobUpdate{
		State: media.StateQueued, ContentAvailable: false, LastPolledAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The credential works while it is merely old: reconciliation refreshes.
	pass, err := f.service.ReconcileOnce(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Completed < 1 {
		t.Fatalf("live credential reconciliation %+v", pass)
	}

	// Explicit revocation is durable authority: it always wins over the
	// retained historical reference.
	if _, err := f.pool.Exec(ctx, "UPDATE olp.provider_credentials SET revoked_at = now() WHERE id = $1", *f.credentialID); err != nil {
		t.Fatal(err)
	}
	f.rt.mu.Lock()
	f.rt.revoked[*f.credentialID] = true
	f.rt.mu.Unlock()

	expired, err := media.ReserveJob(ctx, f.pool, media.Reservation{
		ID: uuid.NewString(), RuntimeGenerationID: f.generationID, ProviderRevisionID: f.revisionID,
		APIKeyID: f.apiKeyID, ProviderID: f.providerID, UpstreamModel: mediaVideoModel,
		RouteSlug: "video-default", Operation: media.OpVideoCreate, Surface: "openai", SlotID: &f.slotID,
		CredentialVersionID: f.credentialID,
	})
	if err != nil {
		t.Fatal(err)
	}
	revokedJob, err := media.AttachUpstream(ctx, f.pool, expired.ID, "upstream-video-revoked", media.JobUpdate{
		State: media.StateQueued, ContentAvailable: false, LastPolledAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		"UPDATE olp.media_jobs SET lifecycle_state = 'delete_pending' WHERE id = $1", revokedJob.ID); err != nil {
		t.Fatal(err)
	}
	before := f.upstream.deleteCalls.Load()
	pass, err = f.service.ReconcileOnce(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	var reconErr *string
	if err := f.pool.QueryRow(ctx, "SELECT reconciliation_error FROM olp.media_jobs WHERE id = $1", revokedJob.ID).Scan(&reconErr); err != nil {
		t.Fatal(err)
	}
	if reconErr == nil || *reconErr != "media_job_credential_revoked" {
		t.Fatalf("revocation must be recorded durably, got %v pass %+v", reconErr, pass)
	}
	if got := f.upstream.deleteCalls.Load() - before; got != 0 {
		t.Fatalf("a revoked credential must never reach upstream: %d delete calls", got)
	}
	_ = job
}

// TestVideoConcurrentCreates verifies reservation+attach stays correct under
// parallel load and every create binds a distinct upstream identity.
func TestVideoConcurrentCreates(t *testing.T) {
	f := seedMediaFixture(t, "none", false)

	const workers = 8
	ids := make([]string, workers)
	statuses := make([]int, workers)
	var wg atomic.Int64
	done := make(chan int, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			var out map[string]any
			for attempt := 0; attempt < 100; attempt++ {
				resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
				out = nil
				json.NewDecoder(resp.Body).Decode(&out)
				resp.Body.Close()
				statuses[i] = resp.StatusCode
				errBody, _ := out["error"].(map[string]any)
				if resp.StatusCode != http.StatusServiceUnavailable || errBody["code"] != "media_admission_overloaded" {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			ids[i], _ = out["id"].(string)
			wg.Add(1)
			done <- i
		}(i)
	}
	for i := 0; i < workers; i++ {
		<-done
	}
	seen := map[string]bool{}
	for i, id := range ids {
		if statuses[i] != http.StatusCreated || id == "" {
			t.Fatalf("worker %d: status %d id %q", i, statuses[i], id)
		}
		if seen[id] {
			t.Fatalf("duplicate local job id %q", id)
		}
		seen[id] = true
	}
	var count int
	if err := f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM olp.media_jobs WHERE lifecycle_state = 'active'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != workers {
		t.Fatalf("attached jobs %d", count)
	}
	if files := f.spoolFiles(t); len(files) != 0 {
		t.Fatalf("spool leaked artifacts: %v", files)
	}
}

// TestVideoMultipartBounds exercises the upload boundary over live HTTP:
// oversized bodies and excessive parts fail predictably and stage nothing.
func TestVideoMultipartBounds(t *testing.T) {
	f := seedMediaFixture(t, "none", false)

	// A video upload past the file bound is refused before touching upstream.
	big := strings.Repeat("x", int(media.DefaultVideoReferenceLimit)+1024)
	body := "--video-boundary\r\n" +
		"Content-Disposition: form-data; name=\"model\"\r\n\r\nvideo-default\r\n" +
		"--video-boundary\r\n" +
		"Content-Disposition: form-data; name=\"prompt\"\r\n\r\nshort\r\n" +
		"--video-boundary\r\n" +
		"Content-Disposition: form-data; name=\"input_reference\"; filename=\"big.mp4\"\r\n" +
		"Content-Type: video/mp4\r\n\r\n" + big + "\r\n" +
		"--video-boundary--\r\n"
	resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(body))
	resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("oversized upload was accepted")
	}
	if got := f.upstream.createCalls.Load(); got != 0 {
		t.Fatalf("oversized upload reached upstream: %d", got)
	}

	// Malformed bodies fail as request errors.
	resp = f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader("not-multipart"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed upload: status %d", resp.StatusCode)
	}
	if files := f.spoolFiles(t); len(files) != 0 {
		t.Fatalf("rejected uploads must stage nothing: %v", files)
	}
}
