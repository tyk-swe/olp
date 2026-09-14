package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

// Admission enforces the budgets every replica shares through Valkey: the API
// key limits before a request is served, and the provider connection and
// credential slot quotas before each attempt leaves this gateway. A nil
// *Admission means no limiter was configured at all, which admits keys and
// targets that bound nothing and fails everything else closed.
type Admission struct {
	limiter  *limits.Limiter
	policy   func() limits.OutagePolicy
	log      *slog.Logger
	failOpen atomic.Int64
}

// NewAdmission binds a limiter to the outage policy of the installation. The
// policy is read per decision so an operator can change it without a restart;
// a nil policy fails closed.
func NewAdmission(limiter *limits.Limiter, policy func() limits.OutagePolicy, log *slog.Logger) *Admission {
	return &Admission{limiter: limiter, policy: policy, log: log}
}

// FailOpenTotal counts the requests admitted without a reservation because the
// limiter could not answer and the installation chose to fail open. It is the
// source of the olp_limits_fail_open_total metric, which this build does not
// export yet: the Prometheus surface arrives with the observability milestone
// (docs/roadmap/06-media-and-console-parity.md), and until it does the only
// per-request trace is the warning outage logs.
func (a *Admission) FailOpenTotal() int64 {
	if a == nil {
		return 0
	}
	return a.failOpen.Load()
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

// rateLimited renders the rejection a shared budget produced.
func rateLimited(dimension limits.Dimension, retryAfter time.Duration) *Error {
	code, message := "rate_limit_exceeded", "The API key rate limit was exceeded."
	switch dimension {
	case limits.DimensionRequests:
		message = "The API key requests per minute limit was exceeded."
	case limits.DimensionTokens:
		message = "The API key tokens per minute limit was exceeded."
	case limits.DimensionConcurrency:
		message = "The API key concurrency limit was exceeded."
	case limits.DimensionDailyCost, limits.DimensionMonthlyCost:
		// Both budgets answer with the one message the API contract pins,
		// including why spend a dashboard reports as under the limit can still
		// exhaust it: an attempt nobody could price is charged nothing.
		code, message = "budget_exhausted", "The API key cost budget was exhausted. Unpriced attempts accrue 0."
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

// keyRequest describes the API key budgets as one admission decision.
func keyRequest(authority access.Authority, estimate int64, ttl time.Duration) limits.Request {
	policy := authority.Policy
	return limits.Request{
		APIKeyID:          authority.ID,
		LookupID:          authority.LookupID,
		RequestsPerMinute: policy.RequestsPerMinute,
		TokensPerMinute:   policy.TokensPerMinute,
		MaxConcurrency:    policy.MaxConcurrency,
		DailyCostLimit:    policy.DailyCostLimit,
		MonthlyCostLimit:  policy.MonthlyCostLimit,
		RequestedTokens:   estimate,
		LeaseTTL:          ttl,
	}
}

// reserveKey admits one request against the API key budgets. A nil lease with
// a nil error admits the request without one: either the key bounds nothing,
// or the limiter is unreachable and the installation chose to fail open.
func (a *Admission) reserveKey(ctx context.Context, authority access.Authority, estimate int64, ttl time.Duration) (*limits.Lease, *Error) {
	request := keyRequest(authority, estimate, ttl)
	if !request.HasHardLimits() {
		// Nothing to enforce, so nothing to store: a key without hard limits
		// never reaches Valkey.
		return nil, nil
	}
	if !a.ready() {
		return nil, a.outage(authority.ID, request.HasCostBudget(), errors.New("no limiter configured"))
	}
	if request.TokensPerMinute != nil && estimate > *request.TokensPerMinute {
		// No window will ever hold this request: answer now rather than make
		// the caller retry into a limit it cannot satisfy.
		return nil, invalidRequest("request_exceeds_token_limit", "This request needs more tokens than the API key tokens per minute limit allows.", nil)
	}
	decision, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	lease, err := a.limiter.Reserve(decision, request)
	if err == nil {
		return lease, nil
	}
	var exceeded *limits.ExceededError
	if errors.As(err, &exceeded) {
		return nil, rateLimited(exceeded.Dimension, exceeded.RetryAfter)
	}
	return nil, a.outage(authority.ID, request.HasCostBudget(), err)
}

// outage decides what happens to a request whose budgets cannot be consulted.
// Spending against an unknown balance can never be undone, so a cost budget
// always fails closed; rate and concurrency limits follow the policy the
// installation configured.
func (a *Admission) outage(keyID string, costBudget bool, cause error) *Error {
	if costBudget || a == nil || a.policy == nil || a.policy() != limits.FailOpen {
		return limitsUnavailable()
	}
	a.failOpen.Add(1)
	a.logger().Warn("admitting request without distributed limits", "api_key_id", keyID, "error", cause.Error())
	return nil
}

// settleKey finishes the key reservation. A request that never dispatched an
// attempt consumed nothing and is refunded in full; any other request keeps
// the request and token it spent but must return the concurrency slot, and
// reports the tokens it actually used when the provider disclosed them.
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
}

// targetReservation holds what one attempt reserved on its provider.
type targetReservation struct {
	connection *limits.Lease
	slot       *limits.Lease
	log        *slog.Logger
}

// settle reconciles and releases both leases once the attempt has ended.
func (t *targetReservation) settle(ctx context.Context, actual *int64) {
	if t == nil {
		return
	}
	settleKey(ctx, t.connection, true, actual, t.log)
	settleKey(ctx, t.slot, true, actual, t.log)
}

// connectionRequest describes the quota shared by every attempt that flows
// through one provider connection.
func connectionRequest(provider *runtime.Provider, estimate int64, ttl time.Duration) limits.Request {
	request := limits.Request{
		APIKeyID:        provider.ID,
		LookupID:        limits.ConnectionLookup(provider.ID),
		RequestedTokens: estimate,
		LeaseTTL:        ttl,
	}
	if provider.Limits != nil {
		request.RequestsPerMinute = provider.Limits.RequestsPerMinute
		request.TokensPerMinute = provider.Limits.TokensPerMinute
		request.MaxConcurrency = provider.Limits.MaxConcurrency
	}
	return request
}

// slotRequest describes the quota of one credential slot.
func slotRequest(slot *runtime.Slot, estimate int64, ttl time.Duration) limits.Request {
	return limits.Request{
		APIKeyID:          slot.ID,
		LookupID:          limits.SlotLookup(slot.ID),
		RequestsPerMinute: slot.RequestsPerMinute,
		TokensPerMinute:   slot.TokensPerMinute,
		MaxConcurrency:    slot.MaxConcurrency,
		RequestedTokens:   estimate,
		LeaseTTL:          ttl,
	}
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
		var exceeded *limits.ExceededError
		if errors.As(err, &exceeded) {
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
func (a *Admission) cooldown(ctx context.Context, providerID string, slot *runtime.Slot, d time.Duration) {
	if !a.ready() || d <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), coordinationTimeout)
	defer cancel()
	for _, scope := range [...]string{limits.CredentialScope(providerID, slot.CredentialID), limits.SlotScope(slot.ID)} {
		if err := a.limiter.Cooldown(ctx, scope, d); err != nil {
			a.logger().Warn("shared cooldown not recorded", "provider_id", providerID, "slot_id", slot.ID, "error", err.Error())
		}
	}
}

// cooling reports whether another replica put this credential or slot in a
// cooldown. A store that cannot answer must not take every slot out of
// service, so an unreadable cooldown is treated as absent and logged.
func (a *Admission) cooling(ctx context.Context, providerID string, slot *runtime.Slot) bool {
	if !a.ready() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, coordinationTimeout)
	defer cancel()
	cooling, err := a.limiter.Cooling(ctx, limits.CredentialScope(providerID, slot.CredentialID), limits.SlotScope(slot.ID))
	if err != nil {
		a.logger().Warn("shared cooldown not read", "provider_id", providerID, "slot_id", slot.ID, "error", err.Error())
		return false
	}
	return cooling
}

const (
	// charsPerToken is the ratio text is charged at. Four characters per token
	// is a conservative portable approximation across connectors.
	charsPerToken = 4
	// imageTokens and mediaTokens are the flat charges for content whose size
	// on the wire says nothing about what a model will be billed for: an
	// inline image arrives as a megabyte of base64, and charging it by its
	// length would refuse requests no provider would have refused.
	imageTokens = 1000
	mediaTokens = 2000
	// defaultOutputTokens stands in for a caller that named no output bound.
	defaultOutputTokens = 4096
	// maxEstimate is the largest integer the limiter can store.
	maxEstimate = 1<<53 - 1
)

// estimateTokens is the tokens a request may consume, charged before the
// upstream reports what it actually used. The prompt is walked rather than
// weighed: text is charged at four characters per token, each media part at a
// flat rate, and the reply at the largest size the caller allowed — the bound
// it named for one candidate, multiplied by the candidates asked for. It is
// deliberately generous — a reservation is reconciled against the real usage
// as soon as the attempt ends, and admitting work that cannot fit in the
// window is worse than deferring work that would have.
func estimateTokens(parsed *openai.Request) int64 {
	input := int64(0)
	if parsed != nil {
		switch parsed.Family {
		case openai.FamilyChat:
			input = estimateItems(parsed.Field("messages"))
		case openai.FamilyResponses:
			input = estimateItems(parsed.Field("input"))
		}
		input = addBounded(input, estimateTools(parsed.Field("tools")))
	}
	output := int64(defaultOutputTokens)
	for _, field := range [...]string{"max_completion_tokens", "max_tokens", "max_output_tokens"} {
		if value, ok := integerField(parsed, field); ok {
			output = value
			break
		}
	}
	candidates := int64(1)
	if value, ok := integerField(parsed, "n"); ok {
		candidates = value
	}
	return max(addBounded(input, multiplyBounded(max(output, 1), max(candidates, 1))), 1)
}

// estimateItems charges the conversation one request carries: the messages of
// a chat request, or the input of a responses request, which may also be one
// plain string. A shape this gateway does not recognise is charged nothing
// rather than guessed at, because the reservation is reconciled against the
// usage the upstream reports as soon as the attempt ends.
func estimateItems(raw json.RawMessage) int64 {
	if tokens, ok := textTokens(raw); ok {
		return tokens
	}
	total := int64(0)
	for _, raw := range jsonArray(raw) {
		item := jsonObject(raw)
		if item == nil {
			continue
		}
		// A responses tool result carries what it returned in `output`.
		total = addBounded(total, estimateContent(item["content"]))
		total = addBounded(total, estimateContent(item["output"]))
		// The identifiers and arguments a message travels with are prompt text
		// like any other; `call_id` and `arguments` are the responses spelling
		// of the chat tool-call fields.
		for _, name := range [...]string{"name", "tool_call_id", "call_id", "arguments"} {
			total = addBounded(total, estimateText(item[name]))
		}
		for _, raw := range jsonArray(item["tool_calls"]) {
			call := jsonObject(raw)
			if function := jsonObject(call["function"]); function != nil {
				call = function
			}
			total = addBounded(total, estimateText(call["name"]))
			total = addBounded(total, estimateText(call["arguments"]))
		}
	}
	return total
}

// estimateContent charges one message body: plain text, or the parts a
// multimodal message carries.
func estimateContent(raw json.RawMessage) int64 {
	if tokens, ok := textTokens(raw); ok {
		return tokens
	}
	total := int64(0)
	for _, raw := range jsonArray(raw) {
		part := jsonObject(raw)
		if part == nil {
			// A bare string among the parts is text.
			total = addBounded(total, estimateText(raw))
			continue
		}
		kind, _ := textOf(part["type"])
		switch kind {
		case "image_url", "input_image":
			total = addBounded(total, imageTokens)
		case "input_audio", "input_file", "file":
			total = addBounded(total, mediaTokens)
		default:
			total = addBounded(total, estimateText(part["text"]))
			total = addBounded(total, estimateText(part["refusal"]))
		}
	}
	return total
}

// estimateTools charges the tool catalogue a request carries: a schema the
// model has to read costs what any other prompt text costs.
func estimateTools(raw json.RawMessage) int64 {
	total := int64(0)
	for _, raw := range jsonArray(raw) {
		tool := jsonObject(raw)
		// Chat nests the definition under `function`; responses holds it flat.
		if function := jsonObject(tool["function"]); function != nil {
			tool = function
		}
		total = addBounded(total, estimateText(tool["name"]))
		total = addBounded(total, estimateText(tool["description"]))
		total = addBounded(total, estimateSchema(tool["parameters"]))
	}
	return total
}

// estimateSchema charges a JSON schema as the document it is, with whatever
// formatting the caller happened to send removed.
func estimateSchema(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return 0
	}
	return charge(utf8.RuneCount(compact.Bytes()))
}

// textTokens charges a JSON string, reporting whether the value was one.
func textTokens(raw json.RawMessage) (int64, bool) {
	text, ok := textOf(raw)
	if !ok {
		return 0, false
	}
	return charge(utf8.RuneCountInString(text)), true
}

// estimateText charges a field that holds text, and nothing for one that
// holds anything else.
func estimateText(raw json.RawMessage) int64 {
	tokens, _ := textTokens(raw)
	return tokens
}

// charge converts a character count into tokens, rounding up so that no text
// is free.
func charge(characters int) int64 {
	return int64((characters + charsPerToken - 1) / charsPerToken)
}

// textOf reads a JSON string, reporting whether the value was one. Characters
// are counted rather than bytes so a multi-byte script is not overcharged.
func textOf(raw json.RawMessage) (string, bool) {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

// jsonArray and jsonObject decode a value of the shape the estimate expects,
// and nothing at all for any other shape.
func jsonArray(raw json.RawMessage) []json.RawMessage {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil
	}
	return items
}

func jsonObject(raw json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

// integerField reads one top-level request field as an integer. A field that
// is absent, null, or not an integer leaves the estimate on its default.
func integerField(parsed *openai.Request, name string) (int64, bool) {
	if parsed == nil {
		return 0, false
	}
	raw := parsed.Field(name)
	if len(raw) == 0 {
		return 0, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
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
	total := usage.TotalTokens
	if total <= 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	if total < 0 {
		return nil
	}
	total = min(total, maxEstimate)
	return &total
}
