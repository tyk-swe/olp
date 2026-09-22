//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestEncryptedStoresOverlapWhileRotationStillFencesThem(t *testing.T) {
	h := newAccessHarness(t)
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	tx1, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(context.Background())
	if err := ring.Store(t.Context(), tx1, installation, ids[0], "provider_continuation", []byte("private-"+ids[0]), &expires); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	secondReady := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx, err := h.Pool.Begin(ctx)
		if err == nil {
			defer tx.Rollback(context.Background())
			close(secondReady)
			err = ring.Store(ctx, tx, installation, ids[1], "provider_continuation", []byte("private-"+ids[1]), &expires)
			if err == nil {
				err = tx.Commit(ctx)
			}
		}
		second <- err
	}()
	select {
	case <-secondReady:
	case err := <-second:
		t.Fatal(err)
	}
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		if err := tx1.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		t.Fatal("independent secret stores serialized behind an exclusive installation lock")
	}
	if err := tx1.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx3, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx3.Rollback(context.Background())
	if err := ring.Store(t.Context(), tx3, installation, ids[2], "provider_continuation", []byte("private-"+ids[2]), &expires); err != nil {
		t.Fatal(err)
	}
	rotated, err := secrets.ParseRing([]byte(`{"active_version":2,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"},{"version":2,"key":"` + strings.Repeat("ef", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	type rotation struct {
		count int
		err   error
	}
	rotationDone := make(chan rotation, 1)
	rotationStarted := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		close(rotationStarted)
		count, err := rotated.Rotate(ctx, h.Pool, installation)
		rotationDone <- rotation{count, err}
	}()
	<-rotationStarted
	select {
	case outcome := <-rotationDone:
		t.Fatalf("rotation passed an uncommitted secret store: %+v", outcome)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx3.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	outcome := <-rotationDone
	if outcome.err != nil || outcome.count < 3 {
		t.Fatalf("rotation after writers: %+v", outcome)
	}
	for _, id := range ids {
		var version int
		if err := h.Pool.QueryRow(t.Context(), `SELECT key_version FROM olp_go.secrets WHERE id=$1`, id).Scan(&version); err != nil || version != 2 {
			t.Fatalf("overlapping store retained an old key version: id=%s version=%d err=%v", id, version, err)
		}
		tx, err := h.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		plaintext, err := rotated.Read(t.Context(), tx, installation, id, "provider_continuation")
		tx.Rollback(context.Background())
		if err != nil || !bytes.Equal(plaintext, []byte("private-"+id)) {
			t.Fatalf("rotation lost an overlapping secret store: %v", err)
		}
	}
}
