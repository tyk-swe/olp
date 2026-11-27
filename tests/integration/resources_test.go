//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/tests/harness"
)

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

func TestDetachGeminiResultAndCloseUndispatchedErrors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	store := resources.New(pool)

	missing := fixture.resource(resources.KindFile, fixture.keyID, "files/missing#output")
	missing.UUID = uuid.New()
	if detached, err := store.DetachGeminiResultView(ctx, missing, "files/missing"); detached || !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("DetachGeminiResultView on missing row = (%t, %v), want (false, ErrNotFound)", detached, err)
	}

	settlementResource, err := store.Put(ctx, fixture.resource("fine_tuning_job", fixture.keyID, "pending:undispatched"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO olp.retained_settlements
		(resource_id,operation,templates,token_consuming,poll_after) VALUES($1,'fine_tuning','{}'::jsonb,false,now()-interval '1 second')`,
		settlementResource.UUID); err != nil {
		t.Fatal(err)
	}
	claims, err := store.Settlements().Claim(ctx, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("Claim = (%+v, %v), want 1 claim", claims, err)
	}
	mismatched := claims[0]
	mismatched.ClaimID = uuid.New()
	if closed, err := store.Settlements().CloseUndispatched(ctx, mismatched); closed || !errors.Is(err, resources.ErrSubmissionConflict) {
		t.Fatalf("CloseUndispatched on mismatched claim = (%t, %v), want (false, ErrSubmissionConflict)", closed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE olp.retained_settlements SET lease_until=now()-interval '1 second' WHERE resource_id=$1`, settlementResource.UUID); err != nil {
		t.Fatal(err)
	}
	if closed, err := store.Settlements().CloseUndispatched(ctx, claims[0]); closed || !errors.Is(err, resources.ErrSubmissionConflict) {
		t.Fatalf("CloseUndispatched on expired claim = (%t, %v), want (false, ErrSubmissionConflict)", closed, err)
	}
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
