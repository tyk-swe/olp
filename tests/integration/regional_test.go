//go:build integration

package integration_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestRegionalRateAndConcurrencyLimitsWithGlobalBudgetReconciliation(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "regional key", "scopes": []string{"inference"}, "allowed_routes": []string{}, "requests_per_minute": 2, "tokens_per_minute": 100, "max_concurrency": 1, "regional_limits": map[string]any{"west": map[string]any{"requests_per_minute": 1}}}, map[string]string{"Idempotency-Key": "regional-api-key"}, http.StatusCreated)
	secret := key["secret"].(string)
	west := limClient(t)
	regionalConfig, err := coordination.Configuration(required(t, "OLP_TEST_VALKEY_REGIONAL_URL"), required(t, "OLP_TEST_CA_FILE"), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	east, err := coordination.Open(t.Context(), regionalConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(east.Close)
	// Both stores use the same installation namespace; they are independent
	// regional services, not key namespaces pretending to be separate fleets.
	namespace := limNamespace(t, west, "regions")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		values, err := east.Do(ctx, "KEYS", namespace+"*")
		if err == nil {
			for _, v := range values.([]any) {
				east.Do(ctx, "DEL", v.(string))
			}
		}
	})
	westLimiter, eastLimiter := limLimiter(t, west, namespace), limLimiter(t, east, namespace)
	for _, region := range []struct {
		name    string
		limiter *limits.Limiter
		rpm     int64
	}{{"west", westLimiter, 1}, {"east", eastLimiter, 2}} {
		manager := runtime.NewManager(h.Pool, h.Server.Installation, h.Server.Auth, h.Server.Keys, slog.New(slog.DiscardHandler))
		manager.Region = region.name
		if err := manager.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		authority, err := manager.Authenticate(secret)
		if err != nil {
			t.Fatal(err)
		}
		if *authority.Policy.RequestsPerMinute != region.rpm {
			t.Fatalf("%s effective policy = %+v", region.name, authority.Policy)
		}
		request := limRequest(authority.LookupID)
		request.RequestsPerMinute = authority.Policy.RequestsPerMinute
		request.TokensPerMinute = authority.Policy.TokensPerMinute
		request.MaxConcurrency = authority.Policy.MaxConcurrency
		concurrent := request
		concurrent.LookupID = limLookup()
		concurrent.RequestsPerMinute, concurrent.TokensPerMinute = nil, nil
		held, err := region.limiter.Reserve(t.Context(), concurrent)
		if err != nil {
			t.Fatal(err)
		}
		_, err = region.limiter.Reserve(t.Context(), concurrent)
		var exceeded *limits.ExceededError
		if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionConcurrency {
			t.Fatalf("%s concurrency was not enforced: %v", region.name, err)
		}
		if err = held.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		lease, err := region.limiter.Reserve(t.Context(), request)
		if err != nil {
			t.Fatalf("%s admission: %v", region.name, err)
		}
		// Concurrency is regional too. The other fleet can still hold a lease.
		if region.name == "west" {
			other, err := eastLimiter.Reserve(t.Context(), request)
			if err != nil {
				t.Fatalf("regional concurrency leaked: %v", err)
			}
			if err = other.Refund(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if err = lease.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		for used := int64(1); used < region.rpm; used++ {
			lease, err = region.limiter.Reserve(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if err = lease.Release(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = region.limiter.Reserve(t.Context(), request); err == nil {
			t.Fatalf("%s RPM override was not enforced", region.name)
		}
	}

	// Each regional leader must coexist and install the same durable cost in
	// its own Valkey. A contender in the same region must be excluded.
	westLeader, err := limits.TryAcquireRegionalLeader(t.Context(), h.Pool, "west")
	if err != nil || westLeader == nil {
		t.Fatalf("west leadership: %v", err)
	}
	defer westLeader.Close(context.Background())
	eastLeader, err := limits.TryAcquireRegionalLeader(t.Context(), h.Pool, "east")
	if err != nil || eastLeader == nil {
		t.Fatalf("east leadership: %v", err)
	}
	defer eastLeader.Close(context.Background())
	contender, err := limits.TryAcquireRegionalLeader(t.Context(), h.Pool, "west")
	if err != nil {
		t.Fatal(err)
	}
	if contender != nil {
		contender.Close(context.Background())
		t.Fatal("duplicate regional leader")
	}
	seedOwner, provider := limSeedAuthority(t, h.Pool)
	budgetKey := limSeedKey(t, h.Pool, seedOwner, false)
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET policy='{"daily_cost_limit":"1","monthly_cost_limit":"1"}'::jsonb WHERE id=$1`, budgetKey); err != nil {
		t.Fatal(err)
	}
	lookup := limLookup()
	now := time.Now().UTC()
	for _, region := range []struct {
		leader  *limits.Leader
		limiter *limits.Limiter
	}{{westLeader, westLimiter}, {eastLeader, eastLimiter}} {
		if _, err = region.leader.Reconcile(t.Context(), region.limiter, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, limiter := range []*limits.Limiter{westLimiter, eastLimiter} {
		request := limRequest(lookup)
		request.CostOwnerID = budgetKey
		request.DailyCostLimit = limPointer("1")
		request.MonthlyCostLimit = limPointer("1")
		request.CostEstimate = "0.6"
		request.RequestID = uuid.NewString()
		lease, err := limiter.Reserve(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		lease.SetActualCost("0.6")
		if err = lease.Release(t.Context()); err != nil {
			t.Fatal(err)
		}
		limSeedFact(t, h.Pool, provider, budgetKey, now, limPointer("0.6"))
	}
	// Two regions admitted 0.6 while each had a zero-spend snapshot. Global
	// spend is 1.2, and overshoot 0.2 is within regions × 0.6 admitted cost.
	var globalSpend string
	if err = h.Pool.QueryRow(t.Context(), "SELECT sum(estimated_cost)::text FROM olp.attempt_usage_facts WHERE api_key_id=$1", budgetKey).Scan(&globalSpend); err != nil {
		t.Fatal(err)
	}
	spent, err := decimal.NewFromString(globalSpend)
	if err != nil {
		t.Fatal(err)
	}
	if !spent.Equal(decimal.RequireFromString("1.2")) || spent.Sub(decimal.NewFromInt(1)).GreaterThan(decimal.RequireFromString("0.6").Mul(decimal.NewFromInt(2))) {
		t.Fatalf("global cost exceeds the documented admission envelope: %s", globalSpend)
	}
	for _, region := range []struct {
		leader  *limits.Leader
		limiter *limits.Limiter
	}{{westLeader, westLimiter}, {eastLeader, eastLimiter}} {
		if _, err = region.leader.Reconcile(t.Context(), region.limiter, now); err != nil {
			t.Fatal(err)
		}
		request := limRequest(lookup)
		request.CostOwnerID = budgetKey
		request.DailyCostLimit = limPointer("1")
		request.MonthlyCostLimit = limPointer("1")
		request.CostEstimate = "0.01"
		request.RequestID = uuid.NewString()
		_, err = region.limiter.Reserve(t.Context(), request)
		var exceeded *limits.ExceededError
		if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionDailyCost {
			t.Fatalf("global reconciliation failed in region: %v", err)
		}
	}
	if err = usage.CheckpointTask(usage.WithWorkerRegion(t.Context(), "west"), h.Pool, usage.TaskCostReconciliation, usage.OutcomeSuccess, true); err != nil {
		t.Fatal(err)
	}
	expected := []observability.WorkerTask{{Name: string(usage.TaskCostReconciliation)}}
	for _, region := range []string{"west", "east"} {
		health, err := observability.ReadRegionalWorkerTaskHealth(t.Context(), h.Pool, region)
		if err != nil {
			t.Fatal(err)
		}
		if health.CurrentFor(expected) != (region == "west") {
			t.Fatalf("%s readiness borrowed another region's success", region)
		}
	}
	if err = usage.CheckpointTask(usage.WithWorkerRegion(t.Context(), "east"), h.Pool, usage.TaskCostReconciliation, usage.OutcomeFailure, false); err != nil {
		t.Fatal(err)
	}
	health, err := observability.ReadRegionalWorkerTaskHealth(t.Context(), h.Pool, "west")
	if err != nil || !health.CurrentFor(expected) {
		t.Fatalf("east failure changed west health: %v", err)
	}
}
