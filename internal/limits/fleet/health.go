package fleet

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

const healthRetention = 24 * time.Hour

type ProbeStatus string

const (
	ProbeHealthy   ProbeStatus = "healthy"
	ProbeUnhealthy ProbeStatus = "unhealthy"
)

type ProbeResult struct {
	Status     ProbeStatus `json:"status"`
	Class      string      `json:"class,omitempty"`
	Model      string      `json:"model"`
	ObservedAt time.Time   `json:"observed_at"`
	LatencyMS  int64       `json:"latency_ms"`
}

func (s *State) healthKey(name string) string { return s.scripts.Key("health:" + name) }

func (s *State) PublishCircuit(ctx context.Context, providerID string, until time.Time) error {
	key := s.healthKey("circuits")
	if until.IsZero() {
		_, err := s.scripts.Do(ctx, "HDEL", key, providerID)
		return err
	}
	return s.store(ctx, key, providerID, strconv.FormatInt(until.UnixMilli(), 10))
}

func (s *State) Circuits(ctx context.Context, now time.Time) (map[string]time.Time, error) {
	fields, err := s.hash(ctx, s.healthKey("circuits"))
	if err != nil {
		return nil, err
	}
	open := make(map[string]time.Time, len(fields))
	for provider, raw := range fields {
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, limits.ErrUnexpectedResponse
		}
		if until := time.UnixMilli(ms); until.After(now) {
			open[provider] = until
		}
	}
	return open, nil
}

func (s *State) ClaimProbe(ctx context.Context, target string, ttl time.Duration) (bool, error) {
	value, err := s.scripts.Do(ctx, "SET", s.healthKey("probe:"+target), "1", "NX", "PX",
		strconv.FormatInt(max(ttl.Milliseconds(), 1), 10))
	return value != nil, err
}

func (s *State) RecordProbe(ctx context.Context, providerID string, result ProbeResult) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.store(ctx, s.healthKey("probes"), providerID, string(encoded))
}

func (s *State) Probes(ctx context.Context) (map[string]ProbeResult, error) {
	fields, err := s.hash(ctx, s.healthKey("probes"))
	if err != nil {
		return nil, err
	}
	results := make(map[string]ProbeResult, len(fields))
	for provider, raw := range fields {
		var result ProbeResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, limits.ErrUnexpectedResponse
		}
		results[provider] = result
	}
	return results, nil
}

func (s *State) store(ctx context.Context, key, field, value string) error {
	if _, err := s.scripts.Do(ctx, "HSET", key, field, value); err != nil {
		return err
	}
	_, err := s.scripts.Do(ctx, "PEXPIRE", key, strconv.FormatInt(healthRetention.Milliseconds(), 10))
	return err
}

func (s *State) hash(ctx context.Context, key string) (map[string]string, error) {
	value, err := s.scripts.Do(ctx, "HGETALL", key)
	if err != nil {
		return nil, err
	}
	switch reply := value.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return textFields(reply)
	case []any:
		return pairedFields(reply)
	default:
		return nil, limits.ErrUnexpectedResponse
	}
}

func textFields(reply map[string]any) (map[string]string, error) {
	fields := make(map[string]string, len(reply))
	for field, item := range reply {
		text, ok := item.(string)
		if !ok {
			return nil, limits.ErrUnexpectedResponse
		}
		fields[field] = text
	}
	return fields, nil
}

func pairedFields(reply []any) (map[string]string, error) {
	if len(reply)%2 != 0 {
		return nil, limits.ErrUnexpectedResponse
	}
	fields := make(map[string]string, len(reply)/2)
	for i := 0; i < len(reply); i += 2 {
		field, ok := reply[i].(string)
		text, valid := reply[i+1].(string)
		if !ok || !valid {
			return nil, limits.ErrUnexpectedResponse
		}
		fields[field] = text
	}
	return fields, nil
}
