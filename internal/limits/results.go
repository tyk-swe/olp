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
// has been spent.
type scriptResult struct {
	kind             resultKind
	dimension        Dimension
	retryAfterMS     int64
	windowID         int64
	leaseExpiresAtMS int64
	estimate         bool
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

// parseReservation decodes a reserve_limits reply.
func parseReservation(value any) (scriptResult, error) {
	status, detail, retry, window, expiry, ok := tuple(value, limitsResponseVersion)
	if !ok {
		return scriptResult{}, ErrUnexpectedResponse
	}
	switch {
	case status == 1 && detail == "ok" && retry == 0 && window > 0:
		return scriptResult{kind: resultGranted, windowID: window, leaseExpiresAtMS: expiry}, nil
	case status == 0 && (detail == "rpm" || detail == "tpm") &&
		retry >= 1 && retry <= fixedWindowMS && window > 0 && expiry == 0:
		dimension := DimensionRequests
		if detail == "tpm" {
			dimension = DimensionTokens
		}
		return scriptResult{kind: resultRejected, dimension: dimension, retryAfterMS: retry}, nil
	case status == 0 && detail == "concurrency" && retry >= 1 && window > 0 && expiry == 0:
		return scriptResult{
			kind:         resultRejected,
			dimension:    DimensionConcurrency,
			retryAfterMS: retry,
		}, nil
	case status == -1 && (detail == "malformed_rate_state" || detail == "malformed_concurrency_state") &&
		retry == 0 && window > 0 && expiry == 0:
		return scriptResult{kind: resultMalformed}, nil
	case status == -1 && (detail == "invalid_arguments" || detail == "invalid_server_time") &&
		retry == 0 && window == 0 && expiry == 0:
		return scriptResult{kind: resultScriptFailure}, nil
	default:
		return scriptResult{}, ErrUnexpectedResponse
	}
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
