//go:build integration

package integration_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
)

const providerEndpoint = `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:9/v1"}`

type providerFixture struct {
	pool   *pgxpool.Pool
	server *providers.Server
	owner  string
}

type storedProvider struct {
	providerFixture
	id   string
	name string
	slot string
}

func TestProviderDraftConfigurationWritesKeepEvidenceOnlyWhileItStillApplies(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := providerFixture{pool: pool, owner: uuid.NewString(),
		server: providers.New(providers.Deps{Egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}, Getenv: func(string) string { return "" }})}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users (id,email,display_name,role,etag) VALUES ($1::uuid,$1::text || '@example.com','Provider fixture','owner',$1::uuid)`, f.owner); err != nil {
		t.Fatal(err)
	}
	t.Run("transport change", func(t *testing.T) {
		t.Parallel()
		p := f.certifiedDraft(t)
		pending := p.enroll(t, false)
		etag := p.etag(t)
		if err := p.store(t, p.name, `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:9/v1","options":{"parameter_defaults":{"temperature":0.2}}}`); err != nil {
			t.Fatal(err)
		}
		p.wantEvidence(t, "declared")
		if !p.enrolled(t, pending) {
			t.Fatal("a transport change inside the credential boundary cancelled a pending enrollment")
		}
		if p.etag(t) == etag {
			t.Fatal("changed configuration kept its etag")
		}
	})
	t.Run("credential boundary change", func(t *testing.T) {
		t.Parallel()
		p := f.certifiedDraft(t)
		pending, continued := p.enroll(t, false), p.enroll(t, true)
		if err := p.store(t, p.name, `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:10/v1"}`); err != nil {
			t.Fatal(err)
		}
		p.wantEvidence(t, "declared")
		if p.enrolled(t, pending) {
			t.Fatal("a pending enrollment or its secret survived a credential boundary change")
		}
		if !p.enrolled(t, continued) {
			t.Fatal("a credential boundary change discarded an enrollment that had already continued")
		}
	})
	t.Run("credential boundary change with a stored credential", func(t *testing.T) {
		t.Parallel()
		p := f.certifiedDraft(t)
		if _, err := p.pool.Exec(t.Context(), `INSERT INTO olp.provider_credentials(id,provider_id,version) VALUES($1,$2,1)`, uuid.NewString(), p.id); err != nil {
			t.Fatal(err)
		}
		err := p.store(t, p.name, `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:10/v1"}`)
		if problem := (*httpkit.Problem)(nil); !errors.As(err, &problem) || problem.Code != "credential_destination_changed" {
			t.Fatalf("moving a stored credential to another endpoint = %v, want credential_destination_changed", err)
		}
	})
	t.Run("restaged inventory", func(t *testing.T) {
		t.Parallel()
		p := f.certifiedDraft(t)
		extra := p
		extra.slot = uuid.NewString()
		p.stage(t, p.defaultSlot(), providers.DraftSlot{ID: extra.slot, Position: 1, Name: "extra", Enabled: true, Weight: 1})
		removed, kept := extra.enroll(t, false), p.enroll(t, false)
		p.stage(t, p.defaultSlot())
		if p.enrolled(t, removed) {
			t.Fatal("restaging without a slot kept its pending enrollment")
		}
		if !p.enrolled(t, kept) {
			t.Fatal("restaging without one slot cancelled another slot's pending enrollment")
		}
	})
	t.Run("unchanged configuration", func(t *testing.T) {
		t.Parallel()
		p := f.certifiedDraft(t)
		pending := p.enroll(t, false)
		etag := p.etag(t)
		if err := p.store(t, p.name, providerEndpoint); err != nil {
			t.Fatal(err)
		}
		if p.etag(t) != etag {
			t.Fatal("writing an unchanged configuration advanced the provider etag")
		}
		if err := p.store(t, p.name+"-renamed", providerEndpoint); err != nil {
			t.Fatal(err)
		}
		if p.etag(t) == etag {
			t.Fatal("renaming the provider kept its etag")
		}
		p.wantEvidence(t, "certified")
		if !p.enrolled(t, pending) {
			t.Fatal("renaming the provider cancelled a pending enrollment")
		}
	})
}

func (f providerFixture) certifiedDraft(t *testing.T) storedProvider {
	t.Helper()
	p := storedProvider{providerFixture: f, name: "provider-" + uuid.NewString(), slot: uuid.NewString()}
	if err := p.transact(t, func(tx pgx.Tx) error {
		prepared, err := connection.Prepare(t.Context(), f.server.ConnectionPolicy(tx), providerConfiguration(t, providerEndpoint))
		if err != nil {
			return err
		}
		created, err := f.server.CreateDraft(t.Context(), tx, providers.Draft{Name: p.name, Configuration: prepared, CreatedBy: f.owner})
		if err != nil {
			return err
		}
		p.id = created.ID
		if err := f.server.StageDraftInventory(t.Context(), tx, p.id, fixtureModels, []providers.DraftSlot{p.defaultSlot()}); err != nil {
			return err
		}
		_, err = tx.Exec(t.Context(), `UPDATE olp.provider_models SET capabilities=(SELECT jsonb_agg(c || jsonb_build_object('source','certified','certified_at',now(),'credential_fingerprint','fixture'))
			FROM jsonb_array_elements(capabilities) c) WHERE provider_id=$1`, p.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p.wantEvidence(t, "certified")
	return p
}

var fixtureModels = []providers.DraftModel{{UpstreamModel: "fixture-model", Enabled: true, Capabilities: []connection.Capability{
	{Operation: "generation", Surface: "openai", Mode: "unary"}, {Operation: "generation", Surface: "openai", Mode: "streaming"}}}}

func (p storedProvider) defaultSlot() providers.DraftSlot {
	return providers.DraftSlot{ID: p.slot, Default: true, Name: "default", Enabled: true, Weight: 1}
}

func (p storedProvider) stage(t *testing.T, slots ...providers.DraftSlot) {
	t.Helper()
	if err := p.transact(t, func(tx pgx.Tx) error {
		return p.server.StageDraftInventory(t.Context(), tx, p.id, fixtureModels, slots)
	}); err != nil {
		t.Fatal(err)
	}
}

func providerConfiguration(t *testing.T, raw string) connection.Configuration {
	t.Helper()
	var configuration connection.Configuration
	if err := json.Unmarshal([]byte(raw), &configuration); err != nil {
		t.Fatal(err)
	}
	return configuration
}

func (p storedProvider) transact(t *testing.T, fn func(pgx.Tx) error) error {
	t.Helper()
	tx, err := p.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}

func (p storedProvider) store(t *testing.T, name, configuration string) error {
	t.Helper()
	return p.transact(t, func(tx pgx.Tx) error {
		prepared, err := connection.Prepare(t.Context(), p.server.ConnectionPolicy(tx), providerConfiguration(t, configuration))
		if err != nil {
			return err
		}
		return p.server.StoreDraftConfiguration(t.Context(), tx, p.id, name, prepared)
	})
}

func (p storedProvider) enroll(t *testing.T, continued bool) string {
	t.Helper()
	id := uuid.NewString()
	if err := p.transact(t, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext,expires_at) VALUES($1,'grant_enrollment',1,'\x00',now()+interval '1 hour')`, id); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO olp.grant_enrollments(id,provider_id,slot_id,plugin_digest,profile_id,started_by,expires_at,continued_at)
			VALUES($1,$2,$3,repeat('0',64),'fixture-profile',$4,now()+interval '1 hour',CASE WHEN $5 THEN now() END)`, id, p.id, p.slot, p.owner, continued)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (p storedProvider) enrolled(t *testing.T, enrollment string) bool {
	t.Helper()
	var rows, secrets int
	if err := p.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM olp.grant_enrollments WHERE id=$1),(SELECT count(*) FROM olp.secrets WHERE id=$1 AND purpose='grant_enrollment')`, enrollment).Scan(&rows, &secrets); err != nil {
		t.Fatal(err)
	}
	if rows != secrets {
		t.Fatalf("enrollment %s has %d rows but %d secrets", enrollment, rows, secrets)
	}
	return rows == 1
}

func (p storedProvider) etag(t *testing.T) string {
	t.Helper()
	var etag string
	if err := p.pool.QueryRow(t.Context(), `SELECT etag::text FROM olp.providers WHERE id=$1`, p.id).Scan(&etag); err != nil {
		t.Fatal(err)
	}
	return etag
}

func (p storedProvider) wantEvidence(t *testing.T, source string) {
	t.Helper()
	var encoded []byte
	if err := p.pool.QueryRow(t.Context(), `SELECT capabilities FROM olp.provider_models WHERE provider_id=$1`, p.id).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var capabilities []struct {
		Mode        string  `json:"mode"`
		Source      string  `json:"source"`
		CertifiedAt *string `json:"certified_at"`
	}
	if err := json.Unmarshal(encoded, &capabilities); err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 2 {
		t.Fatalf("capabilities %s, want both declared modes kept", encoded)
	}
	for _, capability := range capabilities {
		if capability.Source != source || (capability.CertifiedAt != nil) != (source == "certified") {
			t.Fatalf("capabilities %s, want every one %s", encoded, source)
		}
	}
}

func TestProviderListingComparisonUsesThePublishedRevisionAndWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	var failFinalPage atomic.Bool
	var pageKeys []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageKeys = append(pageKeys, r.Header.Get("X-Api-Key"))
		if r.Header.Get("X-Api-Key") != "sk-published" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("after_id") {
		case "":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-kept","display_name":"Kept"},{"id":"model-renamed","display_name":"New name"}],"has_more":true,"last_id":"model-renamed"}`))
		case "model-renamed":
			if failFinalPage.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"model-new","display_name":"Brand New"}],"has_more":false,"last_id":"model-new"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	owner, operator := uuid.NewString(), uuid.NewString()
	readableProject, hiddenProject := uuid.NewString(), uuid.NewString()
	providerID, revisionID, slotID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1::uuid,$1::text||'@example.com','Owner','owner',$1::uuid)`, owner)
	exec(`INSERT INTO olp.users(id,email,display_name,role,etag,access_scope) VALUES($1::uuid,$1::text||'@example.com','Operator','operator',$1::uuid,'assigned')`, operator)
	exec(`INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1::uuid,'readable',$1::uuid,$2),($3::uuid,'hidden',$3::uuid,$2)`, readableProject, owner, hiddenProject)
	exec(`INSERT INTO olp.project_members(project_id,user_id,role,added_by) VALUES($1,$2,'manager',$3)`, readableProject, operator, owner)

	keys, installation := notificationKeys(t, pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	token := "listing-comparison-session"
	exec(`INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint) VALUES($1,$2,$3,now()+interval '1 hour','test')`, uuid.NewString(), operator, auth.Digest(secrets.SessionDigest, token))
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	accessServer := access.New(access.Config{Pool: pool, Installation: installation, Origin: "https://test.example", Auth: auth, Keys: keys, Log: slog.New(slog.DiscardHandler)})
	server := providers.New(providers.Deps{Access: accessServer, Egress: &policy, Getenv: func(string) string { return "" }, Log: slog.New(slog.DiscardHandler)})

	configuration := `{"kind":"anthropic","auth_mode":"api_key","endpoint":` + strconvQuote(upstream.URL) + `,"api_version":"2023-06-01"}`
	draftConfiguration := `{"kind":"anthropic","auth_mode":"api_key","endpoint":"http://127.0.0.1:9","api_version":"2020-01-01"}`
	providerETag := uuid.NewString()
	exec(`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id)
		VALUES($1,'listing-fixture','anthropic','active',$2,$3,$3,$4,$5)`, providerID, draftConfiguration, providerETag, owner, readableProject)
	storeCredential := func(secret string) (string, int) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		id, version, err := server.StoreCredential(ctx, tx, providerID, secret)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return id, version
	}
	publishedCredential, publishedVersion := storeCredential("sk-published")
	draftCredential, draftVersion := storeCredential("sk-draft")
	exec(`INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits)
		VALUES($1,$2,true,0,'default',true,0,1,$3,'{}','{}')`, slotID, providerID, draftCredential)

	provenance := func(credentialID string, version int) *routing.RevisionListing {
		return &routing.RevisionListing{Endpoint: upstream.URL, APIVersion: "2023-06-01", CredentialID: credentialID, CredentialVersion: version, ListedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
	modelIDs := map[string]string{}
	var revisionModels []routing.RevisionModel
	for _, model := range []struct {
		name, display string
		listing       *routing.RevisionListing
	}{
		{"model-kept", "Kept", provenance(publishedCredential, publishedVersion)},
		{"model-renamed", "Old name", provenance(publishedCredential, publishedVersion)},
		{"model-gone", "Gone", provenance(publishedCredential, publishedVersion)},
		{"model-manual", "Manual", nil},
		{"model-rotated", "Rotated", provenance(draftCredential, draftVersion)},
	} {
		modelIDs[model.name] = uuid.NewString()
		revisionModels = append(revisionModels, routing.RevisionModel{
			ID: modelIDs[model.name], UpstreamModel: model.name, DisplayName: model.display, Listing: model.listing,
			Capabilities: []routing.RevisionCapability{{Operation: "generation", Surface: "anthropic", Mode: "unary", Source: "certified"}},
		})
	}
	modelsJSON, err := json.Marshal(revisionModels)
	if err != nil {
		t.Fatal(err)
	}
	slotsJSON, err := json.Marshal([]routing.RevisionSlot{{Slot: routing.Slot{ID: slotID, Name: "default", Enabled: true, Weight: 1, CredentialID: &publishedCredential, CredentialVersion: &publishedVersion}, Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by)
		VALUES($1,$2,1,'listing-fixture',$3,$4,$5,$6,$7,$8)`, revisionID, providerID, configuration, modelsJSON, slotsJSON, publishedVersion, providerETag, owner)
	exec(`UPDATE olp.providers SET active_revision=1,active_revision_id=$2 WHERE id=$1`, providerID, revisionID)
	exec(`INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities)
		VALUES($1,$2,'model-manual','Manual',true,'[{"operation":"generation","surface":"anthropic","mode":"unary","source":"certified"}]')`, modelIDs["model-manual"], providerID)
	addRoute := func(slug, project string) {
		t.Helper()
		routeID, routeRevisionID := uuid.NewString(), uuid.NewString()
		targets := `[{"id":"` + uuid.NewString() + `","provider_model_id":"` + modelIDs["model-gone"] + `","provider_id":"` + providerID + `","provider_name":"listing-fixture","provider_model":"model-gone","priority":0,"weight":1,"timeout_ms":1000,"position":0}]`
		exec(`INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,state,etag,project_id) VALUES($1,$2,$3,1,$4,'active',$1,$5)`, routeID, slug, owner, routeRevisionID, project)
		exec(`INSERT INTO olp.route_revisions(id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,routing_policy,fidelity)
			VALUES($1,$2,1,$3,'[]',1000,1,$4,$5,$6,'{}','{"mode":"strict"}')`, routeRevisionID, routeID, slug, targets, uuid.NewString(), owner)
	}
	addRoute("readable-chat", readableProject)
	addRoute("hidden-chat", hiddenProject)

	mux := http.NewServeMux()
	server.Register(mux)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	compare := func() apiResult {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/api/v1/providers/"+providerID+"/listing-comparison", strings.NewReader(""))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", "https://test.example")
		r.Header.Set("X-CSRF-Token", base64.RawURLEncoding.EncodeToString(auth.Digest(secrets.CSRFDigest, token)))
		r.AddCookie(&http.Cookie{Name: access.SessionCookie, Value: token})
		response, err := httpServer.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return apiResult{status: response.StatusCode, body: body}
	}
	snapshot := func() string {
		t.Helper()
		var state string
		if err := pool.QueryRow(ctx, `SELECT p.etag::text||'|'||p.slots_etag::text||'|'||coalesce(p.last_probe_status,'')||'|'||p.draft_dirty::text||'|'||v.models::text||'|'||v.slots::text||'|'||(SELECT capabilities::text FROM olp.provider_models WHERE provider_id=p.id)
			FROM olp.providers p JOIN olp.provider_revisions v ON v.id=p.active_revision_id WHERE p.id=$1`, providerID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	before := snapshot()
	type comparison struct {
		Status string `json:"status"`
		Detail string `json:"detail"`
		New    []struct {
			UpstreamModel string `json:"upstream_model"`
		} `json:"new_models"`
		Changed []struct {
			UpstreamModel string `json:"upstream_model"`
			Listed        string `json:"listed_display_name"`
		} `json:"changed_display_names"`
		NotListed []struct {
			UpstreamModel string `json:"upstream_model"`
			Routes        []struct {
				Slug string `json:"slug"`
			} `json:"routes"`
		} `json:"not_listed"`
		Unverified []struct {
			UpstreamModel string `json:"upstream_model"`
		} `json:"unverified"`
	}
	result := compare()
	if result.status != http.StatusOK {
		t.Fatalf("listing comparison HTTP %d: %s", result.status, result.body)
	}
	t.Logf("POST /api/v1/providers/%s/listing-comparison answered %d %s", providerID, result.status, result.body)
	var complete comparison
	if err := json.Unmarshal(result.body, &complete); err != nil {
		t.Fatal(err)
	}
	if complete.Status != "complete" || len(complete.New) != 1 || complete.New[0].UpstreamModel != "model-new" ||
		len(complete.Changed) != 1 || complete.Changed[0].UpstreamModel != "model-renamed" || complete.Changed[0].Listed != "New name" ||
		len(complete.NotListed) != 1 || complete.NotListed[0].UpstreamModel != "model-gone" ||
		len(complete.NotListed[0].Routes) != 1 || complete.NotListed[0].Routes[0].Slug != "readable-chat" ||
		len(complete.Unverified) != 2 {
		t.Fatalf("comparison %s, want new, renamed, one verified omission on the readable route and two unverified omissions", result.body)
	}
	if len(pageKeys) != 2 || pageKeys[0] != "sk-published" || pageKeys[1] != "sk-published" {
		t.Fatalf("upstream page credentials %v, want the published credential on both pages", pageKeys)
	}
	failFinalPage.Store(true)
	result = compare()
	if result.status != http.StatusOK {
		t.Fatalf("incomplete listing comparison HTTP %d: %s", result.status, result.body)
	}
	t.Logf("withheld final page answered %d %s", result.status, result.body)
	var incomplete comparison
	if err := json.Unmarshal(result.body, &incomplete); err != nil {
		t.Fatal(err)
	}
	if incomplete.Status != "incomplete" || len(incomplete.NotListed) != 0 || len(incomplete.New) != 0 || len(incomplete.Unverified) != 0 {
		t.Fatalf("incomplete comparison %s, want no model verdicts", result.body)
	}
	if after := snapshot(); after != before {
		t.Fatal("listing comparison changed draft, published revision or certification state")
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
