package protocols

import (
	"math"
	"strconv"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
)

// BedrockUsageObservation preserves count presence. Zero is an observation;
// omission does not prove that a generation consumed no tokens.
type BedrockUsageObservation struct {
	Value                                     *wire.Usage
	InputPresent, OutputPresent, TotalPresent bool
	Authoritative                             bool
}

type bedrockUsageNames struct{ input, output, read, write, total string }

var (
	converseUsageNames   = bedrockUsageNames{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheWriteInputTokens", "totalTokens"}
	invocationUsageNames = bedrockUsageNames{"inputTokenCount", "outputTokenCount", "cacheReadInputTokenCount", "cacheWriteInputTokenCount", ""}
	anthropicUsageNames  = bedrockUsageNames{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", ""}
)

func observedBedrockCount(value oif.Value, name string, nullable bool) (int64, bool, error) {
	if name == "" {
		return 0, false, nil
	}
	field, present := value.Lookup(name)
	if !present || nullable && field.Kind() == oif.Null {
		return 0, false, nil
	}
	if field.Kind() != oif.Number {
		return 0, false, protocolError("invalid Bedrock usage count")
	}
	count, err := strconv.ParseInt(field.Raw(), 10, 64)
	if err != nil || count < 0 {
		return 0, false, protocolError("invalid Bedrock usage count")
	}
	return count, true, nil
}

func observedBedrockUsage(value oif.Value, names bedrockUsageNames, anthropic bool) (BedrockUsageObservation, error) {
	var result BedrockUsageObservation
	if value.Kind() == oif.Absent || value.Kind() == oif.Null {
		return result, nil
	}
	if value.Kind() != oif.Object {
		return result, protocolError("invalid Bedrock usage")
	}
	input, hasInput, err := observedBedrockCount(value, names.input, false)
	if err != nil {
		return result, err
	}
	output, hasOutput, err := observedBedrockCount(value, names.output, false)
	if err != nil {
		return result, err
	}
	read, hasRead, err := observedBedrockCount(value, names.read, true)
	if err != nil {
		return result, err
	}
	write, hasWrite, err := observedBedrockCount(value, names.write, true)
	if err != nil {
		return result, err
	}
	total, hasTotal, err := observedBedrockCount(value, names.total, true)
	if err != nil {
		return result, err
	}
	u := &wire.Usage{OutputTokens: output}
	if hasRead {
		u.CachedInputTokens = &read
	}
	if hasWrite {
		u.CacheWriteInputTokens = &write
	}
	if err := observedBedrockCacheDetails(value, u, anthropic); err != nil {
		return result, err
	}
	if read > math.MaxInt64-input || write > math.MaxInt64-input-read || output > math.MaxInt64-input-read-write {
		return result, protocolError("usage count overflow")
	}
	u.InputTokens = input + read + write
	u.TotalTokens = u.InputTokens + output
	if hasTotal {
		u.TotalTokens = total
	}
	if !hasInput && !hasOutput && !hasRead && !hasWrite && !hasTotal {
		return result, nil
	}
	result.Value, result.InputPresent, result.OutputPresent, result.TotalPresent = u, hasInput, hasOutput, hasTotal
	return result, nil
}

func observedBedrockCacheDetails(value oif.Value, usage *wire.Usage, anthropic bool) error {
	if anthropic {
		creation, present := value.Lookup("cache_creation")
		if present && creation.Kind() != oif.Null {
			if creation.Kind() != oif.Object {
				return protocolError("invalid Bedrock cache creation detail")
			}
			short, hasShort, err := observedBedrockCount(creation, "ephemeral_5m_input_tokens", true)
			if err != nil {
				return err
			}
			long, hasLong, err := observedBedrockCount(creation, "ephemeral_1h_input_tokens", true)
			if err != nil {
				return err
			}
			if hasShort {
				usage.CacheWrite5MInputTokens = &short
			}
			if hasLong {
				usage.CacheWrite1HInputTokens = &long
			}
		}
	} else {
		details, present := value.Lookup("cacheDetails")
		if present && details.Kind() != oif.Null {
			if details.Kind() != oif.Array {
				return protocolError("invalid Bedrock cache details")
			}
			for _, detail := range details.Elements() {
				if detail.Kind() != oif.Object {
					return protocolError("invalid Bedrock cache detail")
				}
				ttlValue, _ := detail.Lookup("ttl")
				ttl, _ := ttlValue.Chars()
				count, present, err := observedBedrockCount(detail, "inputTokens", false)
				if err != nil || !present {
					return protocolError("invalid Bedrock cache detail count")
				}
				switch ttl {
				case "5m":
					if usage.CacheWrite5MInputTokens != nil {
						return protocolError("duplicate Bedrock cache TTL")
					}
					usage.CacheWrite5MInputTokens = &count
				case "1h":
					if usage.CacheWrite1HInputTokens != nil {
						return protocolError("duplicate Bedrock cache TTL")
					}
					usage.CacheWrite1HInputTokens = &count
				default:
					return protocolError("unknown Bedrock cache TTL")
				}
			}
		}
	}
	short, long := bedrockOptionalCount(usage.CacheWrite5MInputTokens), bedrockOptionalCount(usage.CacheWrite1HInputTokens)
	if short > math.MaxInt64-long || short+long > bedrockOptionalCount(usage.CacheWriteInputTokens) {
		return protocolError("Bedrock cache write detail exceeds total")
	}
	if usage.CacheWriteInputTokens == nil && (usage.CacheWrite5MInputTokens != nil || usage.CacheWrite1HInputTokens != nil) {
		return protocolError("Bedrock cache write detail without total")
	}
	return nil
}

func bedrockOptionalCount(count *int64) int64 {
	if count == nil {
		return 0
	}
	return *count
}

// Merge keeps unnamed categories and adopts explicit zero counts. Authoritative
// metrics replace earlier partials. Native totalTokens is retained verbatim;
// canonical input includes cache categories that Bedrock reports separately.
func (observed BedrockUsageObservation) Merge(previous *wire.Usage) (*wire.Usage, error) {
	if observed.Value == nil {
		return previous, nil
	}
	if previous == nil {
		return observed.Value, nil
	}
	if observed.Authoritative {
		if previous.CachedInputTokens == nil && previous.CacheWriteInputTokens == nil &&
			previous.CacheWrite5MInputTokens == nil && previous.CacheWrite1HInputTokens == nil {
			return observed.Value, nil
		}
		merged := *observed.Value
		// Summary counts replace the aggregate, while unnamed cache categories
		// retain already observed billing evidence from the native message.
		if merged.CachedInputTokens == nil {
			merged.CachedInputTokens = previous.CachedInputTokens
		}
		if merged.CacheWriteInputTokens == nil {
			merged.CacheWriteInputTokens = previous.CacheWriteInputTokens
		}
		if merged.CacheWriteInputTokens != nil && previous.CacheWriteInputTokens != nil &&
			*merged.CacheWriteInputTokens == *previous.CacheWriteInputTokens {
			if merged.CacheWrite5MInputTokens == nil {
				merged.CacheWrite5MInputTokens = previous.CacheWrite5MInputTokens
			}
			if merged.CacheWrite1HInputTokens == nil {
				merged.CacheWrite1HInputTokens = previous.CacheWrite1HInputTokens
			}
		}
		read, write := bedrockOptionalCount(merged.CachedInputTokens), bedrockOptionalCount(merged.CacheWriteInputTokens)
		if read > merged.InputTokens || write > merged.InputTokens-read {
			return nil, protocolError("Bedrock cache categories exceed input total")
		}
		short, long := bedrockOptionalCount(merged.CacheWrite5MInputTokens), bedrockOptionalCount(merged.CacheWrite1HInputTokens)
		if short > math.MaxInt64-long || short+long > write {
			return nil, protocolError("Bedrock cache write detail exceeds total")
		}
		return &merged, nil
	}
	merged := *previous
	input := merged.InputTokens - bedrockOptionalCount(merged.CachedInputTokens) - bedrockOptionalCount(merged.CacheWriteInputTokens)
	if observed.InputPresent {
		input = observed.Value.InputTokens - bedrockOptionalCount(observed.Value.CachedInputTokens) - bedrockOptionalCount(observed.Value.CacheWriteInputTokens)
	}
	if observed.OutputPresent {
		merged.OutputTokens = observed.Value.OutputTokens
	}
	if observed.Value.CachedInputTokens != nil {
		merged.CachedInputTokens = observed.Value.CachedInputTokens
	}
	if observed.Value.CacheWriteInputTokens != nil {
		merged.CacheWriteInputTokens = observed.Value.CacheWriteInputTokens
	}
	if observed.Value.CacheWrite5MInputTokens != nil {
		merged.CacheWrite5MInputTokens = observed.Value.CacheWrite5MInputTokens
	}
	if observed.Value.CacheWrite1HInputTokens != nil {
		merged.CacheWrite1HInputTokens = observed.Value.CacheWrite1HInputTokens
	}
	read, write := bedrockOptionalCount(merged.CachedInputTokens), bedrockOptionalCount(merged.CacheWriteInputTokens)
	if input < 0 || read > math.MaxInt64-input || write > math.MaxInt64-input-read || merged.OutputTokens > math.MaxInt64-input-read-write {
		return nil, protocolError("usage count overflow")
	}
	merged.InputTokens = input + read + write
	merged.TotalTokens = merged.InputTokens + merged.OutputTokens
	if observed.TotalPresent {
		merged.TotalTokens = observed.Value.TotalTokens
	}
	short, long := bedrockOptionalCount(merged.CacheWrite5MInputTokens), bedrockOptionalCount(merged.CacheWrite1HInputTokens)
	if short > math.MaxInt64-long || short+long > write {
		return nil, protocolError("Bedrock cache write detail exceeds total")
	}
	return &merged, nil
}
