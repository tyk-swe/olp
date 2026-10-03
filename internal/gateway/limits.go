package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

// Admission enforces the budgets every replica shares through Valkey: the API
// key limits before a request is served, and the provider connection and
// credential slot quotas before each attempt leaves this gateway. A nil
// *Admission means no limiter was configured at all, which admits keys and
// targets that bound nothing and fails everything else closed.
type Admission struct {
	limiter                 *limits.Limiter
	policy                  func() limits.OutagePolicy
	log                     *slog.Logger
	failOpen                atomic.Int64
	dailyBudgetRejections   atomic.Int64
	monthlyBudgetRejections atomic.Int64
}

// NewAdmission binds a limiter to the outage policy of the installation. The
// policy is read per decision so an operator can change it without a restart;
// a nil policy fails closed.
func NewAdmission(limiter *limits.Limiter, policy func() limits.OutagePolicy, log *slog.Logger) *Admission {
	return &Admission{limiter: limiter, policy: policy, log: log}
}

// FailOpenTotal counts the requests admitted without a reservation because the
// limiter could not answer and the installation chose to fail open. It is the
// source of the olp_limits_fail_open_total metric.
func (a *Admission) FailOpenTotal() int64 {
	if a == nil {
		return 0
	}
	return a.failOpen.Load()
}

// BudgetRejections counts the requests a cost budget rejected in the given
// window. It is the source of the olp_key_budget_rejections_total metric.
func (a *Admission) BudgetRejections(dimension limits.Dimension) int64 {
	if a == nil {
		return 0
	}
	switch dimension {
	case limits.DimensionDailyCost:
		return a.dailyBudgetRejections.Load()
	case limits.DimensionMonthlyCost:
		return a.monthlyBudgetRejections.Load()
	}
	return 0
}

// recordRejection counts the windows Prometheus reports. Rate and concurrency
// rejections are per-request churn; cost budget rejections are the durable
// evidence that a budget stopped spend.
func (a *Admission) recordRejection(dimension limits.Dimension) {
	switch dimension {
	case limits.DimensionDailyCost:
		a.dailyBudgetRejections.Add(1)
	case limits.DimensionMonthlyCost:
		a.monthlyBudgetRejections.Add(1)
	}
}

const (
	// reserveTimeout bounds one admission decision. A limiter that cannot
	// answer this quickly is an outage, not a slow allow.
	reserveTimeout = time.Second
	// coordinationTimeout bounds the cooldown reads and writes that are
	// advisory: the request proceeds either way.
	coordinationTimeout = 250 * time.Millisecond
	// maxConcurrencyRetryHint caps the wait suggested for a concurrency
	// rejection, which clears as soon as any in-flight request finishes.
	maxConcurrencyRetryHint = 5 * time.Second
)

func (a *Admission) ready() bool { return a != nil && a.limiter != nil }

func (a *Admission) logger() *slog.Logger {
	if a == nil || a.log == nil {
		return slog.Default()
	}
	return a.log
}

// limitsUnavailable is the terminal answer when a budget that must be enforced
// cannot be consulted.
func limitsUnavailable() *Error {
	return serverError(http.StatusServiceUnavailable, "distributed_limits_unavailable", "Request limits cannot be enforced right now; retry shortly.")
}

// rateLimited renders the rejection a shared budget produced. estimate says a
// cost budget refused the request although it is not spent: the request's own
// estimated cost does not fit beside what is accrued and in flight.
func rateLimited(dimension limits.Dimension, retryAfter time.Duration, estimate bool) *Error {
	code, message := "rate_limit_exceeded", "The API key rate limit was exceeded."
	switch dimension {
	case limits.DimensionRequests:
		message = "The API key requests per minute limit was exceeded."
	case limits.DimensionTokens:
		message = "The API key tokens per minute limit was exceeded."
	case limits.DimensionConcurrency:
		message = "The API key concurrency limit was exceeded."
	case limits.DimensionDailyCost, limits.DimensionMonthlyCost:
		// Both budgets keep one code. An exhausted budget says why spend a
		// dashboard reports as under the limit can still exhaust it: an attempt
		// nobody could price is charged nothing. A budget with room left says it
		// is the request that does not fit, which is what a client can change.
		code = "budget_exhausted"
		if estimate {
			message = "The API key cost budget cannot cover this request's estimated cost beside the spend and requests already counted against it. Lower max_tokens, wait for requests in flight to finish, or raise the budget."
		} else {
			message = "The API key cost budget was exhausted. Unpriced attempts accrue 0."
		}
	}
	return &Error{
		Status:     http.StatusTooManyRequests,
		Type:       "rate_limit_error",
		Code:       code,
		Message:    message,
		RetryAfter: retryHint(dimension, retryAfter),
	}
}

// retryHint bounds the wait advertised to the caller. A concurrency slot frees
// as soon as one in-flight request ends, so the lease TTL it reports would
// send a client away for far longer than it needs to wait, and every hint is
// at least a second because the header carries whole seconds.
func retryHint(dimension limits.Dimension, retryAfter time.Duration) time.Duration {
	if dimension == limits.DimensionConcurrency {
		retryAfter = min(retryAfter, maxConcurrencyRetryHint)
	}
	return max(retryAfter, time.Second)
}

// keyRequest describes the API key budgets as one admission decision. It is the
// one reservation whose allowance the caller is told, in the rate-limit headers,
// so it is the one that asks the rate script to state it: the quotas of a
// provider are not the caller's, and are answered with the decision alone.
func keyRequest(authority access.Authority, estimate int64, ttl time.Duration) limits.Request {
	policy := authority.Policy
	return limits.Request{
		CostOwnerID:       authority.ID,
		LookupID:          authority.LookupID,
		RequestsPerMinute: policy.RequestsPerMinute,
		TokensPerMinute:   policy.TokensPerMinute,
		MaxConcurrency:    policy.MaxConcurrency,
		DailyCostLimit:    policy.DailyCostLimit,
		MonthlyCostLimit:  policy.MonthlyCostLimit,
		RequestedTokens:   estimate,
		LeaseTTL:          ttl,
		ReportRate:        true,
	}
}

func groupRequest(authority access.Authority, ttl time.Duration) *limits.Request {
	if authority.BudgetGroupID == nil ||
		(authority.BudgetGroupDailyCostLimit == nil && authority.BudgetGroupMonthlyCostLimit == nil) {
		return nil
	}
	return &limits.Request{
		CostOwnerID:      *authority.BudgetGroupID,
		LookupID:         limits.BudgetGroupLookup(*authority.BudgetGroupID),
		DailyCostLimit:   authority.BudgetGroupDailyCostLimit,
		MonthlyCostLimit: authority.BudgetGroupMonthlyCostLimit,
		RequestedTokens:  0,
		LeaseTTL:         ttl,
	}
}

// reserveKey admits one request against the API key budgets. A nil lease with
// a nil error admits the request without one: either the key bounds nothing,
// or the limiter is unreachable and the installation chose to fail open.
func (a *Admission) reserveKey(ctx context.Context, authority access.Authority, estimate int64, ttl time.Duration) (*limits.Lease, *Error) {
	return a.reserveKeyCosted(ctx, authority, estimate, ttl, costReservation{})
}

// reserveKeyCosted admits one request against the API key budgets and, for a
// request that can be priced, reserves its estimated cost against the cost
// budgets of the key and its budget group beside the spend already accrued. The
// group's budget is taken first, since it is shared; its lease is attached to the
// key's, so the one handle the request keeps finishes both. A budget that
// refuses the request after the group's was taken gives the group's back.
func (a *Admission) reserveKeyCosted(ctx context.Context, authority access.Authority, estimate int64, ttl time.Duration, hold costReservation) (*limits.Lease, *Error) {
	var group *limits.Lease
	if request := groupRequest(authority, ttl); request != nil {
		if !a.ready() {
			return nil, limitsUnavailable()
		}
		request.CostEstimate, request.RequestID = hold.amount, hold.requestID
		decision, cancel := context.WithTimeout(ctx, reserveTimeout)
		lease, err := a.limiter.Reserve(decision, *request)
		cancel()
		if err != nil {
			if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
				a.recordRejection(exceeded.Dimension)
				return nil, rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate)
			}
			return nil, a.outage(authority.ID, true, err)
		}
		group = lease
	}
	request := keyRequest(authority, estimate, ttl)
	if !request.HasHardLimits() {
		// Nothing to enforce, so nothing to store: a key without hard limits
		// reaches Valkey only for the group it belongs to.
		return group, nil
	}
	if !a.ready() {
		return nil, limitsUnavailable()
	}
	if request.TokensPerMinute != nil && estimate > *request.TokensPerMinute {
		// No window will ever hold this request: answer now rather than make
		// the caller retry into a limit it cannot satisfy.
		settleKey(ctx, group, false, nil, a.logger())
		return nil, invalidRequest("request_exceeds_token_limit", "This request needs more tokens than the API key tokens per minute limit allows.", nil)
	}
	if request.HasCostBudget() {
		request.CostEstimate, request.RequestID = hold.amount, hold.requestID
	}
	decision, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	lease, err := a.limiter.Reserve(decision, request)
	if err == nil {
		lease.Attach(group)
		return lease, nil
	}
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		a.recordRejection(exceeded.Dimension)
		settleKey(ctx, group, false, nil, a.logger())
		// Only the key's own request and token limits state an allowance, and
		// only its rejection reports it: a budget group has none, and the quota
		// of a provider is not the caller's.
		e := rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate)
		e.rate = exceeded.Rate
		return nil, e
	}
	if e := a.outage(authority.ID, request.HasCostBudget(), err); e != nil {
		settleKey(ctx, group, false, nil, a.logger())
		return nil, e
	}
	// Failing open admits the request without the key's own reservation, but the
	// group's was taken and still has to be settled.
	return group, nil
}

// outage decides what happens to a request whose budgets cannot be consulted.
// Spending against an unknown balance can never be undone, so a cost budget
// always fails closed; rate and concurrency limits follow the policy the
// installation configured.
func (a *Admission) outage(keyID string, costBudget bool, cause error) *Error {
	var service *limits.ServiceError
	if !errors.As(cause, &service) || costBudget || a == nil || a.policy == nil || a.policy() != limits.FailOpen {
		return limitsUnavailable()
	}
	a.failOpen.Add(1)
	a.logger().Warn("admitting request without distributed limits", "api_key_id", keyID, "error", cause.Error())
	return nil
}

// settleKey finishes the key reservation. A request that never dispatched an
// attempt consumed nothing and is refunded in full; any other request keeps
// the request and token it spent but must return the concurrency slot, and
// reports the tokens it actually used when the provider disclosed them. The cost
// it reserved becomes the cost it recorded. That is settled last, and a cost is
// settled once, because it is the step whose loss costs least: the reservation
// then lapses on its own and is removed sooner by the spend accounting records
// for the request. A request that never dispatched, and one that recorded no cost
// because its usage is still to come, have no spend to remove their reservation,
// so it is given back by a release, which is retried like the others.
//
// The caller's context may already be cancelled — the client may have hung up,
// which is exactly when the slot has to come back — so cancellation is dropped
// and every step is retried on the caller's behalf by the limiter.
func settleKey(ctx context.Context, lease *limits.Lease, dispatched bool, actual *int64, log *slog.Logger) {
	if lease == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if !dispatched {
		if err := lease.Refund(ctx); err != nil {
			log.Warn("limit reservation refund failed", "error", err.Error())
		}
		return
	}
	if actual != nil {
		if err := lease.Reconcile(ctx, *actual); err != nil {
			log.Warn("limit reservation reconcile failed", "error", err.Error())
		}
	}
	if err := lease.Release(ctx); err != nil {
		log.Warn("limit reservation release failed", "error", err.Error())
	}
	if err := lease.SettleCost(ctx); err != nil {
		log.Warn("cost reservation settlement failed", "error", err.Error())
	}
}

// targetReservation holds what one attempt reserved on its provider.
type targetReservation struct {
	connection *limits.Lease
	slot       *limits.Lease
	log        *slog.Logger
}

// settle reconciles and releases both leases once the attempt has ended.
func (t *targetReservation) settle(ctx context.Context, dispatched bool, actual *int64) {
	if t == nil {
		return
	}
	settleKey(ctx, t.connection, dispatched, actual, t.log)
	settleKey(ctx, t.slot, dispatched, actual, t.log)
}

// connectionRequest describes the quota shared by every attempt that flows
// through one provider connection.
func connectionRequest(provider *runtime.Provider, estimate int64, ttl time.Duration) limits.Request {
	request := limits.Request{
		CostOwnerID:     provider.ID,
		RequestedTokens: estimate,
		LeaseTTL:        ttl,
	}
	if provider.Limits != nil {
		request.RequestsPerMinute = provider.Limits.RequestsPerMinute
		request.TokensPerMinute = provider.Limits.TokensPerMinute
		request.MaxConcurrency = provider.Limits.MaxConcurrency
	}
	// Naming the quota allocates, so a provider that has none is not made to.
	if request.HasHardLimits() {
		request.LookupID = limits.ConnectionLookup(provider.ID)
	}
	return request
}

// slotRequest describes the quota of one credential slot.
func slotRequest(slot *runtime.Slot, estimate int64, ttl time.Duration) limits.Request {
	request := limits.Request{
		CostOwnerID:       slot.ID,
		RequestsPerMinute: slot.RequestsPerMinute,
		TokensPerMinute:   slot.TokensPerMinute,
		MaxConcurrency:    slot.MaxConcurrency,
		RequestedTokens:   estimate,
		LeaseTTL:          ttl,
	}
	if request.HasHardLimits() {
		request.LookupID = limits.SlotLookup(slot.ID)
	}
	return request
}

// reserveTarget admits one attempt against the provider connection quota and
// then the credential slot quota. It returns the reservation the attempt must
// settle, or a rejection to record as a failed attempt, or skip when a quota
// is configured but cannot be consulted: an unmeterable target is passed over
// so a sibling can serve, never used unmetered.
func (a *Admission) reserveTarget(ctx context.Context, provider *runtime.Provider, slot *runtime.Slot, estimate int64, ttl time.Duration) (reservation *targetReservation, rejection *attemptFailure, skip bool) {
	connection := connectionRequest(provider, estimate, ttl)
	credential := slotRequest(slot, estimate, ttl)
	if !connection.HasHardLimits() && !credential.HasHardLimits() {
		return nil, nil, false
	}
	if !a.ready() {
		a.logger().Warn("skipping target with unenforceable quotas", "provider_id", provider.ID, "slot_id", slot.ID)
		return nil, nil, true
	}
	reservation = &targetReservation{log: a.logger()}
	for _, step := range [...]struct {
		request limits.Request
		lease   **limits.Lease
		quota   string
	}{{connection, &reservation.connection, quotaConnection}, {credential, &reservation.slot, quotaSlot}} {
		if !step.request.HasHardLimits() {
			continue
		}
		decision, cancel := context.WithTimeout(ctx, reserveTimeout)
		lease, err := a.limiter.Reserve(decision, step.request)
		cancel()
		if err == nil {
			*step.lease = lease
			continue
		}
		// Whatever was taken for this attempt is given back before the target
		// is abandoned: the attempt never happened.
		settleTargetRefund(ctx, reservation)
		if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
			return nil, &attemptFailure{class: classRateLimit, quota: step.quota, retryAfter: retryHint(exceeded.Dimension, exceeded.RetryAfter)}, false
		}
		a.logger().Warn("skipping target with unenforceable quotas", "provider_id", provider.ID, "slot_id", slot.ID, "error", err.Error())
		return nil, nil, true
	}
	return reservation, nil, false
}

// settleTargetRefund returns everything a partially reserved attempt took.
func settleTargetRefund(ctx context.Context, reservation *targetReservation) {
	settleKey(ctx, reservation.connection, false, nil, reservation.log)
	settleKey(ctx, reservation.slot, false, nil, reservation.log)
}

// cooldown records a shared cooldown for the credential and for the slot, so
// every replica avoids a target the upstream just rejected instead of each
// learning it alone.
func (a *Admission) cooldown(ctx context.Context, providerID string, slot *runtime.Slot, generation int64, d time.Duration, credential bool) {
	if !a.ready() || d <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coordinationTimeout)
	defer cancel()
	scope := limits.SlotScope(slot.ID)
	if credential {
		scope = limits.CredentialScope(providerID, slot.CredentialID, generation)
	}
	{
		if err := a.limiter.Cooldown(ctx, scope, d); err != nil {
			a.logger().Warn("shared cooldown not recorded", "provider_id", providerID, "slot_id", slot.ID, "error", err.Error())
		}
	}
}

// endCredentialCooldown ends a credential version's shared cooldown before it
// runs out, for every replica.
func (a *Admission) endCredentialCooldown(ctx context.Context, providerID, credentialID string, generation int64) {
	if !a.ready() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coordinationTimeout)
	defer cancel()
	if err := a.limiter.Cooldown(ctx, limits.CredentialScope(providerID, &credentialID, generation), 0); err != nil {
		a.logger().Warn("shared cooldown not ended", "provider_id", providerID, "credential_id", credentialID, "error", err.Error())
	}
}

// cooling reports whether another replica put this credential or slot in a
// cooldown. A store that cannot answer must not take every slot out of
// service, so an unreadable cooldown is treated as absent and logged.
func (a *Admission) cooling(ctx context.Context, providerID string, slot *runtime.Slot, generation int64) bool {
	if !a.ready() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, coordinationTimeout)
	defer cancel()
	cooling, err := a.limiter.Cooling(ctx, limits.CredentialScope(providerID, slot.CredentialID, generation), limits.SlotScope(slot.ID))
	if err != nil {
		a.logger().Warn("shared cooldown not read", "provider_id", providerID, "slot_id", slot.ID, "error", err.Error())
		return false
	}
	return cooling
}

// maxEstimate is the largest integer the limiter can store.
const maxEstimate = estimate.MaxTokens

// requestEstimate covers any provider the pinned route may select. Connection
// defaults affect what is actually sent, even when the client omitted a bound.
// Each attempt is priced once, for its own provider and model, so the cost
// grows with the targets of a route and not with their square.
func requestEstimate(x *execution) int64 {
	var reserved int64
	for _, attempt := range x.attempts {
		provider := x.snapshot().Providers[attempt.ProviderID]
		reserved = max(reserved, x.attemptReservation(attempt, &provider))
	}
	return max(reserved, 1)
}

// keyReservationEstimate covers every upstream attempt the route may dispatch.
// Settlement charges every attempt, including a conservative estimate when an
// upstream may have billed work without reporting usage, so admission must hold
// the same worst-case capacity before any provider is called.
func keyReservationEstimate(perAttempt int64, attempts int) int64 {
	return multiplyBounded(perAttempt, int64(max(attempts, 1)))
}

// settledTokens accounts for every attempted provider, not just the last one.
// Uncertain attempts retain an estimate rather than refunding unknown work.
func (x *execution) settledTokens() *int64 {
	var total int64
	for _, fact := range x.facts {
		actual := totalTokens(fact.Usage)
		switch {
		case actual != nil:
			total = addBounded(total, *actual)
		case fact.BillingUncertain || fact.UsageObserved:
			total = addBounded(total, x.estimate)
		}
	}
	return &total
}

// multiplyBounded and addBounded saturate at the largest integer the limiter
// accepts, so an absurd request is rejected as too large for the key's window
// rather than failing the reservation as malformed.
func multiplyBounded(a, b int64) int64 {
	if a > maxEstimate/b {
		return maxEstimate
	}
	return a * b
}

func addBounded(a, b int64) int64 {
	if a > maxEstimate-b {
		return maxEstimate
	}
	return a + b
}

// totalTokens is the usage an attempt disclosed, as one number to reconcile a
// token reservation against. Providers that report only the two halves are
// summed; a provider that reported nothing leaves the estimate standing.
func totalTokens(usage *openai.Usage) *int64 {
	if usage == nil {
		return nil
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return nil
	}
	// Do not overflow before saturating, or trust a contradictory smaller
	// total over the input and output the same response reported.
	total := addBounded(min(usage.InputTokens, maxEstimate), min(usage.OutputTokens, maxEstimate))
	total = max(total, min(usage.TotalTokens, maxEstimate))
	return &total
}
