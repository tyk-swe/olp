//go:build integration

package integration_test

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
)

type sharedQuotaReplicaState struct {
	*quota.Admission
	limiter *admission.Limiter
}

func sharedQuotaReplica(t *testing.T, prefix string) (sharedQuotaReplicaState, scripts.Runner) {
	t.Helper()
	runner, err := scripts.NewRunner(harness.Valkey(t), prefix)
	if err != nil {
		t.Fatal(err)
	}
	limiter := admission.NewDurable(runner, nil)
	return sharedQuotaReplicaState{quota.NewAdmission(limiter, nil, nil, slog.New(slog.DiscardHandler)), limiter}, runner
}

func sharedQuotaProvider(project, model, group string, ceiling int64) routing.Provider {
	return routing.Provider{
		ID:        uuid.NewString(),
		ProjectID: &project,
		SharedTokenQuotas: []routing.SharedTokenQuota{
			{Model: model, Group: group, TokensPerMinute: ceiling},
		},
	}
}

func sharedQuotaView(ordinal int64, providers ...routing.Provider) *routing.View {
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: ordinal},
		Providers:  map[string]routing.Provider{},
		Routes:     map[string]routing.Route{},
	}
	for _, provider := range providers {
		snapshot.Providers[provider.ID] = provider
	}
	return snapshot.ReadOnly()
}

func groupTokens(t *testing.T, runner scripts.Runner, project, group string) int64 {
	t.Helper()
	usage, err := runner.Usage(t.Context(), limits.SharedQuotaLookup(routing.SharedTokenQuotaGroupID(project, group)))
	if err != nil {
		t.Fatal(err)
	}
	return usage.TokensThisMinute
}

func TestSharedTokenQuotaMoveKeepsOutstandingLeasesInTheirGroup(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	prefix := "olp:" + uuid.NewString() + ":"
	firstReplica, runner := sharedQuotaReplica(t, prefix)
	secondReplica, _ := sharedQuotaReplica(t, prefix)
	project := uuid.NewString()
	before := sharedQuotaProvider(project, "model-a", "group-a", 1000)
	after := before
	after.SharedTokenQuotas = []routing.SharedTokenQuota{{Model: "model-a", Group: "group-b", TokensPerMinute: 1000}}
	slot := routing.Slot{ID: uuid.NewString()}
	demand := func(tokens int64) quota.TokenDemand { return quota.CombinedDemand(tokens) }

	old, refusal, skip := firstReplica.ReserveTarget(t.Context(), &before, &slot, demand(400), time.Minute, "", "model-a", 1)
	if refusal != nil || skip || old == nil {
		t.Fatalf("reserve in group A: refusal=%+v skip=%v", refusal, skip)
	}
	if got := groupTokens(t, runner, project, "group-a"); got != 400 {
		t.Fatalf("group A after first reserve = %d, want 400", got)
	}
	t.Logf("reserved 400 in group-a, group-a=%d group-b=%d", groupTokens(t, runner, project, "group-a"), groupTokens(t, runner, project, "group-b"))

	if err := quota.SyncSharedQuotas(t.Context(), secondReplica.limiter, sharedQuotaView(1, before), sharedQuotaView(2, after)); err != nil {
		t.Fatal(err)
	}
	moved, refusal, skip := secondReplica.ReserveTarget(t.Context(), &after, &slot, demand(200), time.Minute, "", "model-a", 2)
	if refusal != nil || skip || moved == nil {
		t.Fatalf("reserve in group B: refusal=%+v skip=%v", refusal, skip)
	}
	stale, refusal, skip := firstReplica.ReserveTarget(t.Context(), &before, &slot, demand(100), time.Minute, "", "model-a", 1)
	if stale != nil || refusal != nil || !skip {
		t.Fatalf("stale reserve in group A admitted: reservation=%v refusal=%+v skip=%v", stale != nil, refusal, skip)
	}
	if a, b := groupTokens(t, runner, project, "group-a"), groupTokens(t, runner, project, "group-b"); a != 400 || b != 200 {
		t.Fatalf("after move A=%d B=%d, want 400 and 200", a, b)
	}
	t.Logf("moved the model to group-b and reserved 200 there, stale reserve in group-a refused, group-a=%d group-b=%d", groupTokens(t, runner, project, "group-a"), groupTokens(t, runner, project, "group-b"))

	usage := &admission.TokenUsage{Total: 250, Credit: true}
	old.Settle(t.Context(), true, usage)
	old.Settle(t.Context(), true, usage)
	if a, b := groupTokens(t, runner, project, "group-a"), groupTokens(t, runner, project, "group-b"); a != 250 || b != 200 {
		t.Fatalf("after replayed settlement A=%d B=%d, want 250 and 200", a, b)
	}
	t.Logf("settled the old attempt at 250 and replayed it, group-a=%d group-b=%d", groupTokens(t, runner, project, "group-a"), groupTokens(t, runner, project, "group-b"))

	moved.Settle(t.Context(), false, nil)
	if a, b := groupTokens(t, runner, project, "group-a"), groupTokens(t, runner, project, "group-b"); a != 250 || b != 0 {
		t.Fatalf("after abort A=%d B=%d, want 250 and 0", a, b)
	}
	t.Logf("aborted the new attempt before dispatch, group-a=%d group-b=%d", groupTokens(t, runner, project, "group-a"), groupTokens(t, runner, project, "group-b"))
}

func TestSharedTokenQuotaLaggingReplicaKeepsReservingAfterUnrelatedRelease(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	prefix := "olp:" + uuid.NewString() + ":"
	current, runner := sharedQuotaReplica(t, prefix)
	lagging, _ := sharedQuotaReplica(t, prefix)
	project := uuid.NewString()
	provider := sharedQuotaProvider(project, "model-a", "shared", 1000)
	slot := routing.Slot{ID: uuid.NewString()}

	if err := quota.SyncSharedQuotas(t.Context(), current.limiter, nil, sharedQuotaView(1, provider)); err != nil {
		t.Fatal(err)
	}
	newer, refusal, skip := current.ReserveTarget(t.Context(), &provider, &slot, quota.CombinedDemand(300), time.Minute, "", "model-a", 2)
	if refusal != nil || skip || newer == nil {
		t.Fatalf("reserve at the newer release: refusal=%+v skip=%v", refusal, skip)
	}
	older, refusal, skip := lagging.ReserveTarget(t.Context(), &provider, &slot, quota.CombinedDemand(200), time.Minute, "", "model-a", 1)
	if refusal != nil || skip || older == nil {
		t.Fatalf("lagging replica reserve with unchanged membership: refusal=%+v skip=%v", refusal, skip)
	}
	if got := groupTokens(t, runner, project, "shared"); got != 500 {
		t.Fatalf("shared counter = %d, want 500", got)
	}
	newer.Settle(t.Context(), false, nil)
	older.Settle(t.Context(), false, nil)
}

func TestSharedTokenQuotaHeadroomFollowsEachModelsGroupOnOneSlot(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	replica, _ := sharedQuotaReplica(t, "olp:"+uuid.NewString()+":")
	provider := sharedQuotaProvider(uuid.NewString(), "model-a", "group-a", 1000)
	provider.SharedTokenQuotas = append(provider.SharedTokenQuotas, routing.SharedTokenQuota{Model: "model-b", Group: "group-b", TokensPerMinute: 1000})
	slot := routing.Slot{ID: uuid.NewString()}
	if err := quota.SyncSharedQuotas(t.Context(), replica.limiter, nil, sharedQuotaView(1, provider)); err != nil {
		t.Fatal(err)
	}
	lease, refusal, skip := replica.ReserveTarget(t.Context(), &provider, &slot, quota.CombinedDemand(1000), time.Minute, "", "model-a", 1)
	if refusal != nil || skip || lease == nil {
		t.Fatalf("exhaust group-a: refusal=%+v skip=%v", refusal, skip)
	}
	defer lease.Settle(t.Context(), false, nil)

	state := replica.ReadSupply(t.Context(), routing.SupplyQuery{Slots: []routing.SupplySlot{
		{Provider: &provider, Slot: slot, Model: "model-a"},
		{Provider: &provider, Slot: slot, Model: "model-b"},
	}})
	a, aKnown := state.Headroom[routing.SlotModel{Slot: slot.ID, Model: "model-a"}]
	b, bKnown := state.Headroom[routing.SlotModel{Slot: slot.ID, Model: "model-b"}]
	t.Logf("slot headroom model-a=%v (known %v) model-b=%v (known %v)", a, aKnown, b, bKnown)
	if !aKnown || !bKnown || a != 0 || b != 1 {
		t.Fatalf("headroom model-a=%v/%v model-b=%v/%v, want 0 and 1", a, aKnown, b, bKnown)
	}
}

func TestSharedTokenQuotaCannotBeOverbookedAcrossConnections(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	prefix := "olp:" + uuid.NewString() + ":"
	firstReplica, runner := sharedQuotaReplica(t, prefix)
	secondReplica, _ := sharedQuotaReplica(t, prefix)
	project := uuid.NewString()
	one := sharedQuotaProvider(project, "model-a", "shared", 500)
	two := sharedQuotaProvider(project, "model-a", "shared", 500)
	slot := routing.Slot{ID: uuid.NewString()}
	rotated := routing.Slot{ID: uuid.NewString()}

	held, refusal, skip := firstReplica.ReserveTarget(t.Context(), &one, &slot, quota.CombinedDemand(300), time.Minute, "", "model-a", 7)
	if refusal != nil || skip || held == nil {
		t.Fatalf("first member reserve: refusal=%+v skip=%v", refusal, skip)
	}
	other, refusal, skip := secondReplica.ReserveTarget(t.Context(), &two, &slot, quota.CombinedDemand(200), time.Minute, "", "model-a", 7)
	if refusal != nil || skip || other == nil {
		t.Fatalf("second member reserve: refusal=%+v skip=%v", refusal, skip)
	}
	extra, refusal, skip := firstReplica.ReserveTarget(t.Context(), &one, &rotated, quota.CombinedDemand(1), time.Minute, "", "model-a", 7)
	if extra != nil || skip || refusal == nil || refusal.Quota != quota.SharedGroup || refusal.Dimension != limits.DimensionTokens {
		t.Fatalf("overbooked reserve: refusal=%+v skip=%v", refusal, skip)
	}
	if got := groupTokens(t, runner, project, "shared"); got != 500 {
		t.Fatalf("shared counter = %d, want 500", got)
	}
	held.Settle(t.Context(), false, nil)
	other.Settle(t.Context(), false, nil)
	if got := groupTokens(t, runner, project, "shared"); got != 0 {
		t.Fatalf("shared counter after refunds = %d, want 0", got)
	}

	type outcome struct {
		reservation *quota.TargetReservation
		refusal     *quota.TargetRefusal
		skip        bool
	}
	outcomes := make(chan outcome, 10)
	var group sync.WaitGroup
	for i := range 10 {
		group.Go(func() {
			replica, provider := firstReplica, &one
			if i%2 == 1 {
				replica, provider = secondReplica, &two
			}
			reservation, rejection, skipped := replica.ReserveTarget(t.Context(), provider, &slot, quota.CombinedDemand(100), time.Minute, "", "model-a", 7)
			outcomes <- outcome{reservation, rejection, skipped}
		})
	}
	group.Wait()
	close(outcomes)
	admitted := 0
	for result := range outcomes {
		switch {
		case result.reservation != nil:
			admitted++
			result.reservation.Settle(t.Context(), false, nil)
		case result.refusal != nil && result.refusal.Quota == quota.SharedGroup:
		default:
			t.Errorf("contended reserve: refusal=%+v skip=%v", result.refusal, result.skip)
		}
	}
	if admitted != 5 {
		t.Fatalf("admitted %d contenders against a 500 ceiling, want 5", admitted)
	}
	if got := groupTokens(t, runner, project, "shared"); got != 0 {
		t.Fatalf("shared counter after contention refunds = %d, want 0", got)
	}
}

func TestSharedTokenQuotaPartialFailureLeavesNoCharge(t *testing.T) {
	t.Parallel()
	awayFromMinuteRollover()
	replica, runner := sharedQuotaReplica(t, "olp:"+uuid.NewString()+":")
	project := uuid.NewString()
	provider := sharedQuotaProvider(project, "model-a", "shared", 1000)
	provider.Limits = &routing.Limits{TokensPerMinute: new(int64(1000))}
	tight := routing.Slot{ID: uuid.NewString(), Limits: routing.Limits{TokensPerMinute: new(int64(10))}}

	reservation, refusal, skip := replica.ReserveTarget(t.Context(), &provider, &tight, quota.CombinedDemand(200), time.Minute, "", "model-a", 3)
	if reservation != nil || skip || refusal == nil || refusal.Quota != quota.Slot {
		t.Fatalf("tight slot reserve: refusal=%+v skip=%v", refusal, skip)
	}
	if got := groupTokens(t, runner, project, "shared"); got != 0 {
		t.Fatalf("shared counter after partial failure = %d, want 0", got)
	}
	usage, err := runner.Usage(t.Context(), limits.ConnectionLookup(provider.ID))
	if err != nil {
		t.Fatal(err)
	}
	if usage.TokensThisMinute != 0 {
		t.Fatalf("connection counter after partial failure = %d, want 0", usage.TokensThisMinute)
	}
}
