package providerinvoke

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

// ApplyPromptCache changes only omitted provider controls. It runs as part of
// transformed preparation, before input inspection and token estimation.
func ApplyPromptCache(inv Invocation, config connectors.Config, policy *cache.PromptPolicy, key string) (Invocation, error) {
	if policy == nil {
		return inv, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(inv.Prepared.Document().Bytes(), &root); err != nil {
		return inv, err
	}
	changed := false
	if (inv.Wire == wire.Chat || inv.Wire == wire.Responses) && (config.Kind == "openai" || config.Kind == "azure_openai") {
		if _, exists := root["prompt_cache_key"]; !exists && key != "" {
			root["prompt_cache_key"], _ = json.Marshal(key)
			changed = true
		}
	} else if inv.Wire == wire.Anthropic && (config.Kind == "anthropic" || config.Kind == "vertex_ai" || config.Kind == "bedrock") {
		positions := policy.Positions
		if len(positions) == 0 {
			positions = []string{"system", "tools", "last_user"}
		}
		ttl := policy.TTL
		if ttl == "" {
			ttl = "5m"
		}
		control, _ := json.Marshal(map[string]string{"type": "ephemeral", "ttl": ttl})
		// Respect caller-owned breakpoints and the provider's four-breakpoint
		// limit. A caller's top-level automatic policy owns all insertion.
		if _, exists := root["cache_control"]; !exists && compatibleCacheTTLs(root, ttl) {
			used := countCacheControls(root)
			if slices.Contains(positions, "tools") && used < 4 {
				var blocks []map[string]json.RawMessage
				if json.Unmarshal(root["tools"], &blocks) == nil && markLast(blocks, control) {
					root["tools"], _ = json.Marshal(blocks)
					changed = true
					used++
				}
			}
			if slices.Contains(positions, "system") && used < 4 {
				var blocks []map[string]json.RawMessage
				var text string
				if json.Unmarshal(root["system"], &text) == nil && text != "" {
					raw, _ := json.Marshal(text)
					blocks = []map[string]json.RawMessage{{"type": json.RawMessage(`"text"`), "text": raw}}
				} else {
					_ = json.Unmarshal(root["system"], &blocks)
				}
				if markLast(blocks, control) {
					root["system"], _ = json.Marshal(blocks)
					changed = true
					used++
				}
			}
			if slices.Contains(positions, "last_user") && used < 4 {
				var messages []map[string]json.RawMessage
				if json.Unmarshal(root["messages"], &messages) == nil {
					for i := len(messages) - 1; i >= 0; i-- {
						if string(messages[i]["role"]) != `"user"` {
							continue
						}
						var blocks []map[string]json.RawMessage
						var text string
						if json.Unmarshal(messages[i]["content"], &text) == nil {
							raw, _ := json.Marshal(text)
							blocks = []map[string]json.RawMessage{{"type": json.RawMessage(`"text"`), "text": raw}}
						} else {
							_ = json.Unmarshal(messages[i]["content"], &blocks)
						}
						if markLast(blocks, control) {
							messages[i]["content"], _ = json.Marshal(blocks)
							root["messages"], _ = json.Marshal(messages)
							changed = true
						}
						break
					}
				}
			}
		}
	}
	if !changed {
		return inv, nil
	}
	body, err := json.Marshal(root)
	if err != nil {
		return inv, err
	}
	doc, err := oif.ParseJSON(body, inv.Prepared.Document().Limits())
	if err != nil {
		return inv, err
	}
	p, err := oif.PrepareDestination(inv.Prepared.Request(), inv.Prepared.Descriptor(), doc, oif.ExplicitTransform, "declared provider prompt-cache policy")
	if err != nil {
		return inv, err
	}
	inv.Prepared = p.WithProvenance(inv.Prepared.Provenance()...)
	return inv, nil
}

// Mixing TTLs requires ordering the long-lived breakpoints before the short
// ones. Leave a caller's different TTL entirely in charge instead of inserting
// a breakpoint that could invalidate that ordering.
func compatibleCacheTTLs(root map[string]json.RawMessage, ttl string) bool {
	var value any
	raw, _ := json.Marshal(root)
	_ = json.Unmarshal(raw, &value)
	var compatible func(any) bool
	compatible = func(value any) bool {
		switch value := value.(type) {
		case map[string]any:
			for name, child := range value {
				if name == "cache_control" {
					control, ok := child.(map[string]any)
					if !ok {
						return false
					}
					callerTTL, _ := control["ttl"].(string)
					if callerTTL == "" {
						callerTTL = "5m"
					}
					if callerTTL != ttl {
						return false
					}
				} else if !compatible(child) {
					return false
				}
			}
		case []any:
			for _, child := range value {
				if !compatible(child) {
					return false
				}
			}
		}
		return true
	}
	return compatible(value)
}
func markLast(blocks []map[string]json.RawMessage, control json.RawMessage) bool {
	if len(blocks) == 0 {
		return false
	}
	last := blocks[len(blocks)-1]
	if last == nil {
		return false
	}
	if _, ok := last["cache_control"]; ok {
		return false
	}
	last["cache_control"] = control
	return true
}
func countCacheControls(root map[string]json.RawMessage) int {
	var value any
	raw, _ := json.Marshal(root)
	_ = json.Unmarshal(raw, &value)
	var walk func(any) int
	walk = func(v any) int {
		n := 0
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if k == "cache_control" {
					n++
				} else {
					n += walk(x)
				}
			}
		case []any:
			for _, x := range v {
				n += walk(x)
			}
		}
		return n
	}
	return walk(value)
}
