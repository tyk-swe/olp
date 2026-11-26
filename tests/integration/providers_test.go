//go:build integration

package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

const providerEndpoint = `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:9/v1"}`

func TestProviderCredentialSlotsShowSharedQuotaFromActiveRevision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	keys, installation := notificationKeys(t, pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	configuration := func(assignment routing.SharedTokenQuota) []byte {
		cfg, err := connection.Decode([]byte(providerEndpoint))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Options.SharedTokenQuotas = []routing.SharedTokenQuota{assignment}
		return encoded(t, cfg)
	}
	active := routing.SharedTokenQuota{Model: "model-a", Group: "shared", TokensPerMinute: 1000}
	revision := uuid.NewString()
	exec(`INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,source_etag,activated_by)
		VALUES($1,$2,1,'quota-fixture',$3,'[]','[]',$1,$4)`, revision, f.providerID, configuration(active), f.ownerID)
	exec(`UPDATE olp.providers SET state='active',active_revision=1,active_revision_id=$2 WHERE id=$1`, f.providerID, revision)
	const token = "credential-slots-session"
	exec(`INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint) VALUES($1,$2,$3,now()+interval '1 hour','test')`,
		uuid.NewString(), f.ownerID, auth.Digest(secrets.SessionDigest, token))

	awayFromMinuteRollover()
	runner := newRunner(t)
	const used int64 = 700
	lookup := limits.SharedQuotaLookup(routing.SharedTokenQuotaGroupID(f.projectID, active.Group))
	if _, err := runner.ReserveRate(ctx, scripts.RateReservation{Lookup: lookup, LeaseID: uuid.NewString(), Tokens: used, TokensPerMinute: active.TokensPerMinute, TTL: time.Minute, Stated: true}); err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.DiscardHandler)
	api := access.New(access.Config{Pool: pool, Installation: installation, Auth: auth, Keys: keys, Log: quiet})
	mux := http.NewServeMux()
	providers.New(providers.Deps{Access: api, Egress: &egress.Policy{}, Getenv: func(string) string { return "" }, Quotas: fleet.New(runner), Log: quiet}).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	type usage struct {
		Used    int64 `json:"tokens_this_minute"`
		Ceiling int64 `json:"tokens_per_minute"`
	}
	for _, draftGroup := range []string{active.Group, "renamed"} {
		draft := routing.SharedTokenQuota{Model: active.Model, Group: draftGroup, TokensPerMinute: 2000}
		exec(`UPDATE olp.providers SET configuration=$2 WHERE id=$1`, f.providerID, configuration(draft))
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/providers/"+f.providerID+"/credential-slots", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.AddCookie(&http.Cookie{Name: access.SessionCookie, Value: token})
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Groups map[string]usage `json:"shared_group_usage"`
		}
		if response.StatusCode != 200 || json.Unmarshal(raw, &body) != nil {
			t.Fatalf("credential slots returned HTTP %d %s", response.StatusCode, raw)
		}
		if got := body.Groups[active.Group]; len(body.Groups) != 1 || got != (usage{Used: used, Ceiling: active.TokensPerMinute}) {
			t.Fatalf("credential slots with draft group %q returned %+v, want active group %q with %d used of %d", draftGroup, body.Groups, active.Group, used, active.TokensPerMinute)
		}
		t.Logf("GET credential-slots with draft group %q: HTTP %d shared_group_usage=%+v", draftGroup, response.StatusCode, body.Groups)
	}
}

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

type stubExternalSecrets struct {
	mu      sync.Mutex
	resolve func(context.Context, secretstore.Reference) ([]byte, error)
}

func (s *stubExternalSecrets) set(fn func(context.Context, secretstore.Reference) ([]byte, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolve = fn
}

func (s *stubExternalSecrets) Resolve(ctx context.Context, ref secretstore.Reference) ([]byte, error) {
	s.mu.Lock()
	fn := s.resolve
	s.mu.Unlock()
	return fn(ctx, ref)
}

type providerAPITestEnv struct {
	pool      *pgxpool.Pool
	auth      *secrets.AuthKey
	server    *providers.Server
	http      *httptest.Server
	ownerID   string
	sessionID string
	token     string
}

func newProviderAPITestEnv(t *testing.T, external *stubExternalSecrets) providerAPITestEnv {
	t.Helper()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	ownerID := uuid.NewString()
	sessionID := uuid.NewString()
	token := "provider-regression-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1::uuid,$1::text||'@example.com','Owner','owner',$1::uuid)`, ownerID); err != nil {
		t.Fatal(err)
	}
	keys, installation := notificationKeys(t, pool)
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	if err := access.PrepareInstallation(ctx, pool, installation, auth, keys); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.sessions(id,user_id,digest,expires_at,browser_hint) VALUES($1,$2,$3,now()+interval '1 hour','test')`,
		sessionID, ownerID, auth.Digest(secrets.SessionDigest, token)); err != nil {
		t.Fatal(err)
	}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	quiet := slog.New(slog.DiscardHandler)
	accessServer := access.New(access.Config{Pool: pool, Installation: installation, Origin: "https://test.example", Auth: auth, Keys: keys, Log: quiet})
	deps := providers.Deps{
		Access:    accessServer,
		Egress:    &policy,
		Getenv:    func(string) string { return "" },
		Activated: func(context.Context, pgx.Tx, string, string) error { return nil },
		Log:       quiet,
	}
	if external != nil {
		deps.ExternalSecrets = external
	}
	server := providers.New(deps)
	mux := http.NewServeMux()
	server.Register(mux)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	return providerAPITestEnv{pool: pool, auth: auth, server: server, http: httpServer, ownerID: ownerID, sessionID: sessionID, token: token}
}

func (e providerAPITestEnv) call(t *testing.T, method, path, body, ifMatch, idempotencyKey string) apiResult {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, e.http.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		r.Header.Set("If-Match", `"`+ifMatch+`"`)
	}
	if idempotencyKey != "" {
		r.Header.Set("Idempotency-Key", idempotencyKey)
	}
	r.Header.Set("Origin", "https://test.example")
	r.Header.Set("X-CSRF-Token", base64.RawURLEncoding.EncodeToString(e.auth.Digest(secrets.CSRFDigest, e.token)))
	r.AddCookie(&http.Cookie{Name: access.SessionCookie, Value: e.token})
	response, err := e.http.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return apiResult{status: response.StatusCode, body: raw}
}

func assertInstallationLockFree(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin lock check: %w", err)
	}
	defer tx.Rollback(ctx)
	var id string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.installation WHERE singleton FOR UPDATE NOWAIT").Scan(&id); err != nil {
		return fmt.Errorf("installation lock was held during external resolution: %w", err)
	}
	return nil
}

func TestProviderExternalSecretActivationRunsWithoutInstallationLock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	upstream := httptest.NewServer(vendor.Handler())
	t.Cleanup(upstream.Close)

	external := &stubExternalSecrets{}
	env := newProviderAPITestEnv(t, external)
	providerID, slotID, modelID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	initialETag := uuid.NewString()
	cfgJSON := `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":` + strconvQuote(upstream.URL+"/v1") + `}`
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'external-fixture','openai_compatible','draft',$2,$3,$3,$4)`,
		providerID, cfgJSON, initialETag, env.ownerID); err != nil {
		t.Fatal(err)
	}
	ref := secretstore.Reference{Store: "gcp", SecretID: "projects/test-project/secrets/provider-key", Version: "1"}
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	credID, _, err := env.server.StoreExternalCredential(ctx, tx, providerID, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits) VALUES($1,$2,true,0,'default',true,0,1,$3,'{}','{}')`,
		slotID, providerID, credID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities) VALUES($1,$2,$3,'Fixture Model',true,'[{"operation":"generation","surface":"openai","mode":"unary","source":"declared"}]')`,
		modelID, providerID, vendor.Model); err != nil {
		t.Fatal(err)
	}

	var probeCalls atomic.Int32
	external.set(func(resolveCtx context.Context, got secretstore.Reference) ([]byte, error) {
		probeCalls.Add(1)
		if got != ref {
			return nil, fmt.Errorf("unexpected reference %+v", got)
		}
		if err := assertInstallationLockFree(resolveCtx, env.pool); err != nil {
			return nil, err
		}
		return []byte(vendor.Secret), nil
	})
	probeRes := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/probe", "", initialETag, "")
	if probeRes.status != http.StatusOK {
		t.Fatalf("probe HTTP %d: %s", probeRes.status, probeRes.body)
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe external resolve calls = %d, want 1", probeCalls.Load())
	}

	certRes := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/models/"+modelID+"/certify", "", initialETag, "")
	if certRes.status != http.StatusOK {
		t.Fatalf("certify HTTP %d: %s", certRes.status, certRes.body)
	}
	var certified struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(certRes.body, &certified); err != nil || certified.Status != "certified" {
		t.Fatalf("certify body = %s, want status certified", certRes.body)
	}
	currentETag := func() string {
		t.Helper()
		var etag string
		if err := env.pool.QueryRow(ctx, `SELECT etag::text FROM olp.providers WHERE id=$1`, providerID).Scan(&etag); err != nil {
			t.Fatal(err)
		}
		return etag
	}

	etagBeforeConflict := currentETag()
	external.set(func(resolveCtx context.Context, got secretstore.Reference) ([]byte, error) {
		if err := assertInstallationLockFree(resolveCtx, env.pool); err != nil {
			return nil, err
		}
		if _, err := env.pool.Exec(resolveCtx, `UPDATE olp.providers SET etag=$2 WHERE id=$1`, providerID, uuid.NewString()); err != nil {
			return nil, err
		}
		return []byte(vendor.Secret), nil
	})
	conflictRes := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/activate", "", etagBeforeConflict, uuid.NewString())
	if conflictRes.status != http.StatusPreconditionFailed || !strings.Contains(string(conflictRes.body), "/etag_mismatch") {
		t.Fatalf("concurrent edit during activate returned HTTP %d: %s, want 412 etag_mismatch", conflictRes.status, conflictRes.body)
	}
	var state string
	if err := env.pool.QueryRow(ctx, `SELECT state FROM olp.providers WHERE id=$1`, providerID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "draft" {
		t.Fatalf("provider state after conflicted activate = %q, want draft", state)
	}

	var activateCalls atomic.Int32
	external.set(func(resolveCtx context.Context, got secretstore.Reference) ([]byte, error) {
		activateCalls.Add(1)
		if err := assertInstallationLockFree(resolveCtx, env.pool); err != nil {
			return nil, err
		}
		return []byte(vendor.Secret), nil
	})
	activateKey := uuid.NewString()
	etagBeforeActivate := currentETag()
	first := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/activate", "", etagBeforeActivate, activateKey)
	if first.status != http.StatusOK {
		t.Fatalf("activate HTTP %d: %s", first.status, first.body)
	}
	if activateCalls.Load() != 1 {
		t.Fatalf("activate external resolve calls = %d, want 1", activateCalls.Load())
	}
	var activated struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(first.body, &activated); err != nil || activated.State != "active" {
		t.Fatalf("activate body = %s, want active", first.body)
	}
	replay := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/activate", "", etagBeforeActivate, activateKey)
	if replay.status != http.StatusOK || string(replay.body) != string(first.body) {
		t.Fatalf("activate replay HTTP %d %s, want 200 %s", replay.status, replay.body, first.body)
	}
	if activateCalls.Load() != 1 {
		t.Fatalf("activate replay invoked external Resolve again: calls = %d, want 1", activateCalls.Load())
	}
}

func TestProviderExpiredEnrollmentPollSettlesAtomicallyAndRejectsRevokedSession(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	env := newProviderAPITestEnv(t, nil)
	providerID, slotID := uuid.NewString(), uuid.NewString()
	etag := uuid.NewString()
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'enrollment-fixture','openai_compatible','draft',$2,$3,$3,$4)`,
		providerID, providerEndpoint, etag, env.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,restrictions,limits) VALUES($1,$2,true,0,'default',true,0,1,'{}','{}')`,
		slotID, providerID); err != nil {
		t.Fatal(err)
	}
	insertExpired := func() string {
		t.Helper()
		id := uuid.NewString()
		if _, err := env.pool.Exec(ctx, `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext,expires_at) VALUES($1,'grant_enrollment',1,'\x00',now()+interval '1 hour')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := env.pool.Exec(ctx, `INSERT INTO olp.grant_enrollments(id,provider_id,slot_id,plugin_digest,profile_id,started_by,expires_at,poll_interval,poll_at)
			VALUES($1,$2,$3,repeat('0',64),'fixture-profile',$4,now()-interval '1 minute',5,now()-interval '1 minute')`, id, providerID, slotID, env.ownerID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	expiredID := insertExpired()
	untouchedID := insertExpired()

	res := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/grant-enrollments/"+expiredID+"/poll", "", "", "")
	if res.status != http.StatusOK {
		t.Fatalf("expired poll HTTP %d: %s", res.status, res.body)
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(res.body, &status); err != nil || status.Status != "expired" {
		t.Fatalf("expired poll body = %s, want status expired", res.body)
	}
	var outcome *string
	var continued bool
	var secretCount, auditCount int
	if err := env.pool.QueryRow(ctx, `SELECT outcome,continued_at IS NOT NULL,(SELECT count(*) FROM olp.secrets WHERE id=$1 AND purpose='grant_enrollment'),
		(SELECT count(*) FROM olp.audit WHERE action='provider.grant.enroll' AND resource_type='provider' AND resource_id=$2 AND outcome='failure')
		FROM olp.grant_enrollments WHERE id=$1`, expiredID, providerID).Scan(&outcome, &continued, &secretCount, &auditCount); err != nil {
		t.Fatal(err)
	}
	if outcome == nil || *outcome != "expired" || !continued || secretCount != 0 || auditCount != 1 {
		t.Fatalf("settled state outcome=%v continued=%v secrets=%d audits=%d, want expired/true/0/1", outcome, continued, secretCount, auditCount)
	}

	if _, err := env.pool.Exec(ctx, `DELETE FROM olp.sessions WHERE id=$1`, env.sessionID); err != nil {
		t.Fatal(err)
	}
	revokedRes := env.call(t, http.MethodPost, "/api/v1/providers/"+providerID+"/grant-enrollments/"+untouchedID+"/poll", "", "", "")
	if revokedRes.status != http.StatusUnauthorized {
		t.Fatalf("revoked session poll HTTP %d: %s, want 401", revokedRes.status, revokedRes.body)
	}
	if err := env.pool.QueryRow(ctx, `SELECT outcome,continued_at IS NOT NULL,(SELECT count(*) FROM olp.secrets WHERE id=$1 AND purpose='grant_enrollment'),
		(SELECT count(*) FROM olp.audit WHERE action='provider.grant.enroll' AND resource_type='provider' AND resource_id=$2 AND outcome='failure')
		FROM olp.grant_enrollments WHERE id=$1`, untouchedID, providerID).Scan(&outcome, &continued, &secretCount, &auditCount); err != nil {
		t.Fatal(err)
	}
	if outcome != nil || continued || secretCount != 1 || auditCount != 1 {
		t.Fatalf("revoked session mutated enrollment outcome=%v continued=%v secrets=%d audits=%d, want nil/false/1/1", outcome, continued, secretCount, auditCount)
	}
}

func TestProviderSlotWriteRejectsAmbiguousPastedCredentialAndVersion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	env := newProviderAPITestEnv(t, nil)
	providerID, slotID := uuid.NewString(), uuid.NewString()
	etag := uuid.NewString()
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'slot-write-fixture','openai_compatible','draft',$2,$3,$3,$4)`,
		providerID, providerEndpoint, etag, env.ownerID); err != nil {
		t.Fatal(err)
	}
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	credID, _, err := env.server.StoreCredential(ctx, tx, providerID, "sk-existing")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits) VALUES($1,$2,true,0,'default',true,0,1,$3,'{}','{}')`,
		slotID, providerID, credID); err != nil {
		t.Fatal(err)
	}

	payload := `{"slot":{"name":"default","credential_version_id":` + strconvQuote(credID) + `},"credential":"sk-pasted-conflict"}`
	res := env.call(t, http.MethodPut, "/api/v1/providers/"+providerID+"/credential-slots/"+slotID, payload, etag, uuid.NewString())
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("ambiguous slot write HTTP %d: %s, want 422", res.status, res.body)
	}
	var problem struct {
		Type   string           `json:"type"`
		Errors map[string][]any `json:"errors"`
	}
	if err := json.Unmarshal(res.body, &problem); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(problem.Type, "/validation_failed") || len(problem.Errors["credential"]) == 0 {
		t.Fatalf("ambiguous slot write problem = %s, want validation_failed on credential", res.body)
	}
	var credCount int
	if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM olp.provider_credentials WHERE provider_id=$1`, providerID).Scan(&credCount); err != nil {
		t.Fatal(err)
	}
	if credCount != 1 {
		t.Fatalf("provider credentials count = %d, want 1", credCount)
	}
}
