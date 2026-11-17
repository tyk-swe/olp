package providerinvoke

import (
	"cmp"
	"encoding/json"
	"errors"
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
	source := inv.Prepared.Document()
	if source.Root().Kind() != oif.Object {
		return inv, errors.New("prompt cache requires a JSON object request")
	}
	root := source.Fields()
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
		if used, insertable := breakpointBudget(source.Root(), ttl); insertable {
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
func breakpointBudget(root oif.Value, ttl string) (int, bool) {
	if _, automatic := root.Lookup("cache_control"); automatic {
		return 0, false
	}
	return cacheControls(root, ttl)
}
func cacheControls(value oif.Value, ttl string) (int, bool) {
	count := 0
	for _, member := range value.Members() {
		n, ok := memberCacheControls(member, ttl)
		if !ok {
			return 0, false
		}
		count += n
	}
	for _, element := range value.Elements() {
		n, ok := cacheControls(element, ttl)
		if !ok {
			return 0, false
		}
		count += n
	}
	return count, true
}
func memberCacheControls(member oif.Member, ttl string) (int, bool) {
	if member.Name != "cache_control" {
		return cacheControls(member.Value, ttl)
	}
	callerTTL, _ := member.Value.Lookup("ttl")
	text, _ := callerTTL.Chars()
	return 1, member.Value.Kind() == oif.Object && cmp.Or(text, "5m") == ttl
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
