//go:build integration

package database

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
)

func TestIntegrationCommitmentLifecycle(t *testing.T) {
	pool := scratchPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	owner, provider, revision := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	scalar := func(q string, args ...any) string {
		t.Helper()
		var v string
		if err := pool.QueryRow(t.Context(), q, args...).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	exec(`INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,'commitments@example.test','Owner','owner',$1)`, owner)
	config := `{"kind":"openai","options":{"commitment":{"amount":"1","period":"hour"}}}`
	exec(`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'capacity','openai','draft',$2,$1,$1,$3)`, provider, config, owner)
	exec(`SELECT olp.publish_commitments('2026-10-01T10:15:00Z')`)
	if n := scalar(`SELECT count(*)::text FROM olp.commitment_schedules`); n != "0" {
		t.Fatalf("draft incurred %s obligations", n)
	}
	exec(`INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,source_etag,activated_by) VALUES($1,$2,1,'capacity',$3,'[]','[]',$1,$4)`, revision, provider, config, owner)
	exec(`UPDATE olp.providers SET state='active',active_revision=1,active_revision_id=$2 WHERE id=$1`, provider, revision)
	publish := func(at string) { exec(`SELECT olp.publish_commitments($1::timestamptz)`, at) }
	publish("2026-10-01T10:15:00Z")
	exec(`SELECT olp.materialize_commitments('2026-10-01T10:16:00Z','2026-10-01',512)`)
	if v := scalar(`SELECT amount::text||'/'||(bucket_start AT TIME ZONE 'UTC')::text FROM olp.commitment_periods`); v != "1.000000000000/2026-10-01 10:00:00" {
		t.Fatal(v)
	}
	exec(`UPDATE olp.providers SET state='disabled' WHERE id=$1`, provider)
	publish("2026-10-01T10:20:00Z")
	if n := scalar(`SELECT count(*)::text FROM olp.commitment_schedules`); n != "1" {
		t.Fatalf("republication/disable duplicated schedule: %s", n)
	}
	change := func(amount, period string) {
		exec(`UPDATE olp.provider_revisions SET configuration=$2::json WHERE id=$1`, revision, fmt.Sprintf(`{"kind":"openai","options":{"commitment":{"amount":"%s","period":"%s"}}}`, amount, period))
	}
	change("2", "hour")
	publish("2026-10-01T10:30:00Z")
	change("3", "hour")
	publish("2026-10-01T10:40:00Z")
	if v := scalar(`SELECT amount::text FROM olp.commitment_schedules WHERE effective_at='2026-10-01T11:00:00Z'`); v != "3.000000000000" {
		t.Fatal(v)
	}
	if n := scalar(`SELECT count(*)::text FROM olp.commitment_schedules`); n != "2" {
		t.Fatal(n)
	}
	// A cadence change replaces the pending amount edit. Hourly obligations
	// continue until the shared month boundary; no monthly overlap is possible.
	change("31", "month")
	publish("2026-10-01T10:45:00Z")
	if v := scalar(`SELECT effective_at::text FROM olp.commitment_schedules WHERE period='month'`); v != "2026-11-01 00:00:00+00" {
		t.Fatal(v)
	}
	// Reverting to the effective terms cancels all pending changes.
	change("1", "hour")
	publish("2026-10-01T10:46:00Z")
	if n := scalar(`SELECT count(*)::text FROM olp.commitment_schedules WHERE ends_at IS NULL`); n != "1" {
		t.Fatal(n)
	}
	exec(`UPDATE olp.provider_revisions SET configuration='{"kind":"openai","options":{}}' WHERE id=$1`, revision)
	publish("2026-10-01T10:50:00Z")
	if v := scalar(`SELECT ends_at::text FROM olp.commitment_schedules`); v != "2026-10-01 11:00:00+00" {
		t.Fatal(v)
	}
	// Materializers may race without duplicating a purchased period.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := pool.Exec(t.Context(), `SELECT olp.materialize_commitments('2026-10-02','2026-10-01',2)`)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := scalar(`SELECT count(*)::text FROM olp.commitment_periods`); n != "1" {
		t.Fatal(n)
	}
}

func TestIntegrationCommitmentExactAllocation(t *testing.T) {
	pool := scratchPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	for _, period := range []string{"hour", "day", "month"} {
		t.Run(period, func(t *testing.T) {
			schedule, pid := uuid.NewString(), uuid.NewString()
			_, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_schedules(id,provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at)
   VALUES($1,$2,$3,'old name','openai',1,'USD',$4,'2026-10-01');
   `, schedule, uuid.NewString(), uuid.NewString(), period)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(t.Context(), `INSERT INTO olp.commitment_periods(id,schedule_id,bucket_start,bucket_end,amount) VALUES($1,$2,'2026-10-01',olp.commitment_end($3,'2026-10-01'),1)`, pid, schedule, period); err != nil {
				t.Fatal(err)
			}
			for i := range 3 {
				if _, err = pool.Exec(t.Context(), `INSERT INTO olp.commitment_usage(attempt_id,period_id,observed_at,weight,eligible,complete,payload) VALUES($1,$2,$3,1,true,true,'{}')`, uuid.NewString(), pid, time.Date(2026, 10, 1, 0, i, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = pool.Exec(t.Context(), `INSERT INTO olp.commitment_usage(attempt_id,period_id,observed_at,weight,eligible,complete,payload) VALUES($1,$2,'2026-10-01',1000,false,true,'{}')`, uuid.NewString(), pid); err != nil {
				t.Fatal(err)
			}
			var sum string
			var count int
			if err = pool.QueryRow(t.Context(), `SELECT sum(allocated_cost)::text,count(*) FROM olp.commitment_allocations WHERE period_id=$1`, pid).Scan(&sum, &count); err != nil {
				t.Fatal(err)
			}
			if sum != "1.000000000000" || count != 3 {
				t.Fatalf("allocation=%s across %d", sum, count)
			}
		})
	}
}

func TestIntegrationCommitmentPublishedHistoryBackfill(t *testing.T) {
	pool := scratchPool(t)
	history := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0073" {
			continue
		}
		data, err := fs.ReadFile(migrations, "migrations/"+entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		history["migrations/"+entry.Name()] = &fstest.MapFile{Data: data}
	}
	if err := migrate(t.Context(), pool, history); err != nil {
		t.Fatal(err)
	}
	owner, provider, revision := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,'history@example.test','Owner','owner',$1)`, owner); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -5)
	for i := range 4 {
		providers := map[string]any{}
		if i < 3 {
			p := map[string]any{"id": provider, "revision_id": revision, "name": "Published capacity", "kind": "openai"}
			if i != 1 {
				p["commitment"] = map[string]any{"amount": "7", "period": "day"}
			}
			providers[provider] = p
		}
		doc, _ := json.Marshal(map[string]any{"providers": providers})
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.runtime_releases(id,sequence,sha256,snapshot,created_by,published_at) VALUES($1,$2,'historical',$3,$4,$5)`, uuid.NewString(), i+1, doc, owner, day.AddDate(0, 0, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `SELECT olp.materialize_commitments(now(),$1,512)`, day); err != nil {
		t.Fatal(err)
	}
	var schedules, periods, complete int
	if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM olp.commitment_schedules),(SELECT count(*) FROM olp.commitment_periods),(SELECT count(*) FROM olp.commitment_periods WHERE complete)`).Scan(&schedules, &periods, &complete); err != nil {
		t.Fatal(err)
	}
	if schedules != 2 || periods != 2 || complete != 0 {
		t.Fatalf("historical schedules=%d periods=%d complete=%d", schedules, periods, complete)
	}
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
}
