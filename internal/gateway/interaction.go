package gateway

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func (st *attemptState) upstreamState() string {
	switch st.upstream.Load() {
	case 1:
		return usage.UpstreamUnknown
	case 2:
		return usage.UpstreamAccepted
	case 3:
		return usage.UpstreamTerminal
	default:
		return usage.UpstreamNotSent
	}
}

// servingAllowed constrains later Attempts to the baseline selected by the
// first admitted dispatch. A matching model alias never establishes equivalent
// accounts or serving environments. Unknown principal identity also pins the
// credential slot; a secret refresh alone does not change a declared principal.
func (x *execution) servingAllowed(provider *runtime.Provider, model string, slot runtime.Slot, selectBaseline bool) bool {
	if !x.strict() {
		return true
	}
	prepared, err := x.preparedProvider(provider, model)
	if err != nil || prepared.plan == nil {
		return false
	}
	serving := prepared.plan.Serving()
	if x.serving == nil {
		if selectBaseline {
			x.serving = &serving
			x.servingSlot = slot.ID
			x.servingBinding = model
		}
		return true
	}
	return serving == *x.serving && model == x.servingBinding && (serving.PrincipalID != "" || slot.ID == x.servingSlot)
}

// eventActionable recognizes native tool representations before the admitted
// projection writes them. It does not inspect arbitrary text for tool names or
// infer that a client will wait for complete argument fragments.
func eventActionable(event oif.Event) bool {
	root := event.Source().Root()
	typeOf := func(value oif.Value) string {
		kind, _ := value.Lookup("type")
		text, _ := kind.Text()
		return text
	}
	for _, container := range []string{"content_block", "item", "delta"} {
		value, _ := root.Lookup(container)
		switch typeOf(value) {
		case "tool_use", "server_tool_use", "function_call", "input_json_delta":
			return true
		}
	}
	if choices, ok := root.Lookup("choices"); ok {
		for _, choice := range choices.Elements() {
			delta, _ := choice.Lookup("delta")
			if calls, _ := delta.Lookup("tool_calls"); len(calls.Elements()) > 0 {
				return true
			}
		}
	}
	if candidates, ok := root.Lookup("candidates"); ok {
		for _, candidate := range candidates.Elements() {
			content, _ := candidate.Lookup("content")
			parts, _ := content.Lookup("parts")
			for _, part := range parts.Elements() {
				if _, ok := part.Lookup("functionCall"); ok {
					return true
				}
			}
		}
	}
	if block, ok := root.Lookup("contentBlockStart"); ok {
		start, _ := block.Lookup("start")
		if _, ok := start.Lookup("toolUse"); ok {
			return true
		}
	}
	return event.Name() == "response.function_call_arguments.delta"
}
