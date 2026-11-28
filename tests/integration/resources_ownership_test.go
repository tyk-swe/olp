//go:build integration

package integration_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/resources"
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
