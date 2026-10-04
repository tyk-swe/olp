package limits

// Script replies are parsed strictly: a reply that does not match the contract
// in scripts/ describes some other script, and guessing at its meaning would
// admit or reject requests for reasons this package cannot explain.

// resultKind classifies one admission reply.
type resultKind int

const (
	resultGranted resultKind = iota
	resultRejected
	resultUninitialized
	resultMalformed
	resultScriptFailure
)

// scriptResult is a parsed reservation reply. windowID and leaseExpiresAtMS are
// only meaningful for a granted reservation, and estimate for a rejected cost
// reservation: it says the budget refused the request's estimate, not that it
// has been spent. rate is the allowance a granted or rejected rate reservation
// answered with, when it was asked to state one.
type scriptResult struct {
	kind             resultKind
	dimension        Dimension
	retryAfterMS     int64
	windowID         int64
	leaseExpiresAtMS int64
	estimate         bool
	rate             RateState
}

// replyItems asserts an array reply of exactly size elements.
func replyItems(value any, size int) ([]any, bool) {
	items, ok := value.([]any)
	if !ok || len(items) != size {
		return nil, false
	}
	return items, true
}

func replyInt(value any) (int64, bool) {
	number, ok := value.(int64)
	return number, ok
}

func replyString(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

// luaSafe reports whether a counter came back inside the range Lua represents
// exactly. Anything else means the stored state has drifted out of contract.
func luaSafe(value int64) bool { return value >= 0 && value <= maxLuaInteger }

// tuple decodes the {version, status, detail, retry_after_ms, a, b} shape that
// both reservation scripts answer with. Each script family versions its replies
// on its own, so a change to one family's contract never invalidates the other.
func tuple(value any, wantVersion int64) (status int64, detail string, retry, first, second int64, ok bool) {
	items, ok := replyItems(value, 6)
	if !ok {
		return 0, "", 0, 0, 0, false
	}
	return head(items, wantVersion)
}

// head decodes the six fields every reservation reply begins with, from an array
// of at least six.
func head(items []any, wantVersion int64) (status int64, detail string, retry, first, second int64, ok bool) {
	version, okVersion := replyInt(items[0])
	status, okStatus := replyInt(items[1])
	detail, okDetail := replyString(items[2])
	retry, okRetry := replyInt(items[3])
	first, okFirst := replyInt(items[4])
	second, okSecond := replyInt(items[5])
	if !okVersion || !okStatus || !okDetail || !okRetry || !okFirst || !okSecond ||
		version != wantVersion || !luaSafe(retry) || !luaSafe(first) || !luaSafe(second) {
		return 0, "", 0, 0, 0, false
	}
	return status, detail, retry, first, second, true
}

// maxWindowID bounds the fixed window a rate reply may name: the end of every
// window below it, in milliseconds, is a counter the scripts represent exactly.
const maxWindowID = maxLuaInteger / fixedWindowMS

// A reserve_limits reply begins with replyHeadFields fields, which every reply
// has. A grant or a rejection that was asked to state the allowance appends the
// allowanceFields fields that state it, and nothing else does: a failure
// measured nothing, and a request that did not ask is not made to carry fields
// that nothing reads.
const (
	replyHeadFields = 6
	allowanceFields = 5
)

// parseReservation decodes a reserve_limits reply. stated is whether the request
// asked for the allowance, and the reply has to be the shape that was asked for:
// a script that answered otherwise is not the one this package runs. A stated
// allowance has to be consistent with itself and with the decision it came with,
// since a remaining count beyond its limit, or a window that outlasts the minute,
// describes some other script.
func parseReservation(value any, stated bool) (scriptResult, error) {
	items, ok := value.([]any)
	if !ok || len(items) < replyHeadFields {
		return scriptResult{}, ErrUnexpectedResponse
	}
	status, detail, retry, window, expiry, ok := head(items, limitsResponseVersion)
	if !ok {
		return scriptResult{}, ErrUnexpectedResponse
	}
	var result scriptResult
	switch {
	case status == 1 && detail == "ok" && retry == 0 && window > 0:
		result = scriptResult{kind: resultGranted, windowID: window, leaseExpiresAtMS: expiry}
	case status == 0 && (detail == "rpm" || detail == "tpm") &&
		retry >= 1 && retry <= fixedWindowMS && window > 0 && expiry == 0:
		dimension := DimensionRequests
		if detail == "tpm" {
			dimension = DimensionTokens
		}
		result = scriptResult{kind: resultRejected, dimension: dimension, retryAfterMS: retry}
	case status == 0 && detail == "concurrency" && retry >= 1 && window > 0 && expiry == 0:
		result = scriptResult{kind: resultRejected, dimension: DimensionConcurrency, retryAfterMS: retry}
	case status == -1 && (detail == "malformed_rate_state" || detail == "malformed_concurrency_state") &&
		retry == 0 && window > 0 && expiry == 0:
		result = scriptResult{kind: resultMalformed}
	case status == -1 && (detail == "invalid_arguments" || detail == "invalid_server_time") &&
		retry == 0 && window == 0 && expiry == 0:
		result = scriptResult{kind: resultScriptFailure}
	default:
		return scriptResult{}, ErrUnexpectedResponse
	}
	if !stated || status == -1 {
		// A failure states nothing whatever was asked, since it can come before
		// the script has read what was asked.
		if len(items) != replyHeadFields {
			return scriptResult{}, ErrUnexpectedResponse
		}
		return result, nil
	}
	if len(items) != replyHeadFields+allowanceFields {
		return scriptResult{}, ErrUnexpectedResponse
	}
	rate, ok := parseAllowance(items[replyHeadFields:], window, result)
	if !ok {
		return scriptResult{}, ErrUnexpectedResponse
	}
	result.rate = rate
	return result, nil
}

// parseAllowance decodes the fields a stated reply appends to a decision and
// reports whether they fit it.
func parseAllowance(items []any, window int64, decision scriptResult) (RateState, bool) {
	var fields [allowanceFields]int64
	for index := range fields {
		counter, ok := replyInt(items[index])
		if !ok || !luaSafe(counter) {
			return RateState{}, false
		}
		fields[index] = counter
	}
	requestLimit, requestRemaining, tokenLimit, tokenRemaining, reset := fields[0], fields[1], fields[2], fields[3], fields[4]
	// A request or token limit is what gives the counts a window to be read in.
	limited := requestLimit > 0 || tokenLimit > 0
	inWindow := reset >= 1 && reset <= fixedWindowMS
	if requestRemaining > requestLimit || tokenRemaining > tokenLimit ||
		limited && (!inWindow || window >= maxWindowID) || !limited && reset != 0 {
		return RateState{}, false
	}
	switch {
	case decision.kind == resultGranted:
		// This request's own reservation is already counted, so a limit that
		// applies has room for it and no more than the limit minus that.
		if requestLimit > 0 && requestRemaining >= requestLimit || tokenLimit > 0 && tokenRemaining >= tokenLimit {
			return RateState{}, false
		}
	case decision.dimension == DimensionRequests:
		if requestLimit == 0 || requestRemaining != 0 || decision.retryAfterMS != reset {
			return RateState{}, false
		}
	case decision.dimension == DimensionTokens:
		if tokenLimit == 0 || decision.retryAfterMS != reset {
			return RateState{}, false
		}
	}
	return rateState(window, requestLimit, requestRemaining, tokenLimit, tokenRemaining, reset), true
}

// parseCostReservation decodes a reserve_cost reply, whose last two fields are
// the day and month windows the script measured against.
func parseCostReservation(value any) (scriptResult, error) {
	status, detail, retry, day, month, ok := tuple(value, costResponseVersion)
	if ok && status == -1 && retry == 0 && day == 0 && month == 0 &&
		(detail == "invalid_arguments" || detail == "invalid_server_time") {
		return scriptResult{kind: resultScriptFailure}, nil
	}
	if !ok || day < 1 || month < 1 {
		return scriptResult{}, ErrUnexpectedResponse
	}
	switch {
	case status == 1 && detail == "ok" && retry == 0:
		return scriptResult{kind: resultGranted}, nil
	case status == 0 && (detail == "daily_cost" || detail == "daily_cost_estimate") && retry >= 1 && retry <= dayMS:
		return scriptResult{
			kind: resultRejected, dimension: DimensionDailyCost, retryAfterMS: retry,
			estimate: detail == "daily_cost_estimate",
		}, nil
	case status == 0 && (detail == "monthly_cost" || detail == "monthly_cost_estimate") && retry >= 1 && retry <= maxMonthMS:
		return scriptResult{
			kind: resultRejected, dimension: DimensionMonthlyCost, retryAfterMS: retry,
			estimate: detail == "monthly_cost_estimate",
		}, nil
	case status == -1 && retry == 0 &&
		(detail == "uninitialized_daily_cost_state" || detail == "uninitialized_monthly_cost_state"):
		return scriptResult{kind: resultUninitialized}, nil
	case status == -1 && retry == 0 &&
		(detail == "malformed_daily_cost_state" || detail == "malformed_monthly_cost_state"):
		return scriptResult{kind: resultMalformed}, nil
	default:
		return scriptResult{}, ErrUnexpectedResponse
	}
}

// parseReconciliation decodes a reconcile_cost reply into the windows it
// installed.
func parseReconciliation(value any) (bool, bool, error) {
	items, ok := replyItems(value, 5)
	if !ok {
		return false, false, ErrUnexpectedResponse
	}
	version, okVersion := replyInt(items[0])
	status, okStatus := replyInt(items[1])
	detail, okDetail := replyString(items[2])
	daily, okDaily := replyInt(items[3])
	monthly, okMonthly := replyInt(items[4])
	if !okVersion || !okStatus || !okDetail || !okDaily || !okMonthly ||
		version != costResponseVersion {
		return false, false, ErrUnexpectedResponse
	}
	switch {
	case status == 1 && detail == "ok" &&
		(daily == 0 || daily == 1) && (monthly == 0 || monthly == 1):
		return daily == 1, monthly == 1, nil
	case status == -1 && daily == 0 && monthly == 0 &&
		(detail == "malformed_daily_cost_state" || detail == "malformed_monthly_cost_state"):
		return false, false, ErrMalformedState
	default:
		return false, false, ErrUnexpectedResponse
	}
}

// parseSettlement decodes a settle_cost reply and reports whether the script
// found the lease it was asked to settle. A lease that is no longer there, because
// accounting already removed it or it expired, is a settlement that correctly did
// nothing, not a failure.
func parseSettlement(value any) (bool, error) {
	items, ok := replyItems(value, 5)
	if !ok {
		return false, ErrUnexpectedResponse
	}
	version, okVersion := replyInt(items[0])
	status, okStatus := replyInt(items[1])
	detail, okDetail := replyString(items[2])
	settled, okSettled := replyInt(items[3])
	spare, okSpare := replyInt(items[4])
	if !okVersion || !okStatus || !okDetail || !okSettled || !okSpare ||
		version != costResponseVersion || spare != 0 {
		return false, ErrUnexpectedResponse
	}
	if status == 1 && detail == "ok" && (settled == 0 || settled == 1) {
		return settled == 1, nil
	}
	return false, ErrUnexpectedResponse
}
