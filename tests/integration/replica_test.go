//go:build integration

package integration_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestManagementReadReplicaLagConsumesTheRevocationDeadline(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "replica key", "scopes": []string{"inference", "models_read"}}, idem("replica-key"), 201)
	secret := key["secret"].(string)
	u, err := url.Parse(required(t, "OLP_TEST_DATABASE_READ_URL"))
	if err != nil {
		t.Fatal(err)
	}
	primary, _ := url.Parse(h.DBURL)
	u.Path = primary.Path
	readPool, err := pgxpool.New(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(readPool.Close)
	var recovery bool
	deadline := time.Now().Add(20 * time.Second)
	for {
		err = readPool.QueryRow(t.Context(), "SELECT pg_is_in_recovery()").Scan(&recovery)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("standby did not replay the new test database")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !recovery {
		t.Fatal("replica qualification requires a physical standby")
	}
	h.Runtime.ReadPool = readPool
	for {
		err = h.Runtime.Refresh(t.Context())
		if err == nil {
			if _, err = h.Runtime.Authenticate(secret); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("standby did not catch up: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	caller := sweepCaller{token: secret}
	if status, _, body := h.call(caller, "GET", "/v1/models", nil, nil); status != 200 {
		t.Fatalf("fresh replica gateway: %d %s", status, body)
	}
	if _, err = readPool.Exec(t.Context(), "SELECT pg_wal_replay_pause()"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		readPool.Exec(ctx, "SELECT pg_wal_replay_resume()")
	})
	deadline = time.Now().Add(5 * time.Second)
	for {
		var state string
		if err = readPool.QueryRow(t.Context(), "SELECT pg_get_wal_replay_pause_state()").Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "paused" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("standby did not pause replay")
		}
		time.Sleep(10 * time.Millisecond)
	}
	current := h.want(owner, "GET", "/api/v1/api-keys/"+key["id"].(string), nil, nil, 200)
	headers := etagHeader(current)
	headers["Idempotency-Key"] = "revoke-replica-key"
	h.want(owner, "POST", "/api/v1/api-keys/"+key["id"].(string)+"/revoke", nil, headers, 200)
	deadline = time.Now().Add(runtime.AuthorityStaleAfter + 10*time.Second)
	t.Log("physical replay paused after primary revocation; checking the 60-second authority deadline")
	for {
		h.Runtime.Refresh(t.Context())
		_, err = h.Runtime.Authenticate(secret)
		if errors.Is(err, runtime.ErrStaleAuthority) {
			break
		}
		if err != nil {
			t.Fatalf("paused replica observed an unreplayed revocation: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("successful reads kept stale replica authority alive")
		}
		time.Sleep(time.Second)
	}
	if !h.Runtime.Authority().Stale || h.Runtime.Eligibility("credential") != runtime.StaleAuthority {
		t.Fatal("lagged authority remained eligible")
	}
	if status, _, body := h.call(caller, "GET", "/v1/models", nil, nil); status != 503 {
		t.Fatalf("stale replica gateway: %d %s", status, body)
	}
	if _, err = readPool.Exec(t.Context(), "SELECT pg_wal_replay_resume()"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		err = h.Runtime.Refresh(t.Context())
		if err == nil && !h.Runtime.Authority().Stale {
			authority, authErr := h.Runtime.Authenticate(secret)
			if authErr == nil && authority.RevokedAt != nil {
				if authority.Allows("inference", "assistant", nil, time.Now()) {
					t.Fatal("revoked authority admits inference")
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed standby did not recover: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status, _, body := h.call(caller, "GET", "/v1/models", nil, nil); status != 401 {
		t.Fatalf("revoked key gateway after replay: %d %s", status, body)
	}
}
