package quota

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/routing"
)

type TargetRefusal struct {
	Quota      string
	Dimension  limits.Dimension
	RetryAfter time.Duration
}

func (a *Admission) Reserve(ctx context.Context, request admission.Request) (*admission.Lease, error) {
	return a.limiter.Reserve(ctx, request)
}

func (a *Admission) InstallSnapshots(ctx context.Context, snapshots []limits.Snapshot) error {
	if !a.Ready() {
		return nil
	}
	return a.limiter.InstallSnapshots(ctx, snapshots)
}

func (a *Admission) Fleet() *fleet.State { return a.fleet }

// TargetReservation holds what one attempt reserved on its provider.
type TargetReservation struct {
	connection  *admission.Lease
	slot        *admission.Lease
	shared      *admission.Lease
	sharedGroup string
	settled     atomic.Bool
	log         *slog.Logger
}

func (t *TargetReservation) SharedGroup() string {
	if t == nil {
		return ""
	}
	return t.sharedGroup
}

func (t *TargetReservation) SharedLease() (lookup, leaseID string, ok bool) {
	if t == nil {
		return "", "", false
	}
	return t.shared.RateLease()
}

// Settle reconciles and releases every lease once the attempt has ended. It
// runs at most once, so a settlement replayed after a move or a retry still
// lands on the original leases exactly once.
func (t *TargetReservation) Settle(ctx context.Context, dispatched bool, usage *admission.TokenUsage) {
	if t == nil || !t.settled.CompareAndSwap(false, true) {
		return
	}
	SettleLease(ctx, t.shared, dispatched, usage, t.log)
	SettleLease(ctx, t.connection, dispatched, usage, t.log)
	SettleLease(ctx, t.slot, dispatched, usage, t.log)
}

func quotaRequest(lookup string, rate admission.RateLimits, ttl time.Duration) admission.Request {
	return admission.Request{Lookup: lookup, LeaseTTL: ttl, Rate: rate}
}

func connectionRequest(provider *routing.Provider, demand TokenDemand, ttl time.Duration, priority string) admission.Request {
	if provider.Limits == nil {
		return admission.Request{}
	}
	return quotaRequest(limits.ConnectionLookup(provider.ID), admission.RateLimits{
		RequestsPerMinute: provider.Limits.RequestsPerMinute, TokensPerMinute: provider.Limits.TokensPerMinute,
		InputTokensPerMinute: provider.Limits.InputTokensPerMinute, OutputTokensPerMinute: provider.Limits.OutputTokensPerMinute,
		MaxConcurrency: provider.Limits.MaxConcurrency,
		Tokens:         demand.Total, InputTokens: demand.Input, OutputTokens: demand.Output,
		Share: share(provider.Limits.Supply, priority),
	}, ttl)
}

func slotRequest(slot *routing.Slot, demand TokenDemand, ttl time.Duration, priority string) admission.Request {
	return quotaRequest(limits.SlotLookup(slot.ID), admission.RateLimits{
		RequestsPerMinute: slot.RequestsPerMinute, TokensPerMinute: slot.TokensPerMinute,
		InputTokensPerMinute: slot.InputTokensPerMinute, OutputTokensPerMinute: slot.OutputTokensPerMinute,
		MaxConcurrency: slot.MaxConcurrency,
		Tokens:         demand.Total, InputTokens: demand.Input, OutputTokens: demand.Output,
		Share: share(slot.Supply, priority),
	}, ttl)
}

// ReserveTarget admits one attempt against the provider connection quota and
// then the credential slot quota. It returns the reservation the attempt must
// settle, or a rejection to record as a failed attempt, or skip when a quota
// is configured but cannot be consulted: an unmeterable target is passed over
// so a sibling can serve, never used unmetered.
func (a *Admission) ReserveTarget(ctx context.Context, provider *routing.Provider, slot *routing.Slot, demand TokenDemand, ttl time.Duration, priority string, model string, generation int64) (reservation *TargetReservation, rejection *TargetRefusal, skip bool) {
	if provider.Limits.TokenLimited() {
		if a.checkPendingTokenAccounting(ctx, provider.ID) != nil {
			return nil, nil, true
		}
	}
	if slot.TokenLimited() {
		if a.checkPendingTokenAccounting(ctx, slot.ID) != nil {
			return nil, nil, true
		}
	}
	assignment, shared := sharedQuotaFor(provider, model)
	var group admission.Request
	if shared {
		group = sharedRequest(assignment, generation, demand, ttl)
	}
	connection := connectionRequest(provider, demand, ttl, priority)
	credential := slotRequest(slot, demand, ttl, priority)
	if !group.HasHardLimits() && !connection.HasHardLimits() && !credential.HasHardLimits() {
		return nil, nil, false
	}
	if !a.Ready() {
		a.Logger().Warn("skipping target with unenforceable quotas", "provider_id", provider.ID, "slot_id", slot.ID)
		return nil, nil, true
	}
	reservation = &TargetReservation{log: a.Logger()}
	if shared {
		reservation.sharedGroup = assignment.groupID
	}
	for _, step := range [...]struct {
		request admission.Request
		lease   **admission.Lease
		quota   string
	}{{group, &reservation.shared, SharedGroup}, {connection, &reservation.connection, Connection}, {credential, &reservation.slot, Slot}} {
		if !step.request.HasHardLimits() {
			continue
		}
		decision, cancel := context.WithTimeout(ctx, ReserveTimeout)
		lease, err := a.limiter.Reserve(decision, step.request)
		cancel()
		if err == nil {
			*step.lease = lease
			continue
		}
		settleTargetRefund(ctx, reservation)
		if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
			return nil, &TargetRefusal{Quota: step.quota, Dimension: exceeded.Dimension, RetryAfter: retryHint(exceeded.Dimension, exceeded.RetryAfter)}, false
		}
		a.Logger().Warn("skipping target with unenforceable quotas", "provider_id", provider.ID, "slot_id", slot.ID, "error", err.Error())
		return nil, nil, true
	}
	return reservation, nil, false
}

// settleTargetRefund returns everything a partially reserved attempt took.
func settleTargetRefund(ctx context.Context, reservation *TargetReservation) {
	SettleKey(ctx, reservation.shared, false, nil, reservation.log)
	SettleKey(ctx, reservation.connection, false, nil, reservation.log)
	SettleKey(ctx, reservation.slot, false, nil, reservation.log)
}

// Cooldown records a shared cooldown for the credential and for the slot, so
// every replica avoids a target the upstream just rejected instead of each
// learning it alone.
func (a *Admission) Cooldown(ctx context.Context, providerID string, slot *routing.Slot, generation int64, d time.Duration, credential bool) {
	if !a.Ready() || d <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coordinationTimeout)
	defer cancel()
	scope := fleet.SlotScope(slot.ID)
	if credential {
		scope = fleet.CredentialScope(providerID, slot.CredentialID, generation)
	}
	{
		if err := a.fleet.Cooldown(ctx, scope, d); err != nil {
			a.Logger().Warn("shared cooldown not recorded", "provider_id", providerID, "slot_id", slot.ID, "error", err.Error())
		}
	}
}

// EndCredentialCooldown ends a credential version's shared cooldown before it
// runs out, for every replica.
func (a *Admission) EndCredentialCooldown(ctx context.Context, providerID, credentialID string, generation int64) {
	if !a.Ready() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coordinationTimeout)
	defer cancel()
	if err := a.fleet.Cooldown(ctx, fleet.CredentialScope(providerID, &credentialID, generation), 0); err != nil {
		a.Logger().Warn("shared cooldown not ended", "provider_id", providerID, "credential_id", credentialID, "error", err.Error())
	}
}

// Cooling reports whether another replica put this credential or slot in a
// cooldown. A store that cannot answer must not take every slot out of
// service, so an unreadable cooldown is treated as absent and logged.
func (a *Admission) Cooling(ctx context.Context, providerID string, slot *routing.Slot, generation int64) bool {
	if !a.Ready() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, coordinationTimeout)
	defer cancel()
	cooling, err := a.fleet.Cooling(ctx, fleet.CredentialScope(providerID, slot.CredentialID, generation), fleet.SlotScope(slot.ID))
	if err != nil {
		a.Logger().Warn("shared cooldown not read", "provider_id", providerID, "slot_id", slot.ID, "error", err.Error())
		return false
	}
	return cooling
}

// maxEstimate is the largest integer the limiter can store.
