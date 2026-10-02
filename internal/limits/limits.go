// Package limits admits requests against the request, token, concurrency and
// cost budgets that every replica shares through Valkey.
//
// Valkey is the only clock and the only authority: the Lua scripts in scripts/
// decide atomically from the server's own TIME, so a rejection mutates no
// counter, a granted reservation stays refundable and reconcilable by lease ID,
// and any stored state a script cannot interpret fails the request closed
// instead of admitting it on a zero. A caller that asks, with Request.ReportRate,
// has the rate script's answer also state what is left of the request and token
// allowances, for a grant and for a rejection alike, so it can report them with
// no second round trip. A caller that does not ask is answered with the decision
// alone and pays nothing for them.
//
// A cost budget is measured against two quantities. The accrued spend is
// PostgreSQL's, installed by ApplyCostSnapshot and never lowered. The pending
// spend is what requests still in flight may cost: each priced request reserves
// its estimate at admission, replaces it with what it cost when it ends, and has
// it removed in the same step that installs its accrued spend. Pending state is
// advisory and derived, so a reservation lapses on its own and a damaged one is
// discarded rather than allowed to block accounting.
package limits

import (
	"context"
	"crypto/sha1"
	_ "embed"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/coordination"
)

//go:embed scripts/reserve_limits.lua
var reserveLimitsSource string

//go:embed scripts/reconcile_limits.lua
var reconcileLimitsSource string

//go:embed scripts/refund_limits.lua
var refundLimitsSource string

//go:embed scripts/release_concurrency.lua
var releaseConcurrencySource string

//go:embed scripts/reserve_cost.lua
var reserveCostSource string

//go:embed scripts/reconcile_cost.lua
var reconcileCostSource string

//go:embed scripts/settle_cost.lua
var settleCostSource string

//go:embed scripts/provider_usage.lua
var providerUsageSource string

//go:embed scripts/provider_cooldown.lua
var providerCooldownSource string

// script is an embedded Lua script with the SHA-1 digest Valkey caches it under.
// A call names the digest and sends the source only to a server that does not
// hold it yet: the cost scripts are far larger than the arguments they run with.
type script struct{ source, digest string }

func newScript(source string) script {
	sum := sha1.Sum([]byte(source))
	return script{source: source, digest: hex.EncodeToString(sum[:])}
}

var (
	reserveLimitsScript      = newScript(reserveLimitsSource)
	reconcileLimitsScript    = newScript(reconcileLimitsSource)
	refundLimitsScript       = newScript(refundLimitsSource)
	releaseConcurrencyScript = newScript(releaseConcurrencySource)
	reserveCostScript        = newScript(reserveCostSource)
	reconcileCostScript      = newScript(reconcileCostSource)
	settleCostScript         = newScript(settleCostSource)
	providerUsageScript      = newScript(providerUsageSource)
	providerCooldownScript   = newScript(providerCooldownSource)
)

const (
	// MaxCounter is the largest integer Lua 5.1 represents exactly. Every
	// counter and identifier the scripts handle must stay within it, so values
	// beyond it are rejected before they reach Valkey.
	MaxCounter    = int64(1)<<53 - 1
	maxLuaInteger = MaxCounter
	// limitsResponseVersion and costResponseVersion are the reply contracts the
	// rate and cost scripts share with this package. A different version means
	// the deployed script is not this one. They advance independently: a change
	// to what the rate script answers must not invalidate the cost scripts.
	limitsResponseVersion = 2
	costResponseVersion   = 1
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
	// DefaultCostGrace is how long a cost reservation outlives the request that
	// made it when the caller names no grace. Accounting normally removes it
	// within seconds; this is the bound when it cannot, sized to cover a consumer
	// that crashed and was recovered or a PostgreSQL failover.
	DefaultCostGrace = 5 * time.Minute
	// settleTimeout bounds the one attempt SettleCost makes to replace an estimate
	// with a cost. That runs as a request ends and is the cheapest step to lose:
	// the reservation made at admission still lapses on its own, and the spend
	// accounting records for the request removes it sooner. Giving an estimate
	// back, because the request cost nothing or never ran, is not that: nothing
	// else is coming to remove it, so it is retried within cleanupTimeout like
	// every other release.
	settleTimeout = 100 * time.Millisecond
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
	// Estimate is set when a cost budget refused the request although it is not
	// spent: what has accrued and what requests in flight hold leave no room for
	// this request's estimate. Lowering the estimate, or waiting for requests in
	// flight to end, can admit it; neither helps a budget that is exhausted.
	Estimate bool
	// Rate is the request and token allowance the rate script measured the request
	// against, for a rejection that script made of a Request that asked for it
	// with ReportRate: nothing was reserved, so it is what the window still holds.
	// It is zero when a cost budget refused the request, because the script never
	// ran, when the request did not ask, and for a key that limits neither
	// requests nor tokens.
	Rate RateState
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
	// CostEstimate is the most this request may cost, a canonical decimal string,
	// reserved against the cost budget beside the spend already accrued. Empty
	// reserves nothing and judges the request on accrued spend alone, which is
	// what a request nobody can price gets.
	CostEstimate string
	// RequestID names the cost reservation. It is the identifier accounting later
	// carries on the spend it records for the request, which is how that spend
	// replaces the reservation instead of counting beside it. Required with a
	// CostEstimate.
	RequestID string
	// CostGrace is how long past LeaseTTL the reservation survives if nothing
	// settles it. Zero means DefaultCostGrace.
	CostGrace time.Duration
	// ReportRate asks the rate script to state the request and token allowance it
	// measured the request against, which the lease's RateState and the refusal's
	// Rate then report. Only a caller that reads them asks, since the answer is
	// larger: a reservation that does not, such as a provider's quota that nothing
	// reports on, is answered with the decision alone, and so is a request that
	// limits neither requests nor tokens, which has no allowance to state.
	ReportRate bool
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

// HasRateLimits reports whether a request, token or concurrency limit applies,
// which is what makes the reservation touch the rate script. A request bound
// only by a cost budget never does.
func (r Request) HasRateLimits() bool {
	return r.RequestsPerMinute != nil || r.TokensPerMinute != nil || r.MaxConcurrency != nil
}

// reportsRate reports whether the rate script is asked for the allowance: the
// caller wants it, and a request or token limit gives it something to state.
// Validation has made every limit that is set a positive one.
func (r Request) reportsRate() bool {
	return r.ReportRate && (r.RequestsPerMinute != nil || r.TokensPerMinute != nil)
}

// costGrace is the time a cost reservation outlives the request.
func (r Request) costGrace() time.Duration {
	if r.CostGrace == 0 {
		return DefaultCostGrace
	}
	return r.CostGrace
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
	if r.CostGrace < 0 {
		return &InvalidRequestError{Reason: "cost grace cannot be negative"}
	}
	if r.CostEstimate != "" {
		if !r.HasCostBudget() {
			return &InvalidRequestError{Reason: "a cost estimate needs a cost budget to reserve against"}
		}
		if !ValidCostLimit(r.CostEstimate) {
			return &InvalidRequestError{
				Reason: "cost estimate must be a positive decimal with at most 12 integer and 12 fractional digits",
			}
		}
		if _, err := uuid.Parse(r.RequestID); err != nil {
			return &InvalidRequestError{Reason: "a cost estimate needs the request ID it will be accounted under"}
		}
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

// Commander runs one Valkey command. A *coordination.Client is the one a process
// uses; a test substitutes its own to fail a command the way an outage does.
type Commander interface {
	Do(ctx context.Context, args ...string) (any, error)
}

// Limiter reserves shared budgets through one Valkey connection.
type Limiter struct {
	client    Commander
	namespace string
}

// New builds a limiter over client. The namespace prefixes every key and must
// not introduce a second Valkey Cluster hash tag.
func New(client Commander, namespace string) (*Limiter, error) {
	// A nil *coordination.Client inside the interface is as unusable as none.
	if concrete, ok := client.(*coordination.Client); client == nil || (ok && concrete == nil) {
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

// keys holds the keys one request can touch. The rate and concurrency keys share
// the lookup as their cluster hash tag so one script sees both; the cost keys
// share the cost owner ID so every lookup of one owner meets the same balance
// and the same reservations.
type keys struct {
	rate        string
	concurrency string
	dailyCost   string
	monthlyCost string
	pending     string
	expiry      string
}

// keysFor names only the keys the request's budgets use, so a request pays
// nothing for a budget it was not given.
func (l *Limiter) keysFor(r Request) keys {
	var k keys
	if r.HasRateLimits() {
		k.rate, k.concurrency = l.rateKeys(r.LookupID)
	}
	if r.HasCostBudget() {
		prefix := l.costPrefix(r.CostOwnerID)
		k.dailyCost, k.monthlyCost = prefix+":day", prefix+":month"
		k.pending, k.expiry = prefix+":pending", prefix+":expiry"
	}
	return k
}

// rateKeys addresses the counters one lookup shares across replicas.
func (l *Limiter) rateKeys(lookupID string) (string, string) {
	tagged := l.namespace + ":{" + lookupID + "}"
	return tagged + ":rate", tagged + ":concurrency"
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
// rotation is not punished for the version it replaced. Grants also scope
// refusals to the dispatched generation; static credentials use zero.
func CredentialScope(providerID string, credentialID *string, generation int64) string {
	version := nilUUID
	if credentialID != nil {
		version = *credentialID
	}
	scope := canonicalUUID(providerID) + ":" + canonicalUUID(version)
	if generation != 0 {
		scope += ":" + strconv.FormatInt(generation, 10)
	}
	return scope
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
// what separates "rejected" from "unknown". It names the script by its digest and
// sends the source only when the server answers that it holds no such script.
func (l *Limiter) eval(ctx context.Context, s script, scriptKeys []string, args ...string) (any, error) {
	command := make([]string, 0, 3+len(scriptKeys)+len(args))
	command = append(command, "EVALSHA", s.digest, strconv.Itoa(len(scriptKeys)))
	command = append(command, scriptKeys...)
	command = append(command, args...)
	value, err := l.client.Do(ctx, command...)
	if noScript(err) {
		// A server that has just started, or that a failover has put in charge, has
		// not cached the script. The refusal comes before anything runs, so sending
		// the call again cannot run it twice, and EVAL caches the script for the
		// calls that follow.
		command[0], command[1] = "EVAL", s.source
		value, err = l.client.Do(ctx, command...)
	}
	if err != nil {
		return nil, &ServiceError{Err: err}
	}
	return value, nil
}

// noScript reports whether the server refused a call because it holds no script
// with that digest. The client reports it in the words of the server or of its
// own driver, and only as text.
func noScript(err error) bool {
	command, ok := errors.AsType[*coordination.CommandError](err)
	if !ok || command.Ambiguous || command.Cause == nil {
		return false
	}
	text := command.Cause.Error()
	return strings.Contains(text, "NOSCRIPT") || strings.Contains(text, "NoScriptError")
}

// Reserve admits one request against every configured budget. Cost is settled
// first because it is the budget a caller cannot undo by refunding a lease.
// It returns ExceededError when a budget rejects the request, ErrUninitializedCost
// when cost state is not reconciled yet, ErrMalformedState when stored state is
// unusable, and ServiceError when Valkey is unreachable. A request refused after
// its cost was reserved gives the reservation back before returning.
func (l *Limiter) Reserve(ctx context.Context, r Request) (*Lease, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	scriptKeys := l.keysFor(r)
	lease := &Lease{
		limiter:        l,
		rateKey:        scriptKeys.rate,
		concurrencyKey: scriptKeys.concurrency,
		reservedTokens: r.RequestedTokens,
		hasToken:       r.TokensPerMinute != nil,
		hasRequest:     r.RequestsPerMinute != nil,
		hasRate:        r.HasRateLimits(),
	}
	if r.HasCostBudget() {
		reserved, err := l.reserveCost(ctx, r, scriptKeys)
		if err != nil {
			return nil, err
		}
		if reserved {
			lease.costReserved = true
			lease.pendingKey, lease.expiryKey = scriptKeys.pending, scriptKeys.expiry
			lease.costID = canonicalUUID(r.RequestID)
			lease.costGrace = r.costGrace()
		}
	}
	if !r.HasRateLimits() {
		// Nothing for the rate script to count, so it is not asked.
		return lease, nil
	}
	lease.id = uuid.Must(uuid.NewV7()).String()
	granted, err := l.reserveRate(ctx, r, scriptKeys, lease)
	if err != nil {
		_, outage := errors.AsType[*ServiceError](err)
		lease.abandonCost(ctx, outage)
		return nil, err
	}
	lease.windowID = granted.windowID
	lease.concurrencyExpiresAtMS = granted.leaseExpiresAtMS
	if r.reportsRate() {
		lease.rate = &rateAnswer{state: granted.rate, at: time.Now()}
	}
	return lease, nil
}

// reserveRate counts the request against the rate and concurrency limits.
func (l *Limiter) reserveRate(ctx context.Context, r Request, scriptKeys keys, lease *Lease) (scriptResult, error) {
	stated := r.reportsRate()
	value, err := l.eval(ctx, reserveLimitsScript,
		[]string{scriptKeys.rate, scriptKeys.concurrency},
		optionalLimit(r.RequestsPerMinute), optionalLimit(r.TokensPerMinute),
		strconv.FormatInt(r.RequestedTokens, 10), optionalLimit(r.MaxConcurrency),
		lease.id, strconv.FormatInt(r.LeaseTTL.Milliseconds(), 10), switchArg(stated))
	if err != nil {
		return scriptResult{}, err
	}
	result, err := parseReservation(value, stated)
	if err != nil {
		return scriptResult{}, err
	}
	// The script echoes the limits it was given with the allowance, and an answer
	// that names other limits was not made for this request.
	if stated && (result.kind == resultGranted || result.kind == resultRejected) &&
		(result.rate.RequestLimit != limitValue(r.RequestsPerMinute) ||
			result.rate.TokenLimit != limitValue(r.TokensPerMinute)) {
		return scriptResult{}, ErrUnexpectedResponse
	}
	switch result.kind {
	case resultGranted:
		// A concurrency lease is reported exactly when one was requested. Any
		// other pairing means the reply does not describe this request.
		if (r.MaxConcurrency != nil) != (result.leaseExpiresAtMS > 0) {
			return scriptResult{}, ErrUnexpectedResponse
		}
		return result, nil
	case resultRejected:
		return scriptResult{}, &ExceededError{
			Dimension:  result.dimension,
			RetryAfter: time.Duration(result.retryAfterMS) * time.Millisecond,
			Rate:       result.rate,
		}
	case resultMalformed:
		return scriptResult{}, ErrMalformedState
	default:
		return scriptResult{}, ErrUnexpectedResponse
	}
}

// reserveCost checks the daily and monthly balances against what is accrued and
// what requests in flight may still spend, and records this request's estimate
// as one of them. It never writes an accrued total: only PostgreSQL
// reconciliation may do that. It reports whether an estimate was recorded.
func (l *Limiter) reserveCost(ctx context.Context, r Request, scriptKeys keys) (bool, error) {
	daily, monthly := "", ""
	if r.DailyCostLimit != nil {
		daily = *r.DailyCostLimit
	}
	if r.MonthlyCostLimit != nil {
		monthly = *r.MonthlyCostLimit
	}
	amount, leaseID, ttl := "0", "", "0"
	if r.CostEstimate != "" {
		amount, leaseID = r.CostEstimate, canonicalUUID(r.RequestID)
		ttl = strconv.FormatInt((r.LeaseTTL + r.costGrace()).Milliseconds(), 10)
	}
	value, err := l.eval(ctx, reserveCostScript,
		[]string{scriptKeys.dailyCost, scriptKeys.monthlyCost, scriptKeys.pending, scriptKeys.expiry},
		daily, monthly, "0", amount, leaseID, ttl)
	if err == nil {
		result, parseErr := parseCostReservation(value)
		if parseErr == nil {
			switch result.kind {
			case resultGranted:
				return r.CostEstimate != "", nil
			case resultRejected:
				return false, &ExceededError{
					Dimension:  result.dimension,
					RetryAfter: time.Duration(result.retryAfterMS) * time.Millisecond,
					Estimate:   result.estimate,
				}
			case resultUninitialized:
				return false, ErrUninitializedCost
			case resultMalformed:
				return false, ErrMalformedState
			}
			parseErr = ErrUnexpectedResponse
		}
		err = parseErr
	}
	// A command that timed out may still have run, and a reply this package cannot
	// read may describe a reservation that was made. Nothing will settle either, so
	// the estimate is given back; releasing a lease that is not there does nothing.
	// A command that never reached Valkey reserved nothing, and during an outage is
	// not asked again.
	if r.CostEstimate != "" && ambiguousFailure(err) {
		(&Lease{
			limiter: l, costReserved: true, pendingKey: scriptKeys.pending, expiryKey: scriptKeys.expiry,
			costID: canonicalUUID(r.RequestID),
		}).abandonCost(ctx, true)
	}
	return false, err
}

// ambiguousFailure reports whether a failed cost reservation may nonetheless have
// been made: the command may have run, or its reply cannot be read. A refusal
// that never reached Valkey cannot have.
func ambiguousFailure(err error) bool {
	if _, ok := errors.AsType[*ServiceError](err); !ok {
		return true
	}
	command, ok := errors.AsType[*coordination.CommandError](err)
	return !ok || command.Ambiguous
}

// optionalLimit renders an absent limit as the script's "unlimited" zero.
func optionalLimit(limit *int64) string {
	return strconv.FormatInt(limitValue(limit), 10)
}

// switchArg renders a switch as the script's "0" or "1".
func switchArg(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// limitValue is an absent limit as the script's "unlimited" zero.
func limitValue(limit *int64) int64 {
	if limit == nil {
		return 0
	}
	return *limit
}

// RateState is what remains of a lookup's per-minute request and token
// allowances in the fixed UTC minute a reservation was measured in. Only a
// dimension with a limit is stated: a limit of zero means that dimension is
// unlimited and its remaining count is zero.
//
// The counts are the ones the rate script held when it answered, not a live
// reading. A granted request's own reservation is already counted in them, and
// its token reservation is the admission estimate, which Reconcile later
// replaces with the tokens actually used. A rejected request reserved nothing,
// so its counts are what the window still holds. The zero value states no
// allowance, and is what a reservation reports that was not asked to state one.
type RateState struct {
	// RequestLimit and TokenLimit are the per-minute limits.
	RequestLimit int64
	TokenLimit   int64
	// RequestRemaining and TokenRemaining are what those limits still allow
	// within the minute, never below zero.
	RequestRemaining int64
	TokenRemaining   int64
	// ResetAfter is the time the script measured to the end of the minute, when
	// both counters start again, and ResetAt that boundary. Valkey's clock is the
	// only one either is read from.
	ResetAfter time.Duration
	ResetAt    time.Time
}

// Limited reports whether the state carries an allowance: a request or a token
// limit applies.
func (s RateState) Limited() bool { return s.RequestLimit > 0 || s.TokenLimit > 0 }

// LimitsRequests reports whether the requests per minute are limited.
func (s RateState) LimitsRequests() bool { return s.RequestLimit > 0 }

// LimitsTokens reports whether the tokens per minute are limited.
func (s RateState) LimitsTokens() bool { return s.TokenLimit > 0 }

// rateState builds the allowance a rate reservation reply states for the fixed
// window with that ID. A reply that limits neither requests nor tokens states
// none, whatever the window.
func rateState(window, requestLimit, requestRemaining, tokenLimit, tokenRemaining, resetMS int64) RateState {
	if requestLimit == 0 && tokenLimit == 0 {
		return RateState{}
	}
	return RateState{
		RequestLimit:     requestLimit,
		RequestRemaining: requestRemaining,
		TokenLimit:       tokenLimit,
		TokenRemaining:   tokenRemaining,
		ResetAfter:       time.Duration(resetMS) * time.Millisecond,
		ResetAt:          time.UnixMilli((window + 1) * fixedWindowMS).UTC(),
	}
}

// Lease is a granted reservation. Exactly one of Refund, Reconcile or Release
// finishes it, and each may be called repeatedly: the scripts are idempotent
// per lease.
//
// A request admitted against a budget group holds two leases, one per cost
// owner. Attach makes the group's lease part of the key's, so one handle
// finishes both.
type Lease struct {
	limiter                *Limiter
	id                     string
	rateKey                string
	concurrencyKey         string
	windowID               int64
	reservedTokens         int64
	hasToken               bool
	hasRequest             bool
	hasRate                bool
	concurrencyExpiresAtMS int64
	// rate is the allowance the rate script answered the reservation with. Only a
	// request that asked for one holds it, so that no other lease is made larger
	// by it.
	rate *rateAnswer
	// group is the lease taken on the budget group's shared cost budget.
	group *Lease
	// costReserved says this lease holds an estimate in its owner's pending
	// reservations under costID, which is the request's accounting identifier.
	// actualCost is what the request cost, once it is known.
	costReserved bool
	pendingKey   string
	expiryKey    string
	costID       string
	costGrace    time.Duration
	actualCost   string
}

// RateState reports the request and token allowance this reservation was
// measured against, as the rate script answered it. It is the lookup's own: a
// lease attached with Attach, which carries a budget group's cost, adds
// nothing to it. It is the zero state, which is not Limited, for a nil lease, for
// one that reserved no request or token limit, and for one whose Request did not
// ask for the allowance with ReportRate.
func (le *Lease) RateState() RateState {
	if le == nil || le.rate == nil {
		return RateState{}
	}
	return le.rate.state
}

// RateReset is how long the minute RateState was measured in still has to run:
// the reset the script measured less the time that has passed since its answer
// arrived, which is the monotonic clock of this process and not Valkey's. A
// response that is written long after admission, as a stream's first byte or a
// slow completion is, can then report the time that is left rather than the time
// there was. It is a whole number of milliseconds, like the script's own
// reading, rounded up so that a minute with any time left never reads as over.
// It is zero once the minute is over, and for a lease that states no allowance.
func (le *Lease) RateReset() time.Duration {
	if le == nil || le.rate == nil || !le.rate.state.Limited() {
		return 0
	}
	left := le.rate.state.ResetAfter - time.Since(le.rate.at)
	if left <= 0 {
		return 0
	}
	return (left + time.Millisecond - 1).Truncate(time.Millisecond)
}

// rateAnswer is the allowance a lease was answered with and when this process
// received the answer.
type rateAnswer struct {
	state RateState
	at    time.Time
}

// Attach makes group part of this lease, so that finishing the request finishes
// both. A nil group changes nothing.
func (le *Lease) Attach(group *Lease) {
	if le != nil && group != nil {
		le.group = group
	}
}

// HasCostReservation reports whether this lease, or the one attached to it, holds
// an estimate against a cost budget.
func (le *Lease) HasCostReservation() bool {
	return le != nil && (le.costReserved || le.group.HasCostReservation())
}

// SetActualCost records what the request cost, a canonical decimal string, for
// SettleCost to replace its estimate with. The attached lease is a reservation
// of the same request and takes the same amount.
func (le *Lease) SetActualCost(amount string) {
	if le == nil {
		return
	}
	le.actualCost = amount
	le.group.SetActualCost(amount)
}

// SettleCost replaces the estimate with the recorded actual cost. It does nothing
// for a lease that holds no estimate or has no actual cost recorded.
//
// A request that cost nothing, which is also one whose usage is still to come, gives
// its estimate back: that is a release, retried like Refund's, because nothing
// else removes it before it lapses. Replacing the estimate with a cost is one
// attempt, bounded by settleTimeout for each lease, because a settlement that is
// lost leaves the reservation made at admission: it lapses on its own, and the
// spend accounting records for the request removes it sooner.
func (le *Lease) SettleCost(ctx context.Context) error {
	if le == nil {
		return nil
	}
	err := le.group.SettleCost(ctx)
	if !le.costReserved || le.actualCost == "" {
		return err
	}
	if zeroAmount(le.actualCost) {
		return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
			return le.settleCost(ctx, "0", "0")
		}))
	}
	grace := strconv.FormatInt(max(le.costGrace.Milliseconds(), 1), 10)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	return errors.Join(err, le.settleCost(ctx, le.actualCost, grace))
}

// zeroAmount reports whether a decimal is nothing, however it is written.
func zeroAmount(amount string) bool {
	digits := strings.Replace(amount, ".", "", 1)
	return digits != "" && strings.Trim(digits, "0") == ""
}

// settleCost runs the settlement script once, within whatever time ctx allows.
// The caller sets the bound: SettleCost holds a replacement to settleTimeout, and
// the releases that retry hold each attempt to cleanupTimeout.
func (le *Lease) settleCost(ctx context.Context, amount, ttl string) error {
	value, err := le.limiter.eval(ctx, settleCostScript, []string{le.pendingKey, le.expiryKey}, le.costID, amount, ttl)
	if err != nil {
		return err
	}
	_, err = parseSettlement(value)
	return err
}

// abandonCost gives back an estimate that nothing will settle. When the failure
// that led here left the outcome of the script unknown, one bounded attempt is
// made, so that an outage is not made to wait longer; otherwise the release is
// retried like every other cleanup.
func (le *Lease) abandonCost(ctx context.Context, ambiguous bool) {
	if !le.costReserved {
		return
	}
	release := func(ctx context.Context) error { return le.settleCost(ctx, "0", "0") }
	if ambiguous {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = release(ctx)
		return
	}
	_ = cleanup(ctx, release)
}

// Refund returns an admission that never dispatched. It only applies inside the
// minute that granted it, so it can never take capacity from a later window, and
// it releases the cost estimate, which a request that spent nothing never owed.
func (le *Lease) Refund(ctx context.Context) error {
	if le == nil {
		return nil
	}
	err := le.group.Refund(ctx)
	if le.costReserved {
		err = errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
			return le.settleCost(ctx, "0", "0")
		}))
	}
	if !le.hasRate {
		return err
	}
	tokens := int64(0)
	if le.hasToken {
		tokens = le.reservedTokens
	}
	requests := "0"
	if le.hasRequest {
		requests = "1"
	}
	return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
		_, err := le.limiter.eval(ctx, refundLimitsScript,
			[]string{le.rateKey, le.concurrencyKey},
			strconv.FormatInt(le.windowID, 10), requests,
			strconv.FormatInt(tokens, 10), le.id)
		return err
	}))
}

// Reconcile settles the token reservation against the tokens actually used. It
// is a no-op without a token budget, and the script ignores a window that has
// already rolled over.
func (le *Lease) Reconcile(ctx context.Context, actualTokens int64) error {
	if le == nil {
		return nil
	}
	err := le.group.Reconcile(ctx, actualTokens)
	if !le.hasToken {
		return err
	}
	if actualTokens < 0 || actualTokens > maxLuaInteger {
		return errors.Join(err, &InvalidRequestError{Reason: "actual tokens must be a non-negative Lua-safe integer"})
	}
	adjustment := actualTokens - le.reservedTokens
	if adjustment == 0 {
		return err
	}
	return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
		_, err := le.limiter.eval(ctx, reconcileLimitsScript, []string{le.rateKey},
			strconv.FormatInt(le.windowID, 10), strconv.FormatInt(adjustment, 10), le.id)
		return err
	}))
}

// Release frees the concurrency slot. Requests and tokens stay consumed: the
// attempt did happen.
func (le *Lease) Release(ctx context.Context) error {
	if le == nil {
		return nil
	}
	err := le.group.Release(ctx)
	if le.concurrencyExpiresAtMS == 0 {
		return err
	}
	return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
		_, err := le.limiter.eval(ctx, releaseConcurrencyScript, []string{le.concurrencyKey}, le.id)
		return err
	}))
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
