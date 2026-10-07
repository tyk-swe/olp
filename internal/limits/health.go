package limits

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// Fleet health is two hashes keyed by provider: the circuits gateways have
// opened, each until the moment it closes on its own, and the latest result of
// each provider's active probe. Gateways read both on a short poll, so the
// state any replica publishes reaches the rest of the fleet within that bound.

// healthRetention bounds how long a fleet health hash outlives its last write,
// so an installation that stops probing leaves nothing behind.
const healthRetention = 24 * time.Hour

// ProbeResult is the content-free outcome of one active health probe.
type ProbeResult struct {
	Status     string    `json:"status"` // "healthy" or "unhealthy"
	Class      string    `json:"class,omitempty"`
	Model      string    `json:"model"`
	ObservedAt time.Time `json:"observed_at"`
	LatencyMS  int64     `json:"latency_ms"`
}

// Probe statuses.
const (
	ProbeHealthy   = "healthy"
	ProbeUnhealthy = "unhealthy"
)

// PublishCircuit records that providerID's circuit is open until the given
// moment; a zero moment closes it.
func (l *Limiter) PublishCircuit(ctx context.Context, providerID string, until time.Time) error {
	key := l.healthKey("circuits")
	if until.IsZero() {
		_, err := l.client.Do(ctx, "HDEL", key, providerID)
		return serviceError(err)
	}
	if _, err := l.client.Do(ctx, "HSET", key, providerID, strconv.FormatInt(until.UnixMilli(), 10)); err != nil {
		return &ServiceError{Err: err}
	}
	return l.retain(ctx, key)
}

// Circuits returns the providers whose fleet circuit is open at now, with the
// moment each closes. Entries that have already closed are ignored; the next
// transition for their provider replaces them.
func (l *Limiter) Circuits(ctx context.Context, now time.Time) (map[string]time.Time, error) {
	fields, err := l.hash(ctx, l.healthKey("circuits"))
	if err != nil {
		return nil, err
	}
	open := make(map[string]time.Time, len(fields))
	for provider, raw := range fields {
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, ErrUnexpectedResponse
		}
		if until := time.UnixMilli(ms); until.After(now) {
			open[provider] = until
		}
	}
	return open, nil
}

// ClaimProbe reserves the next probe of a target for the whole fleet: only
// the replica whose claim succeeds probes it before ttl passes.
func (l *Limiter) ClaimProbe(ctx context.Context, target string, ttl time.Duration) (bool, error) {
	value, err := l.client.Do(ctx, "SET", l.healthKey("probe:"+target), "1", "NX", "PX",
		strconv.FormatInt(max(ttl.Milliseconds(), 1), 10))
	if err != nil {
		return false, &ServiceError{Err: err}
	}
	return value != nil, nil
}

// RecordProbe stores the latest probe result for providerID.
func (l *Limiter) RecordProbe(ctx context.Context, providerID string, result ProbeResult) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	key := l.healthKey("probes")
	if _, err := l.client.Do(ctx, "HSET", key, providerID, string(encoded)); err != nil {
		return &ServiceError{Err: err}
	}
	return l.retain(ctx, key)
}

// Probes returns the latest probe result of every probed provider.
func (l *Limiter) Probes(ctx context.Context) (map[string]ProbeResult, error) {
	fields, err := l.hash(ctx, l.healthKey("probes"))
	if err != nil {
		return nil, err
	}
	results := make(map[string]ProbeResult, len(fields))
	for provider, raw := range fields {
		var result ProbeResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, ErrUnexpectedResponse
		}
		results[provider] = result
	}
	return results, nil
}

func (l *Limiter) healthKey(name string) string {
	return l.namespace + ":health:" + name
}

func (l *Limiter) retain(ctx context.Context, key string) error {
	_, err := l.client.Do(ctx, "PEXPIRE", key, strconv.FormatInt(healthRetention.Milliseconds(), 10))
	return serviceError(err)
}

// hash reads a whole hash, which Valkey answers as a map or, over RESP2, as
// alternating fields and values.
func (l *Limiter) hash(ctx context.Context, key string) (map[string]string, error) {
	value, err := l.client.Do(ctx, "HGETALL", key)
	if err != nil {
		return nil, &ServiceError{Err: err}
	}
	fields := map[string]string{}
	switch reply := value.(type) {
	case map[string]any:
		for field, item := range reply {
			text, ok := replyString(item)
			if !ok {
				return nil, ErrUnexpectedResponse
			}
			fields[field] = text
		}
	case []any:
		if len(reply)%2 != 0 {
			return nil, ErrUnexpectedResponse
		}
		for i := 0; i < len(reply); i += 2 {
			field, ok := replyString(reply[i])
			text, valid := replyString(reply[i+1])
			if !ok || !valid {
				return nil, ErrUnexpectedResponse
			}
			fields[field] = text
		}
	case nil:
	default:
		return nil, ErrUnexpectedResponse
	}
	return fields, nil
}

func serviceError(err error) error {
	if err != nil {
		return &ServiceError{Err: err}
	}
	return nil
}
