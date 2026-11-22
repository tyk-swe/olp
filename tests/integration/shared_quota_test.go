//go:build integration

package integration_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/release"
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

type roundTripBudget struct {
	scripts.Commander
	remaining atomic.Int64
}

func (b *roundTripBudget) Do(ctx context.Context, args ...string) (any, error) {
	if b.remaining.Add(-1) < 0 {
		return nil, context.DeadlineExceeded
	}
	return b.Commander.Do(ctx, args...)
}

func (b *roundTripBudget) Pipeline(ctx context.Context, commands ...[]string) ([]any, error) {
	if b.remaining.Add(-1) < 0 {
		return nil, context.DeadlineExceeded
	}
	return b.Commander.Pipeline(ctx, commands...)
}

func TestSharedTokenQuotaMoveKeepsOutstandingLeasesInTheirGroup(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	installation, err := database.Installation(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	auth := secrets.NewAuthKey(make([]byte, 32), installation)
	manager := func(limiter *admission.Limiter) *release.Manager {
		return release.NewManager(release.ManagerConfig{
			Pool: pool, Installation: installation, Auth: auth, Log: slog.New(slog.DiscardHandler), Hostname: uuid.NewString(),
			SyncSharedQuotas: func(ctx context.Context, previous, next *routing.View, skippedGroupIDs ...string) error {
				return quota.SyncSharedQuotas(ctx, limiter, previous, next, skippedGroupIDs...)
			},
		})
	}
	publish := func(ordinal int64, provider routing.Provider) {
		t.Helper()
		snapshot := &routing.Snapshot{
			Generation: routing.Generation{ID: uuid.NewString(), Ordinal: ordinal},
			Providers:  map[string]routing.Provider{provider.ID: provider},
			Routes:     map[string]routing.Route{},
		}
		digest, err := snapshot.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.runtime_releases (id,sequence,sha256,snapshot,created_by) VALUES ($1,$2,$3,$4,$5)`,
			snapshot.Generation.ID, ordinal, digest, encoded(t, snapshot), fixture.ownerID); err != nil {
			t.Fatal(err)
		}
	}
	awayFromMinuteRollover()
	prefix := "olp:" + uuid.NewString() + ":"
	firstReplica, runner := sharedQuotaReplica(t, prefix)
	secondReplica, _ := sharedQuotaReplica(t, prefix)
	project := fixture.projectID
	before := sharedQuotaProvider(project, "model-a", "group-a", 1000)
	before.Kind, before.Name, before.RevisionID = connectors.KindOpenAICompatible, "shared quota", uuid.NewString()
	after := before
	after.SharedTokenQuotas = []routing.SharedTokenQuota{
		{Model: "model-a", Group: "group-b", TokensPerMinute: 1000},
		{Model: "model-c", Group: "group-c", TokensPerMinute: 1000},
		{Model: "model-d", Group: "group-d", TokensPerMinute: 1000},
	}
	slot := routing.Slot{ID: uuid.NewString()}
	demand := func(tokens int64) quota.TokenDemand { return quota.CombinedDemand(tokens) }
	publish(2, before)
	first := manager(firstReplica.limiter)
	if err := first.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	old, refusal, skip := firstReplica.ReserveTarget(t.Context(), &before, &slot, demand(400), time.Minute, "", "model-a", first.Release().Sequence)
	if refusal != nil || skip || old == nil {
		t.Fatalf("reserve in group A: refusal=%+v skip=%v", refusal, skip)
	}
	if got := groupTokens(t, runner, project, "group-a"); got != 400 {
		t.Fatalf("group A after first reserve = %d, want 400", got)
	}
	t.Logf("reserved 400 in group-a, group-a=%d group-b=%d", groupTokens(t, runner, project, "group-a"), groupTokens(t, runner, project, "group-b"))

	publish(3, after)
	bounded := &roundTripBudget{Commander: harness.Valkey(t)}
	bounded.remaining.Store(2)
	boundedRunner, err := scripts.NewRunner(bounded, prefix)
	if err != nil {
		t.Fatal(err)
	}
	second := manager(admission.NewDurable(boundedRunner, nil))
	if err := second.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh with a limited Valkey round-trip budget: %v", err)
	}
	moved, refusal, skip := secondReplica.ReserveTarget(t.Context(), &after, &slot, demand(200), time.Minute, "", "model-a", second.Release().Sequence)
	if refusal != nil || skip || moved == nil {
		t.Fatalf("reserve in group B: refusal=%+v skip=%v", refusal, skip)
	}
	stale, refusal, skip := firstReplica.ReserveTarget(t.Context(), &before, &slot, demand(100), time.Minute, "", "model-a", first.Release().Sequence)
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

	first := sharedQuotaView(1, provider)
	second := sharedQuotaView(2, provider)
	if err := quota.SyncSharedQuotas(t.Context(), current.limiter, nil, first); err != nil {
		t.Fatal(err)
	}
	if err := quota.SyncSharedQuotas(t.Context(), current.limiter, first, second); err != nil {
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
	lowered := provider
	lowered.SharedTokenQuotas = []routing.SharedTokenQuota{{Model: "model-a", Group: "shared", TokensPerMinute: 500}}
	if err := quota.SyncSharedQuotas(t.Context(), current.limiter, second, sharedQuotaView(3, lowered)); err != nil {
		t.Fatal(err)
	}
	extra, refusal, skip := lagging.ReserveTarget(t.Context(), &provider, &slot, quota.CombinedDemand(1), time.Minute, "", "model-a", 1)
	if extra != nil || skip || refusal == nil || refusal.Quota != quota.SharedGroup || refusal.Dimension != limits.DimensionTokens {
		t.Fatalf("lagging replica over the lowered ceiling: reservation=%v refusal=%+v skip=%v", extra != nil, refusal, skip)
	}
	t.Logf("lagging replica with a %d TPM snapshot refused an extra token at the installed %d TPM ceiling", provider.SharedTokenQuotas[0].TokensPerMinute, lowered.SharedTokenQuotas[0].TokensPerMinute)
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

func TestSharedTokenQuotaRefusalMovesOnToTheNextTarget(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, grouped, open = "shared-quota-fallback", "grouped-model", "open-model"
	fixture := newUsageFixture(t, pool)
	upstream := &retainedUpstream{replies: func(string, int) map[string]string {
		return map[string]string{"POST /chat/completions": `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"` + open + `","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`}
	}}
	g := newStoredGateway(t, fixture, storedPlan{slug: slug, model: grouped, operations: []string{"generation"}, upstream: upstream})
	g.key(t, "owner")

	before := g.runtime.release.Load().Snapshot()
	provider := g.snapshot.Providers[fixture.providerID]
	provider.ProjectID = &fixture.projectID
	provider.SharedTokenQuotas = []routing.SharedTokenQuota{{Model: grouped, Group: "fallback", TokensPerMinute: 1}}
	sibling, another := provider.Slots[0], provider.Slots[0]
	sibling.ID, another.ID = uuid.NewString(), uuid.NewString()
	provider.Slots = append(provider.Slots, sibling, another)
	openCapability := provider.Capabilities[0]
	openCapability.Model = open
	provider.Capabilities = append(provider.Capabilities, openCapability)
	g.snapshot.Providers[fixture.providerID] = provider
	g.republish(t, func(route *routing.Route) {
		fallback := route.Targets[0]
		fallback.ID, fallback.ProviderModel, fallback.Priority = uuid.NewString(), open, route.Targets[0].Priority+1
		route.Targets = append(route.Targets, fallback)
	})
	if err := quota.SyncSharedQuotas(t.Context(), g.limiter, before, g.runtime.release.Load().Snapshot()); err != nil {
		t.Fatal(err)
	}

	g.send(t, "POST", "/v1/chat/completions", map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"},
		`{"model":"`+slug+`","messages":[{"role":"user","content":"hi"}]}`, 200)
	sent := upstream.sent("POST /chat/completions")
	if len(sent) != 1 || !strings.Contains(sent[0].body, `"model":"`+open+`"`) {
		t.Fatalf("upstream chat calls = %+v, want one call to the %s fallback target", sent, open)
	}
}
