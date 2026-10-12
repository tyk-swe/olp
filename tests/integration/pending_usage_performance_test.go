//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/resources"
)

// Capture the query actually issued by the production owner, so explain and
// occupancy qualification cannot drift to a hand-copied statement.
type pendingQueryTrace struct {
	mu  sync.Mutex
	sql string
}

func (p *pendingQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	p.mu.Lock()
	p.sql = data.SQL
	p.mu.Unlock()
	return ctx
}
func (*pendingQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPendingCostOwnerLookupAtOccupiedProject(t *testing.T) {
	h := newAccessHarness(t)
	session := h.owner()
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, session, "gemini-interactions", provider)
	local, _, owner := pendingGeminiGeneration(t, h, slug, key)
	id := local[len("interaction_"):]
	// Accepted mappings can rotate their accounting projections faster than
	// autovacuum runs. Keep the private fixture's dead tuples so automatic
	// cleanup cannot hide an unbounded proof before the next vacuum.
	if _, err := h.Pool.Exec(t.Context(), `ALTER TABLE olp.provider_resource_pending_usage SET (autovacuum_enabled=false)`); err != nil {
		t.Fatal(err)
	}
	busy, clear := uuid.NewString(), uuid.NewString()
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.projects(id,name,etag,created_by) SELECT $1::uuid,'Busy',$1::uuid,created_by FROM olp.api_keys WHERE id=$3
 UNION ALL SELECT $2::uuid,'Clear',$2::uuid,created_by FROM olp.api_keys WHERE id=$3`, busy, clear, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET project_id=$1 WHERE id=$2`, busy, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage,attribution}','{"team":"busy"}'::jsonb) WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = limits.EnsureAggregateBudget(t.Context(), tx, "project", clear); err != nil {
		t.Fatal(err)
	}
	if err = limits.EnsureAttributionBudget(t.Context(), tx, busy, "team", "clear"); err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = limits.EnsureAggregateBudget(t.Context(), tx, "installation", installation); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	trace := &pendingQueryTrace{}
	cfg := h.Pool.Config()
	cfg.MaxConns = 1
	cfg.ConnConfig.Tracer = trace
	probe, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	store := resources.New(probe)
	subjects := []string{limits.AggregateBudgetID("project", clear), limits.AttributionBudgetID(busy, "team", "clear")}
	baselines := make(map[string]float64)
	for _, subject := range subjects {
		for range 3 {
			if err = store.CheckPendingCostOwner(t.Context(), subject); err != nil {
				t.Fatal(err)
			}
		}
		baselines[subject] = pendingProofBuffers(t, probe, trace, subject)
	}
	if _, err = h.Pool.Exec(t.Context(), `INSERT INTO olp.provider_resources(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at)
 SELECT v.id,'response',r.api_key_id,r.route_slug,r.provider_id,r.provider_revision_id,r.route_revision_id,r.slot_id,r.credential_id,v.id::text,'in_progress',
 jsonb_set(jsonb_set(r.metadata,'{pending_usage,request_id}',to_jsonb(v.id::text)),'{pending_usage,event_id}',to_jsonb(v.id::text)),r.expires_at
 FROM olp.provider_resources r CROSS JOIN (SELECT gen_random_uuid() AS id FROM generate_series(1,10000)) v WHERE r.id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), `ANALYZE olp.provider_resource_pending_usage; ANALYZE olp.pending_resource_recorded_owners`); err != nil {
		t.Fatal(err)
	}
	for _, subject := range subjects {
		started := time.Now()
		for range 30 {
			if err = store.CheckPendingCostOwner(t.Context(), subject); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("10001 occupied rows: clear owner average=%s", time.Since(started)/30)
		buffers := pendingProofBuffers(t, probe, trace, subject)
		t.Logf("executed proof buffers: one-row=%g occupied=%g", baselines[subject], buffers)
		// Result instrumentation includes nested function queries. A full scan
		// at this occupancy touches hundreds of heap pages, whereas indexed
		// empty-scope proofs stay within a fixed additional page budget.
		if buffers > baselines[subject]+64 {
			t.Errorf("proof work grew with unrelated occupied rows: baseline=%g occupied=%g", baselines[subject], buffers)
		}
	}
	// Existing bounded Responses can record supply owners in their original
	// envelopes. Qualify the array lookup when every occupied row has owners,
	// as well as the common unbudgeted acceptance case above.
	for offset := 0; offset < 10001; offset += 1000 {
		if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage,attempts,0,routing,budgets}',jsonb_build_array(provider_id::text,slot_id::text)) WHERE id IN (SELECT id FROM olp.provider_resources WHERE metadata ? 'pending_usage' ORDER BY id OFFSET $1 LIMIT 1000)`, offset); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = h.Pool.Exec(t.Context(), `ANALYZE olp.provider_resource_pending_usage; ANALYZE olp.pending_resource_recorded_owners`); err != nil {
		t.Fatal(err)
	}
	for _, subject := range subjects {
		if err = store.CheckPendingCostOwner(t.Context(), subject); err != nil {
			t.Fatal(err)
		}
		buffers := pendingProofBuffers(t, probe, trace, subject)
		t.Logf("recorded-owner occupied proof buffers=%g", buffers)
		if buffers > baselines[subject]+64 {
			t.Errorf("recorded owners caused an occupied proof scan: baseline=%g occupied=%g", baselines[subject], buffers)
		}
	}
	installationOwner := limits.AggregateBudgetID("installation", installation)
	if err = store.CheckPendingCostOwner(t.Context(), installationOwner); !errors.Is(err, resources.ErrPendingUsage) {
		t.Fatalf("installation did not own occupied pending subjects: %v", err)
	}
	installationBuffers := pendingProofBuffers(t, probe, trace, installationOwner)
	t.Logf("installation occupied proof buffers=%g", installationBuffers)
	if installationBuffers > 64 {
		t.Errorf("installation proof scanned the dead heap prefix: buffers=%g", installationBuffers)
	}
	if _, err = h.Pool.Exec(t.Context(), `DELETE FROM olp.provider_resources WHERE metadata ? 'pending_usage'`); err != nil {
		t.Fatal(err)
	}
	for _, subject := range append(subjects, installationOwner, uuid.NewString(), uuid.Nil.String()) {
		if err = store.CheckPendingCostOwner(t.Context(), subject); err != nil {
			t.Fatalf("empty projection retained owner %s: %v", subject, err)
		}
	}
	if err = store.CheckPendingCostOwner(t.Context(), "invalid-owner"); err == nil || errors.Is(err, resources.ErrPendingUsage) {
		t.Fatalf("invalid owner did not fail closed: %v", err)
	}
}
func pendingProofBuffers(t *testing.T, pool *pgxpool.Pool, trace *pendingQueryTrace, subject string) float64 {
	t.Helper()
	trace.mu.Lock()
	query := trace.sql
	trace.mu.Unlock()
	// Explain always starts with the actual production query, even after the
	// previous probe itself replaced the tracer's last statement.
	for len(query) > 0 && query[:min(len(query), len("EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "))] == "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) " {
		query = query[len("EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "):]
	}
	var raw []byte
	if err := pool.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query, subject).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("explain=%s err=%v", raw, err)
	}
	var buffers float64
	for _, field := range []string{"Shared Hit Blocks", "Shared Read Blocks"} {
		if count, ok := plans[0].Plan[field].(float64); ok {
			buffers += count
		}
	}
	return buffers
}
