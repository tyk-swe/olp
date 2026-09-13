//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestConcurrentOwnerChangesKeepUsableAuthority(t *testing.T) {
	h := newAccessHarness(t)
	a := h.owner()
	b := h.invite(a, "second-owner@example.com", "owner")
	aUser := h.want(a, "GET", "/api/v3/profile", nil, nil, 200)
	bUser := h.want(b, "GET", "/api/v3/profile", nil, nil, 200)
	start := make(chan struct{})
	results := make(chan int, 2)
	go func() {
		<-start
		status, _, _ := h.request(a, "PATCH", "/api/v3/users/"+bUser["id"].(string), map[string]any{"role": "viewer"}, etagHeader(bUser))
		results <- status
	}()
	go func() {
		<-start
		status, _, _ := h.request(b, "PATCH", "/api/v3/users/"+aUser["id"].(string), map[string]any{"role": "viewer"}, etagHeader(aUser))
		results <- status
	}()
	close(start)
	first, second := <-results, <-results
	if !((first == 200 && second == 401) || (first == 401 && second == 200)) {
		t.Fatalf("owner race: %d, %d", first, second)
	}
	var owners, events int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.users WHERE active AND role='owner'").Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.audit WHERE action='user.update'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if owners != 1 || events != 1 {
		t.Fatalf("owners=%d committed audits=%d", owners, events)
	}
}

func TestInterruptedRotationResumesAndRejectsStaleWriters(t *testing.T) {
	h := newAccessHarness(t)
	b := h.owner()
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	var damaged string
	for i := 0; i < 101; i++ {
		damaged = uuid.Must(uuid.NewV7()).String()
		if err = h.Server.Keys.Store(t.Context(), tx, h.Server.Installation, damaged, "mutation_replay", []byte("private replay fixture"), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var original []byte
	if err = h.Pool.QueryRow(t.Context(), "UPDATE olp_go.secrets SET ciphertext=ciphertext WHERE id=$1 RETURNING ciphertext", damaged).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), "UPDATE olp_go.secrets SET ciphertext='\\x00' WHERE id=$1", damaged); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(`{"active_version":2,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"},{"version":2,"key":"` + strings.Repeat("ef", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	count, err := ring.Rotate(t.Context(), h.Pool, h.Server.Installation)
	if err == nil || count != 100 {
		t.Fatalf("expected interrupted rotation after one committed batch: count=%d", count)
	}
	h.want(b, "POST", "/api/v3/api-keys", map[string]any{"name": "stale writer"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 503)
	if _, err = h.Pool.Exec(t.Context(), "UPDATE olp_go.secrets SET ciphertext=$1 WHERE id=$2", original, damaged); err != nil {
		t.Fatal(err)
	}
	replacement, err := secrets.ParseRing([]byte(`{"active_version":2,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"},{"version":2,"key":"` + strings.Repeat("01", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if count, err = replacement.Rotate(t.Context(), h.Pool, h.Server.Installation); err == nil || count != 0 {
		t.Fatalf("replacement material must fail before committing: count=%d error=%v", count, err)
	}
	if count, err = ring.Rotate(t.Context(), h.Pool, h.Server.Installation); err != nil || count != 1 {
		t.Fatalf("resume count=%d error=%v", count, err)
	}
	h.Server.Keys = ring
	h.want(b, "GET", "/api/v3/sessions/current", nil, nil, 200)
	h.want(b, "POST", "/api/v3/api-keys", map[string]any{"name": "current writer"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
}

func TestFailedMigrationLeavesNoPartialInstallationAndRecovers(t *testing.T) {
	pool, _ := accessDatabase(t)
	lock, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(t.Context())
	if _, err = lock.Exec(t.Context(), "SELECT pg_advisory_xact_lock(726419823071)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err = database.Migrate(ctx, pool); err == nil {
		t.Fatal("migration unexpectedly passed a held lock")
	}
	if err = lock.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	var present bool
	if err = pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='olp_go')").Scan(&present); err != nil || present {
		t.Fatal("failed migration wrote schema")
	}
	if err = database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Installation(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
}
