// Package limits admits requests against the request, token, concurrency and
// cost budgets that every replica shares through Valkey.
//
// Valkey is the only clock and the only authority: the Lua scripts in scripts/
// decide atomically from the server's own TIME, so a rejection mutates nothing,
// a granted reservation stays refundable and reconcilable by lease ID, and any
// stored state a script cannot interpret fails the request closed instead of
// admitting it on a zero.
package limits

import (
	"context"
	_ "embed"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/coordination"
)

//go:embed scripts/reserve_limits.lua
var reserveLimitsScript string

//go:embed scripts/reconcile_limits.lua
var reconcileLimitsScript string

//go:embed scripts/refund_limits.lua
var refundLimitsScript string

//go:embed scripts/release_concurrency.lua
var releaseConcurrencyScript string

//go:embed scripts/reserve_cost.lua
var reserveCostScript string

//go:embed scripts/reconcile_cost.lua
var reconcileCostScript string

//go:embed scripts/provider_usage.lua
var providerUsageScript string

//go:embed scripts/provider_cooldown.lua
var providerCooldownScript string

const (
	// MaxCounter is the largest integer Lua 5.1 represents exactly. Every
	// counter and identifier the scripts handle must stay within it, so values
	// beyond it are rejected before they reach Valkey.
	MaxCounter    = int64(1)<<53 - 1
	maxLuaInteger = MaxCounter
	// scriptResponseVersion is the reply contract the scripts and this package
	// share. A different version means the deployed script is not this one.
	scriptResponseVersion = 1
	fixedWindowMS         = int64(60_000)
	dayMS                 = int64(86_400_000)
	// maxMonthMS bounds a monthly retry hint by the longest civil month.
	maxMonthMS = 31 * dayMS
	// maxCooldownMS caps a provider cooldown at one day.
	maxCooldownMS = dayMS
	// Cleanup runs after the decision it undoes, so it is bounded tightly and
	// retried: every cleanup script is idempotent per lease.
	cleanupAttempts = 3
	cleanupTimeout  = 250 * time.Millisecond
	cleanupBackoff  = 25 * time.Millisecond
	// nilUUID stands in for an absent credential version in a cooldown scope.
	nilUUID = "00000000-0000-0000-0000-000000000000"
)

// Dimension names the budget that rejected a reservation.
type Dimension string

// The dimensions a reservation can exhaust.
const (
	DimensionRequests    Dimension = "requests"
	DimensionTokens      Dimension = "tokens"
	DimensionConcurrency Dimension = "concurrency"
	DimensionDailyCost   Dimension = "daily_cost"
	DimensionMonthlyCost Dimension = "monthly_cost"
)

var (
	// ErrMalformedState reports stored state that no script would interpret.
	// The request fails closed and the state is repaired by reconciliation
	// rather than by guessing a counter here.
	ErrMalformedState = errors.New("distributed limit state is malformed")
	// ErrUnexpectedResponse reports a reply that does not match the script
	// contract, which means the deployed script is not the one embedded here.
	ErrUnexpectedResponse = errors.New("distributed limit script returned an unexpected response")
	// ErrUninitializedCost reports cost state that PostgreSQL has not
	// reconciled for the current UTC window yet. Spending cannot be admitted
	// against an unknown balance.
	ErrUninitializedCost = errors.New("cost state awaits PostgreSQL reconciliation for the current UTC window")
)

// ServiceError reports that Valkey could not be reached or refused a command.
// The admission decision is unknown, so callers apply the outage policy.
type ServiceError struct{ Err error }

func (e *ServiceError) Error() string { return "distributed limit service failed: " + e.Err.Error() }

// Unwrap exposes the transport failure, for example a coordination.CommandError
// whose Ambiguous flag says whether the command may still have run.
func (e *ServiceError) Unwrap() error { return e.Err }

// ExceededError reports a budget that rejected the request. RetryAfter comes
// from the script, which derived it from the server clock.
type ExceededError struct {
	Dimension  Dimension
	RetryAfter time.Duration
}

func (e *ExceededError) Error() string {
	return string(e.Dimension) + " limit exceeded, retry after " + e.RetryAfter.String()
}

// InvalidRequestError reports a reservation this package refused to send.
type InvalidRequestError struct{ Reason string }

func (e *InvalidRequestError) Error() string { return e.Reason }

// Request describes one admission decision. A nil limit means that dimension is
// unlimited and no state is created for it.
type Request struct {
	// CostOwnerID identifies the cost budget owner. Provider connections and slots
	// pass their own identifier: cost keys are only used when a cost limit is set.
	CostOwnerID string
	// LookupID names the shared rate and concurrency counters.
	LookupID          string
	RequestsPerMinute *int64
	TokensPerMinute   *int64
	MaxConcurrency    *int64
	// DailyCostLimit and MonthlyCostLimit are canonical decimal strings.
	DailyCostLimit   *string
	MonthlyCostLimit *string
	// RequestedTokens is the estimate reserved up front and later reconciled
	// against the tokens the attempt actually used.
	RequestedTokens int64
	// LeaseTTL bounds how long a concurrency slot survives without a release.
	LeaseTTL time.Duration
}

// HasHardLimits reports whether any budget applies. A request without one needs
// no reservation at all.
func (r Request) HasHardLimits() bool {
	return r.RequestsPerMinute != nil || r.TokensPerMinute != nil || r.MaxConcurrency != nil ||
		r.HasCostBudget()
}

// HasCostBudget reports whether a cost limit applies, which is what makes the
// reservation depend on PostgreSQL-reconciled state.
func (r Request) HasCostBudget() bool {
	return r.DailyCostLimit != nil || r.MonthlyCostLimit != nil
}

// Validate rejects a request the scripts could not enforce exactly. It returns
// InvalidRequestError.
func (r Request) Validate() error {
	if !validLookup(r.LookupID) {
		return &InvalidRequestError{Reason: "lookup ID must be 8-40 ASCII letters, digits, or underscores"}
	}
	if _, err := uuid.Parse(r.CostOwnerID); err != nil {
		return &InvalidRequestError{Reason: "cost owner ID must be a UUID"}
	}
	for _, limit := range [...]struct {
		value *int64
		name  string
	}{
		{r.RequestsPerMinute, "requests per minute"},
		{r.TokensPerMinute, "tokens per minute"},
		{r.MaxConcurrency, "max concurrency"},
	} {
		if limit.value == nil {
			continue
		}
		if *limit.value < 1 {
			return &InvalidRequestError{Reason: limit.name + " must be positive"}
		}
		if *limit.value > maxLuaInteger {
			return &InvalidRequestError{Reason: limit.name + " exceeds the Valkey Lua integer range"}
		}
	}
	if r.RequestedTokens < 0 {
		return &InvalidRequestError{Reason: "requested tokens cannot be negative"}
	}
	if r.RequestedTokens > maxLuaInteger {
		return &InvalidRequestError{Reason: "requested tokens exceed the Valkey Lua integer range"}
	}
	if r.TokensPerMinute != nil && r.RequestedTokens < 1 {
		return &InvalidRequestError{Reason: "requested tokens must be positive when a token limit applies"}
	}
	for _, limit := range [...]struct {
		value *string
		name  string
	}{{r.DailyCostLimit, "daily"}, {r.MonthlyCostLimit, "monthly"}} {
		if limit.value != nil && !ValidCostLimit(*limit.value) {
			return &InvalidRequestError{
				Reason: limit.name + " cost limit must be a positive decimal with at most 12 integer and 12 fractional digits",
			}
		}
	}
	// The script measures a lease in whole milliseconds and refuses a zero TTL,
	// so a sub-millisecond TTL is rejected here with an explanation. No upper
	// bound is needed: a Go duration counts nanoseconds in an int64, so its
	// millisecond count can never leave the Lua range.
	if r.LeaseTTL.Milliseconds() < 1 {
		return &InvalidRequestError{Reason: "lease TTL must be at least one millisecond"}
	}
	return nil
}

// ValidCostLimit reports whether value is a positive limit the scripts compare
// exactly: at most twelve integer and twelve fractional digits, no sign and no
// exponent.
func ValidCostLimit(value string) bool {
	integer, fraction := value, ""
	if before, after, ok := strings.Cut(value, "."); ok {
		integer, fraction = before, after
		if len(fraction) < 1 || len(fraction) > 12 {
			return false
		}
	}
	if len(integer) < 1 || len(integer) > 12 {
		return false
	}
	nonzero := false
	for _, part := range [...]string{integer, fraction} {
		for index := 0; index < len(part); index++ {
			if part[index] < '0' || part[index] > '9' {
				return false
			}
			nonzero = nonzero || part[index] != '0'
		}
	}
	return nonzero
}

// validLookup mirrors the character set that keeps a lookup safe inside a
// Valkey Cluster hash tag.
func validLookup(lookup string) bool {
	if len(lookup) < 8 || len(lookup) > 40 {
		return false
	}
	for index := 0; index < len(lookup); index++ {
		c := lookup[index]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		default:
			return false
		}
	}
	return true
}

// Limiter reserves shared budgets through one Valkey connection.
type Limiter struct {
	client    *coordination.Client
	namespace string
}

// New builds a limiter over client. The namespace prefixes every key and must
// not introduce a second Valkey Cluster hash tag.
func New(client *coordination.Client, namespace string) (*Limiter, error) {
	if client == nil {
		return nil, errors.New("limits: a Valkey client is required")
	}
	if len(namespace) < 1 || len(namespace) > 128 {
		return nil, errors.New("limits: namespace must be 1-128 characters")
	}
	for index := 0; index < len(namespace); index++ {
		c := namespace[index]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c == ':', c == '_', c == '-':
		default:
			return nil, errors.New("limits: namespace may only contain ASCII letters, digits, ':', '_' and '-'")
		}
	}
	return &Limiter{client: client, namespace: namespace}, nil
}

// keys holds the four keys one request can touch. The rate and concurrency keys
// share the lookup as their cluster hash tag so one script sees both; the cost
// keys share the cost owner ID so every lookup of one owner meets the same balance.
type keys struct {
	rate        string
	concurrency string
	dailyCost   string
	monthlyCost string
}

func (l *Limiter) keysFor(lookupID, costOwnerID string) keys {
	rate, concurrency := l.rateKeys(lookupID)
	daily, monthly := l.costKeys(costOwnerID)
	return keys{rate: rate, concurrency: concurrency, dailyCost: daily, monthlyCost: monthly}
}

// rateKeys addresses the counters one lookup shares across replicas.
func (l *Limiter) rateKeys(lookupID string) (string, string) {
	tagged := l.namespace + ":{" + lookupID + "}"
	return tagged + ":rate", tagged + ":concurrency:v2"
}

// simpleUUID renders a UUID without dashes so it is safe inside a hash tag. A
// value that is not a UUID is returned unchanged and fails validation later.
func simpleUUID(value string) string {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return value
	}
	return strings.ReplaceAll(parsed.String(), "-", "")
}

// ConnectionLookup names the quota shared by every attempt through one provider
// connection.
func ConnectionLookup(providerID string) string { return "pc_" + simpleUUID(providerID) }

// SlotLookup names the quota shared by every attempt through one provider slot.
func SlotLookup(slotID string) string { return "ps_" + simpleUUID(slotID) }

func BudgetGroupLookup(id string) string { return "bg_" + simpleUUID(id) }

// CredentialScope names the cooldown that follows one credential version, so a
// rotation is not punished for the version it replaced.
func CredentialScope(providerID string, credentialID *string) string {
	version := nilUUID
	if credentialID != nil {
		version = *credentialID
	}
	return canonicalUUID(providerID) + ":" + canonicalUUID(version)
}

// SlotScope names the cooldown that follows one provider slot.
func SlotScope(slotID string) string { return "slot:" + canonicalUUID(slotID) }

// canonicalUUID normalises a UUID so replicas that format it differently still
// address the same cooldown key.
func canonicalUUID(value string) string {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return value
	}
	return parsed.String()
}

// eval runs a script and reports transport failures as ServiceError, which is
// what separates "rejected" from "unknown".
func (l *Limiter) eval(ctx context.Context, script string, scriptKeys []string, args ...string) (any, error) {
	command := make([]string, 0, 3+len(scriptKeys)+len(args))
	command = append(command, "EVAL", script, strconv.Itoa(len(scriptKeys)))
	command = append(command, scriptKeys...)
	command = append(command, args...)
	value, err := l.client.Do(ctx, command...)
	if err != nil {
		return nil, &ServiceError{Err: err}
	}
	return value, nil
}

// Reserve admits one request against every configured budget. Cost is settled
// first because it is the budget a caller cannot undo by refunding a lease.
// It returns ExceededError when a budget rejects the request, ErrUninitializedCost
// when cost state is not reconciled yet, ErrMalformedState when stored state is
// unusable, and ServiceError when Valkey is unreachable.
func (l *Limiter) Reserve(ctx context.Context, r Request) (*Lease, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	scriptKeys := l.keysFor(r.LookupID, r.CostOwnerID)
	if r.HasCostBudget() {
		if err := l.reserveCost(ctx, r, scriptKeys); err != nil {
			return nil, err
		}
	}
	leaseID := uuid.Must(uuid.NewV7()).String()
	value, err := l.eval(ctx, reserveLimitsScript,
		[]string{scriptKeys.rate, scriptKeys.concurrency},
		optionalLimit(r.RequestsPerMinute), optionalLimit(r.TokensPerMinute),
		strconv.FormatInt(r.RequestedTokens, 10), optionalLimit(r.MaxConcurrency),
		leaseID, strconv.FormatInt(r.LeaseTTL.Milliseconds(), 10))
	if err != nil {
		return nil, err
	}
	result, err := parseReservation(value)
	if err != nil {
		return nil, err
	}
	switch result.kind {
	case resultGranted:
		// A concurrency lease is reported exactly when one was requested. Any
		// other pairing means the reply does not describe this request.
		if (r.MaxConcurrency != nil) != (result.leaseExpiresAtMS > 0) {
			return nil, ErrUnexpectedResponse
		}
		return &Lease{
			limiter:                l,
			id:                     leaseID,
			rateKey:                scriptKeys.rate,
			concurrencyKey:         scriptKeys.concurrency,
			windowID:               result.windowID,
			reservedTokens:         r.RequestedTokens,
			hasToken:               r.TokensPerMinute != nil,
			hasRequest:             r.RequestsPerMinute != nil,
			concurrencyExpiresAtMS: result.leaseExpiresAtMS,
		}, nil
	case resultRejected:
		return nil, &ExceededError{
			Dimension:  result.dimension,
			RetryAfter: time.Duration(result.retryAfterMS) * time.Millisecond,
		}
	case resultMalformed:
		return nil, ErrMalformedState
	default:
		return nil, ErrUnexpectedResponse
	}
}

// reserveCost checks the daily and monthly balances. It never writes an accrued
// total: only PostgreSQL reconciliation may do that.
func (l *Limiter) reserveCost(ctx context.Context, r Request, scriptKeys keys) error {
	daily, monthly := "", ""
	if r.DailyCostLimit != nil {
		daily = *r.DailyCostLimit
	}
	if r.MonthlyCostLimit != nil {
		monthly = *r.MonthlyCostLimit
	}
	value, err := l.eval(ctx, reserveCostScript,
		[]string{scriptKeys.dailyCost, scriptKeys.monthlyCost}, daily, monthly, "0")
	if err != nil {
		return err
	}
	result, err := parseCostReservation(value)
	if err != nil {
		return err
	}
	switch result.kind {
	case resultGranted:
		return nil
	case resultRejected:
		return &ExceededError{
			Dimension:  result.dimension,
			RetryAfter: time.Duration(result.retryAfterMS) * time.Millisecond,
		}
	case resultUninitialized:
		return ErrUninitializedCost
	case resultMalformed:
		return ErrMalformedState
	default:
		return ErrUnexpectedResponse
	}
}

// optionalLimit renders an absent limit as the script's "unlimited" zero.
func optionalLimit(limit *int64) string {
	if limit == nil {
		return "0"
	}
	return strconv.FormatInt(*limit, 10)
}

// Lease is a granted reservation. Exactly one of Refund, Reconcile or Release
// finishes it, and each may be called repeatedly: the scripts are idempotent
// per lease.
type Lease struct {
	limiter                *Limiter
	id                     string
	rateKey                string
	concurrencyKey         string
	windowID               int64
	reservedTokens         int64
	hasToken               bool
	hasRequest             bool
	concurrencyExpiresAtMS int64
}

// Refund returns an admission that never dispatched. It only applies inside the
// minute that granted it, so it can never take capacity from a later window.
func (le *Lease) Refund(ctx context.Context) error {
	tokens := int64(0)
	if le.hasToken {
		tokens = le.reservedTokens
	}
	requests := "0"
	if le.hasRequest {
		requests = "1"
	}
	return cleanup(ctx, func(ctx context.Context) error {
		_, err := le.limiter.eval(ctx, refundLimitsScript,
			[]string{le.rateKey, le.concurrencyKey},
			strconv.FormatInt(le.windowID, 10), requests,
			strconv.FormatInt(tokens, 10), le.id)
		return err
	})
}

// Reconcile settles the token reservation against the tokens actually used. It
// is a no-op without a token budget, and the script ignores a window that has
// already rolled over.
func (le *Lease) Reconcile(ctx context.Context, actualTokens int64) error {
	if !le.hasToken {
		return nil
	}
	if actualTokens < 0 || actualTokens > maxLuaInteger {
		return &InvalidRequestError{Reason: "actual tokens must be a non-negative Lua-safe integer"}
	}
	adjustment := actualTokens - le.reservedTokens
	if adjustment == 0 {
		return nil
	}
	return cleanup(ctx, func(ctx context.Context) error {
		_, err := le.limiter.eval(ctx, reconcileLimitsScript, []string{le.rateKey},
			strconv.FormatInt(le.windowID, 10), strconv.FormatInt(adjustment, 10), le.id)
		return err
	})
}

// Release frees the concurrency slot. Requests and tokens stay consumed: the
// attempt did happen.
func (le *Lease) Release(ctx context.Context) error {
	if le.concurrencyExpiresAtMS == 0 {
		return nil
	}
	return cleanup(ctx, func(ctx context.Context) error {
		_, err := le.limiter.eval(ctx, releaseConcurrencyScript, []string{le.concurrencyKey}, le.id)
		return err
	})
}

// cleanup runs a lease mutation that must outlive the request it belongs to:
// the caller's cancellation is dropped, every attempt is bounded, and an
// ambiguous transport failure is retried because the scripts are idempotent.
func cleanup(ctx context.Context, run func(context.Context) error) error {
	ctx = context.WithoutCancel(ctx)
	var err error
	for attempt := range cleanupAttempts {
		if attempt > 0 {
			time.Sleep(cleanupBackoff << (attempt - 1))
		}
		attemptCtx, cancel := context.WithTimeout(ctx, cleanupTimeout)
		err = run(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
	}
	return err
}

// Usage is the live quota consumption of one lookup.
type Usage struct {
	RequestsThisMinute int64
	TokensThisMinute   int64
	ConcurrentRequests int64
}

// ProviderUsage reports what a provider lookup has consumed in the current
// server minute, counting only leases that have not expired.
func (l *Limiter) ProviderUsage(ctx context.Context, lookup string) (Usage, error) {
	if !validLookup(lookup) {
		return Usage{}, &InvalidRequestError{Reason: "lookup ID must be 8-40 ASCII letters, digits, or underscores"}
	}
	rate, concurrency := l.rateKeys(lookup)
	value, err := l.eval(ctx, providerUsageScript, []string{rate, concurrency})
	if err != nil {
		return Usage{}, err
	}
	items, ok := replyItems(value, 3)
	if !ok {
		return Usage{}, ErrUnexpectedResponse
	}
	counters := [3]int64{}
	for index := range counters {
		counter, ok := replyInt(items[index])
		if !ok || counter < 0 || counter > maxLuaInteger {
			return Usage{}, ErrUnexpectedResponse
		}
		counters[index] = counter
	}
	return Usage{
		RequestsThisMinute: counters[0],
		TokensThisMinute:   counters[1],
		ConcurrentRequests: counters[2],
	}, nil
}

// Cooldown parks a scope for d, extending an existing cooldown but never
// shortening one. A duration of zero or less clears the cooldown instead.
func (l *Limiter) Cooldown(ctx context.Context, scope string, d time.Duration) error {
	key := l.cooldownKey(scope)
	if d <= 0 {
		if _, err := l.client.Do(ctx, "DEL", key); err != nil {
			return &ServiceError{Err: err}
		}
		return nil
	}
	milliseconds := min(max(d.Milliseconds(), 1), maxCooldownMS)
	_, err := l.eval(ctx, providerCooldownScript, []string{key},
		strconv.FormatInt(milliseconds, 10))
	return err
}

// Cooling reports whether any of the scopes is still parked.
func (l *Limiter) Cooling(ctx context.Context, scopes ...string) (bool, error) {
	if len(scopes) == 0 {
		return false, nil
	}
	command := make([]string, 0, 1+len(scopes))
	command = append(command, "EXISTS")
	for _, scope := range scopes {
		command = append(command, l.cooldownKey(scope))
	}
	value, err := l.client.Do(ctx, command...)
	if err != nil {
		return false, &ServiceError{Err: err}
	}
	existing, ok := replyInt(value)
	if !ok || existing < 0 {
		return false, ErrUnexpectedResponse
	}
	return existing > 0, nil
}

func (l *Limiter) cooldownKey(scope string) string {
	return l.namespace + ":provider-cooldown:" + scope
}

// Ping reports whether the limiter still has a usable Valkey connection.
func (l *Limiter) Ping(ctx context.Context) error {
	value, err := l.client.Do(ctx, "PING")
	if err != nil {
		return &ServiceError{Err: err}
	}
	if reply, ok := value.(string); !ok || reply != "PONG" {
		return ErrUnexpectedResponse
	}
	return nil
}
