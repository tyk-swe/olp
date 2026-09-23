//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestContinuationResourceCommitRecoveryAndBranches(t *testing.T) {
	h := newAccessHarness(t)
	f := newStrictProviderFixture(t, "anthropic-messages")
	owner := h.owner()
	slug, key := publishStrictProvider(t, h, owner, f, nil, nil, "strict")
	authority, err := h.Runtime.Authenticate(key)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	store := resources.NewEncrypted(h.Pool, installation, ring)
	route := h.Runtime.Release().Snapshot.Routes[slug]
	provider := h.Runtime.Release().Snapshot.Providers[f.providerID]
	version := "chat-anthropic-tools-v1"
	expires := time.Now().Add(time.Hour)
	submission := resources.SubmissionID(time.Now(), uuid.New())
	base := resources.Resource{Kind: resources.KindContinuation, APIKeyID: authority.ID, RouteSlug: slug, ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, RouteRevisionID: route.RevisionID, SlotID: provider.Slots[0].ID, CredentialID: provider.Slots[0].CredentialID, ContractVersion: &version, ExpiresAt: &expires, SubmissionID: &submission}
	initial := []byte(`{"history":["private prompt"],"seed":9007199254740993,"zero":-0,"small":1e-100}`)
	ready := []byte(`{"blocks":[{"type":"thinking","signature":"opaque-private-signature"},{"type":"tool_use","input":{"exact":0.1000000000000000000001}}],"delivery":"ready"}`)
	var mu sync.Mutex
	var claimed *resources.Resource
	creators := 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r, created, err := store.ClaimContinuation(t.Context(), &base, initial)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
				return
			}
			if created {
				creators++
				claimed = r
			}
		})
	}
	wg.Wait()
	if creators != 1 || claimed == nil {
		t.Fatalf("creators=%d", creators)
	}
	got, payload, err := store.ReadContract(t.Context(), resources.KindContinuation, authority.ID, claimed.ID)
	if err != nil || got.State != resources.StatePending || !bytes.Equal(payload, initial) {
		t.Fatalf("pending recovery: state=%v err=%v", got, err)
	}
	if err := store.CompleteContinuation(t.Context(), claimed, ready); !errors.Is(err, resources.ErrTransition) {
		t.Fatalf("completed without dispatch boundary: %v", err)
	}
	if err := store.StartDispatch(t.Context(), claimed); err != nil {
		t.Fatal(err)
	}
	// A failed key write must roll back both readiness and its ciphertext update.
	badRing, err := secrets.ParseRing([]byte(`{"active_version":2,"keys":[{"version":2,"key":"abababababababababababababababababababababababababababababababab"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	stale := resources.NewEncrypted(h.Pool, installation, badRing)
	if err := stale.CompleteContinuation(t.Context(), claimed, ready); err == nil {
		t.Fatal("stale key completed state")
	}
	got, payload, err = store.ReadContract(t.Context(), resources.KindContinuation, authority.ID, claimed.ID)
	if err != nil || got.State != resources.StateDispatching || !bytes.Equal(payload, initial) {
		t.Fatalf("nonatomic failed barrier: %v %v", got, err)
	}
	// An actual ciphertext-write failure must also roll back the ready state.
	// NOT VALID leaves the already committed journal readable while checking
	// this next write.
	if _, err := h.Pool.Exec(t.Context(), `ALTER TABLE olp_go.secrets ADD CONSTRAINT test_continuation_secret_write_failure CHECK (purpose <> 'provider_continuation') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteContinuation(t.Context(), claimed, ready); err == nil {
		t.Fatal("ciphertext-write failure published ready state")
	}
	got, payload, err = store.ReadContract(t.Context(), resources.KindContinuation, authority.ID, claimed.ID)
	if err != nil || got.State != resources.StateDispatching || !bytes.Equal(payload, initial) {
		t.Fatalf("ciphertext-write failure changed journal: state=%v err=%v", got, err)
	}
	if _, err := h.Pool.Exec(t.Context(), `ALTER TABLE olp_go.secrets DROP CONSTRAINT test_continuation_secret_write_failure`); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteContinuation(t.Context(), claimed, ready); err != nil {
		t.Fatal(err)
	}
	// A fresh owner object has no process-local state and recovers exact bytes.
	restarted := resources.NewEncrypted(h.Pool, installation, ring)
	got, payload, err = restarted.FindSubmission(t.Context(), authority.ID, submission)
	if err != nil || got.State != resources.StateReady || !bytes.Equal(payload, ready) {
		t.Fatalf("ready recovery: %v %v", got, err)
	}
	if err := store.CompleteContinuation(t.Context(), claimed, initial); !errors.Is(err, resources.ErrTransition) {
		t.Fatalf("mutated immutable ready state: %v", err)
	}
	var cipher, metadata []byte
	var purpose string
	if err := h.Pool.QueryRow(t.Context(), `SELECT s.ciphertext,s.purpose,r.metadata FROM olp_go.secrets s JOIN olp_go.provider_resources r ON r.id=s.id WHERE r.id=$1`, claimed.UUID).Scan(&cipher, &purpose, &metadata); err != nil {
		t.Fatal(err)
	}
	for _, private := range [][]byte{[]byte("opaque-private-signature"), []byte("private prompt"), []byte("0.1000000000000000000001")} {
		if bytes.Contains(cipher, private) || bytes.Contains(metadata, private) {
			t.Fatal("state leaked into stored cleartext")
		}
	}
	if purpose != "provider_continuation" {
		t.Fatal("wrong encrypted purpose")
	}
	// Both children retain a complete independent snapshot; branching does not
	// consume the parent, and cannot be made by a different owner.
	var children []*resources.Resource
	for i := range 2 {
		child := base
		id := resources.SubmissionID(time.Now(), uuid.New())
		child.SubmissionID = &id
		child.ParentID = &claimed.UUID
		r, created, err := store.ClaimContinuation(t.Context(), &child, []byte(fmt.Sprintf(`{"branch":%d}`, i)))
		if err != nil || !created || r.ParentID == nil || *r.ParentID != claimed.UUID {
			t.Fatalf("branch %d: %v", i, err)
		}
		children = append(children, r)
	}
	other := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "other owner", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)["id"].(string)
	if _, _, err := store.ReadContract(t.Context(), resources.KindContinuation, other, claimed.ID); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("cross owner read: %v", err)
	}
	child := base
	id := resources.SubmissionID(time.Now(), uuid.New())
	child.SubmissionID = &id
	child.ParentID = &claimed.UUID
	child.APIKeyID = other
	if _, _, err := store.ClaimContinuation(t.Context(), &child, initial); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("cross owner parent: %v", err)
	}
	// Rotation owns every encrypted purpose, including continuation payloads.
	rotatedRing, err := secrets.ParseRing([]byte(`{"active_version":2,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"},{"version":2,"key":"` + strings.Repeat("ef", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := rotatedRing.Rotate(t.Context(), h.Pool, installation); err != nil || n < 3 {
		t.Fatalf("rotation count=%d err=%v", n, err)
	}
	store = resources.NewEncrypted(h.Pool, installation, rotatedRing)
	_, payload, err = store.ReadContract(t.Context(), resources.KindContinuation, authority.ID, claimed.ID)
	if err != nil || !bytes.Equal(payload, ready) {
		t.Fatalf("rotation changed native state: %v", err)
	}
	if _, _, err := restarted.ReadContract(t.Context(), resources.KindContinuation, authority.ID, claimed.ID); !errors.Is(err, resources.ErrContract) {
		t.Fatalf("old key read rotated state: %v", err)
	}
	// Authenticated ciphertext rejects a change without logging/returning its data.
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.secrets SET ciphertext=set_byte(ciphertext,octet_length(ciphertext)-1,get_byte(ciphertext,octet_length(ciphertext)-1)#1) WHERE id=$1`, children[0].UUID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadContract(t.Context(), resources.KindContinuation, authority.ID, children[0].ID); !errors.Is(err, resources.ErrContract) {
		t.Fatalf("tamper: %v", err)
	}
	if err := store.Tombstone(t.Context(), children[0].ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.secrets WHERE id=$1`, children[0].UUID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("tombstone retained payload: %d %v", count, err)
	}
	oversized := base
	fresh := resources.SubmissionID(time.Now(), uuid.New())
	oversized.SubmissionID = &fresh
	if _, _, err := store.ClaimContinuation(t.Context(), &oversized, make([]byte, resources.MaxContinuationBytes+1)); !errors.Is(err, resources.ErrPayloadTooLarge) {
		t.Fatalf("unbounded payload: %v", err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.provider_resources SET expires_at=now()-interval '1 second' WHERE id=$1`, claimed.UUID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadContract(t.Context(), resources.KindContinuation, authority.ID, claimed.ID); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("synchronous expiry: %v", err)
	}
	if _, err := store.CleanupExpired(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.secrets WHERE id=$1`, claimed.UUID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expiry retained payload: %d %v", count, err)
	}
	if _, _, err := store.ReadContract(t.Context(), resources.KindContinuation, authority.ID, children[1].ID); err != nil {
		t.Fatalf("independent child lost with parent expiry: %v", err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.api_keys SET revoked_at=now() WHERE id=$1`, authority.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadContract(t.Context(), resources.KindContinuation, authority.ID, children[1].ID); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("live revocation ignored: %v", err)
	}
	if _, _, err := store.ClaimContinuation(t.Context(), &oversized, initial); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("revoked owner claim: %v", err)
	}

}

func TestContinuationClaimForDispatchCommitsOneEncryptedJournal(t *testing.T) {
	h := newAccessHarness(t)
	f := newStrictProviderFixture(t, "anthropic-messages")
	slug, key := publishStrictProvider(t, h, h.owner(), f, nil, nil, "strict")
	authority, err := h.Runtime.Authenticate(key)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	store := resources.NewEncrypted(h.Pool, installation, ring)
	release := h.Runtime.Release()
	route := release.Snapshot.Routes[slug]
	provider := release.Snapshot.Providers[f.providerID]
	version := "chat-anthropic-tools-v1"
	expires := time.Now().Add(time.Hour)
	submission := resources.SubmissionID(time.Now(), uuid.New())
	contract := &resources.Resource{Kind: resources.KindContinuation, APIKeyID: authority.ID, RouteSlug: slug, ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, RouteRevisionID: route.RevisionID, SlotID: provider.Slots[0].ID, CredentialID: provider.Slots[0].CredentialID, ContractVersion: &version, ExpiresAt: &expires, SubmissionID: &submission}
	initial := []byte(`{"private":"complete initial dependency"}`)
	ready := []byte(`{"private":"complete ready dependency"}`)
	var mu sync.Mutex
	var creator *resources.Resource
	created := 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			res, fresh, err := store.ClaimForDispatch(t.Context(), contract, initial)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Error(err)
				return
			}
			if fresh {
				created++
				creator = res
			}
		})
	}
	wg.Wait()
	if created != 1 || creator == nil {
		t.Fatalf("claim creators=%d", created)
	}
	got, payload, err := store.FindSubmission(t.Context(), authority.ID, submission)
	if err != nil || got.State != resources.StateDispatching || !bytes.Equal(payload, initial) {
		t.Fatalf("journal was not committed with encrypted claim: state=%v err=%v", got, err)
	}
	if err := store.StartDispatch(t.Context(), got); !errors.Is(err, resources.ErrTransition) {
		t.Fatalf("a second dispatch transition unexpectedly succeeded: %v", err)
	}
	if err := store.CompleteContinuation(t.Context(), creator, ready); err != nil {
		t.Fatal(err)
	}
	got, payload, err = store.ReadContract(t.Context(), resources.KindContinuation, authority.ID, creator.ID)
	if err != nil || got.State != resources.StateReady || !bytes.Equal(payload, ready) {
		t.Fatalf("ready dependency changed after journal: state=%v err=%v", got, err)
	}
	badRing, err := secrets.ParseRing([]byte(`{"active_version":2,"keys":[{"version":2,"key":"abababababababababababababababababababababababababababababababab"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	badStore := resources.NewEncrypted(h.Pool, installation, badRing)
	badSubmission := resources.SubmissionID(time.Now(), uuid.New())
	contract.SubmissionID = &badSubmission
	if _, _, err := badStore.ClaimForDispatch(t.Context(), contract, initial); err == nil {
		t.Fatal("claim with unavailable active key unexpectedly committed")
	}
	if _, _, err := store.FindSubmission(t.Context(), authority.ID, badSubmission); !errors.Is(err, resources.ErrNotFound) {
		t.Fatalf("failed encryption left an accepted-work journal: %v", err)
	}
	var initialSecrets int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.secrets WHERE purpose='provider_continuation'`).Scan(&initialSecrets); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `ALTER TABLE olp_go.secrets ADD CONSTRAINT test_continuation_secret_write_failure CHECK (purpose <> 'provider_continuation') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	failingSubmission := resources.SubmissionID(time.Now(), uuid.New())
	contract.SubmissionID = &failingSubmission
	if _, _, err := store.ClaimForDispatch(t.Context(), contract, initial); err == nil {
		t.Fatal("ciphertext-write failure committed a claim")
	}
	var claims, secretsAfter int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.provider_resources WHERE submission_id=$1`, failingSubmission).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("failed ciphertext write left a journal: count=%d err=%v", claims, err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.secrets WHERE purpose='provider_continuation'`).Scan(&secretsAfter); err != nil || secretsAfter != initialSecrets {
		t.Fatalf("failed ciphertext write left a secret: before=%d after=%d err=%v", initialSecrets, secretsAfter, err)
	}
	if _, err := h.Pool.Exec(t.Context(), `ALTER TABLE olp_go.secrets DROP CONSTRAINT test_continuation_secret_write_failure`); err != nil {
		t.Fatal(err)
	}
}

func TestContinuationClaimWaitsForKeyRotationAndLeavesNoJournalOnStaleRing(t *testing.T) {
	h := newAccessHarness(t)
	f := newStrictProviderFixture(t, "anthropic-messages")
	slug, key := publishStrictProvider(t, h, h.owner(), f, nil, nil, "strict")
	authority, err := h.Runtime.Authenticate(key)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	store := resources.NewEncrypted(h.Pool, installation, ring)
	release := h.Runtime.Release()
	route := release.Snapshot.Routes[slug]
	provider := release.Snapshot.Providers[f.providerID]
	version := "chat-anthropic-tools-v1"
	expires := time.Now().Add(time.Hour)
	submission := resources.SubmissionID(time.Now(), uuid.New())
	contract := &resources.Resource{Kind: resources.KindContinuation, APIKeyID: authority.ID, RouteSlug: slug, ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, RouteRevisionID: route.RevisionID, SlotID: provider.Slots[0].ID, CredentialID: provider.Slots[0].CredentialID, ContractVersion: &version, ExpiresAt: &expires, SubmissionID: &submission}
	var initialSecrets int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.secrets WHERE purpose=$1`, "provider_continuation").Scan(&initialSecrets); err != nil {
		t.Fatal(err)
	}

	rotation, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rotation.Rollback(context.Background())
	var active int
	if err := rotation.QueryRow(t.Context(), `SELECT active_key_version FROM olp_go.installation WHERE singleton FOR UPDATE`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		created bool
		err     error
	}
	finished := make(chan outcome, 1)
	started := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		close(started)
		_, created, err := store.ClaimForDispatch(ctx, contract, []byte(`{"private":"never dispatched"}`))
		finished <- outcome{created, err}
	}()
	<-started
	select {
	case result := <-finished:
		t.Fatalf("claim passed an in-progress key rotation: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := rotation.Exec(t.Context(), `UPDATE olp_go.installation SET active_key_version=$1 WHERE singleton`, active+1); err != nil {
		t.Fatal(err)
	}
	if err := rotation.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	result := <-finished
	if result.err == nil || result.created {
		t.Fatalf("stale key claimed accepted work: %+v", result)
	}
	var claims int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.provider_resources WHERE submission_id=$1`, submission).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("stale key left an accepted-work journal: count=%d err=%v", claims, err)
	}
	var secretsAfter int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.secrets WHERE purpose=$1`, "provider_continuation").Scan(&secretsAfter); err != nil || secretsAfter != initialSecrets {
		t.Fatalf("stale key left ciphertext: before=%d after=%d err=%v", initialSecrets, secretsAfter, err)
	}
}
