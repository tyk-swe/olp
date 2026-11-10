package scripts

import (
	"context"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

type Share struct {
	Class             string
	Percent           int64
	SaturationPercent int64
}

type RateReservation struct {
	Lookup            string
	LeaseID           string
	RequestsPerMinute int64
	TokensPerMinute   int64
	MaxConcurrency    int64
	TrackConcurrency  bool
	Tokens            int64
	TTL               time.Duration
	Stated            bool
	Share             *Share
}

type RateGrant struct {
	WindowID         int64
	HoldsConcurrency bool
	Rate             limits.RateState
}

type RateHold struct {
	Lookup   string
	LeaseID  string
	WindowID int64
	Class    string
}

func (r Runner) rateKeys(lookup string) []string {
	return []string{r.Key("{" + lookup + "}:rate"), r.Key("{" + lookup + "}:concurrency")}
}

func (q RateReservation) args() []string {
	base := []string{
		strconv.FormatInt(q.RequestsPerMinute, 10), strconv.FormatInt(q.TokensPerMinute, 10),
		strconv.FormatInt(q.Tokens, 10), strconv.FormatInt(q.MaxConcurrency, 10),
		q.LeaseID, strconv.FormatInt(q.TTL.Milliseconds(), 10), switchArg(q.Stated),
	}
	shared := base
	if q.Share != nil {
		shared = append(base, q.Share.Class, strconv.FormatInt(q.Share.Percent, 10),
			strconv.FormatInt(q.Share.SaturationPercent, 10))
	}
	if q.TrackConcurrency {
		return append(shared, "1")
	}
	return shared
}

func (r Runner) ReserveRate(ctx context.Context, q RateReservation) (RateGrant, error) {
	value, err := r.run(ctx, call{script: reserveLimits, keys: r.rateKeys(q.Lookup), args: q.args()})
	if err != nil {
		return RateGrant{}, err
	}
	return decodeRate(value, q)
}

func decodeRate(value any, q RateReservation) (RateGrant, error) {
	items, _ := value.([]any)
	h, ok := decodeHead(items, limitsResponseVersion)
	if !ok {
		return RateGrant{}, limits.ErrUnexpectedResponse
	}
	window, expiry := h.first, h.second
	failed := h.status == -1 && h.retry == 0 && expiry == 0
	switch {
	case failed && window > 0 && (h.detail == "malformed_rate_state" || h.detail == "malformed_concurrency_state"):
		return RateGrant{}, exactly(items, headFields, limits.ErrMalformedState)
	case failed && window == 0 && (h.detail == "invalid_arguments" || h.detail == "invalid_server_time"):
		return RateGrant{}, limits.ErrUnexpectedResponse
	}
	dimension, ok := rateDimension(h)
	if !ok {
		return RateGrant{}, limits.ErrUnexpectedResponse
	}
	rate, ok := decodeAllowance(items[headFields:], q, h, dimension)
	if !ok {
		return RateGrant{}, limits.ErrUnexpectedResponse
	}
	if dimension != "" {
		return RateGrant{}, &limits.ExceededError{
			Dimension: dimension, RetryAfter: time.Duration(h.retry) * time.Millisecond, Rate: rate,
		}
	}
	if (q.MaxConcurrency > 0 || q.TrackConcurrency) != (expiry > 0) {
		return RateGrant{}, limits.ErrUnexpectedResponse
	}
	return RateGrant{WindowID: window, HoldsConcurrency: expiry > 0, Rate: rate}, nil
}

func exactly(items []any, size int, err error) error {
	if len(items) != size {
		return limits.ErrUnexpectedResponse
	}
	return err
}

func rateDimension(h head) (limits.Dimension, bool) {
	window, expiry := h.first, h.second
	rejected := h.status == 0 && h.retry >= 1 && window > 0 && expiry == 0
	switch {
	case h.status == 1 && h.detail == "ok" && h.retry == 0 && window > 0:
		return "", true
	case rejected && h.detail == "rpm" && h.retry <= fixedWindowMS:
		return limits.DimensionRequests, true
	case rejected && h.detail == "tpm" && h.retry <= fixedWindowMS:
		return limits.DimensionTokens, true
	case rejected && h.detail == "concurrency":
		return limits.DimensionConcurrency, true
	default:
		return "", false
	}
}

func decodeAllowance(items []any, q RateReservation, h head, rejected limits.Dimension) (limits.RateState, bool) {
	if !q.Stated {
		return limits.RateState{}, len(items) == 0
	}
	if len(items) != allowanceFields {
		return limits.RateState{}, false
	}
	var fields [allowanceFields]int64
	for index := range fields {
		counter, ok := replyInt(items[index])
		if !ok || !luaSafe(counter) {
			return limits.RateState{}, false
		}
		fields[index] = counter
	}
	requestLimit, requestRemaining, tokenLimit, tokenRemaining, reset := fields[0], fields[1], fields[2], fields[3], fields[4]
	window := h.first
	limited := requestLimit > 0 || tokenLimit > 0
	inWindow := reset >= 1 && reset <= fixedWindowMS
	if requestRemaining > requestLimit || tokenRemaining > tokenLimit ||
		limited && (!inWindow || window >= maxWindowID) || !limited && reset != 0 ||
		requestLimit != q.RequestsPerMinute || tokenLimit != q.TokensPerMinute {
		return limits.RateState{}, false
	}
	switch rejected {
	case "":
		if requestLimit > 0 && requestRemaining >= requestLimit || tokenLimit > 0 && tokenRemaining >= tokenLimit {
			return limits.RateState{}, false
		}
	case limits.DimensionRequests:
		if requestLimit == 0 || requestRemaining != 0 || h.retry != reset {
			return limits.RateState{}, false
		}
	case limits.DimensionTokens:
		if tokenLimit == 0 || h.retry != reset {
			return limits.RateState{}, false
		}
	}
	if !limited {
		return limits.RateState{}, true
	}
	return limits.RateState{
		RequestLimit:     requestLimit,
		RequestRemaining: requestRemaining,
		TokenLimit:       tokenLimit,
		TokenRemaining:   tokenRemaining,
		ResetAfter:       time.Duration(reset) * time.Millisecond,
		ResetAt:          time.UnixMilli((window + 1) * fixedWindowMS).UTC(),
	}, true
}

func (h RateHold) classed(args ...string) []string {
	if h.Class != "" {
		return append(args, h.Class)
	}
	return args
}

func (r Runner) RefundRate(ctx context.Context, h RateHold, request bool, tokens int64) error {
	_, err := r.run(ctx, call{script: refundLimits, keys: r.rateKeys(h.Lookup),
		args: h.classed(strconv.FormatInt(h.WindowID, 10), switchArg(request), strconv.FormatInt(tokens, 10), h.LeaseID)})
	return err
}

func (r Runner) AddTokens(ctx context.Context, h RateHold, adjustment int64) error {
	_, err := r.run(ctx, call{script: reconcileLimits, keys: r.rateKeys(h.Lookup)[:1],
		args: h.classed(strconv.FormatInt(h.WindowID, 10), strconv.FormatInt(adjustment, 10), h.LeaseID)})
	return err
}

func (r Runner) ReleaseConcurrency(ctx context.Context, h RateHold) error {
	_, err := r.run(ctx, call{script: releaseConcurrency, keys: r.rateKeys(h.Lookup)[1:], args: []string{h.LeaseID}})
	return err
}

type Usage struct {
	RequestsThisMinute int64
	TokensThisMinute   int64
	ConcurrentRequests int64
}

func (r Runner) usageCall(lookup string) call {
	return call{script: providerUsage, keys: r.rateKeys(lookup)}
}

func (r Runner) Usage(ctx context.Context, lookup string) (Usage, error) {
	value, err := r.run(ctx, r.usageCall(lookup))
	if err != nil {
		return Usage{}, err
	}
	return decodeUsage(value)
}

func decodeUsage(value any) (Usage, error) {
	items, ok := value.([]any)
	if !ok || len(items) != 3 {
		return Usage{}, limits.ErrUnexpectedResponse
	}
	var counters [3]int64
	for index := range counters {
		counter, ok := replyInt(items[index])
		if !ok || !luaSafe(counter) {
			return Usage{}, limits.ErrUnexpectedResponse
		}
		counters[index] = counter
	}
	return Usage{RequestsThisMinute: counters[0], TokensThisMinute: counters[1], ConcurrentRequests: counters[2]}, nil
}

func (r Runner) Cooldown(ctx context.Context, key string, d time.Duration) error {
	milliseconds := min(max(d.Milliseconds(), 1), dayMS)
	_, err := r.run(ctx, call{script: providerCooldown, keys: []string{key}, args: []string{strconv.FormatInt(milliseconds, 10)}})
	return err
}
