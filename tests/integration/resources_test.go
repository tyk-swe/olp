//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/tests/harness"
)

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
