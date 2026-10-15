package gateway

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/routing"
)

// servingAllowed constrains later Attempts to the baseline selected by the
// first admitted dispatch. A matching model alias never establishes equivalent
// accounts or serving environments. A known principal, declared or observed by
// grant enrollment, is the same for every slot of the provider revision, so
// later attempts may use its other slots; an unknown principal pins the
// credential slot. A secret refresh alone does not change a principal.
func (x *call) servingAllowed(provider *routing.Provider, targetID, model string, slot routing.Slot, selectBaseline bool) bool {
	if !x.strict() {
		return true
	}
	var serving oif.ServingIdentity
	if x.media != nil {
		template, found := x.snapshot().MediaTemplate(x.route.Slug, targetID, x.media.Op)
		if found {
			serving = template.Serving()
		}
		if !found {
			return false
		}
	} else if x.unary != nil {
		admission, err := x.unaryAdmission(provider, targetID, model)
		if err != nil {
			return false
		}
		serving = admission.receipt.Serving
	} else {
		prepared, err := x.providerAdmission(provider, targetID, model)
		if err != nil || prepared.receipt == nil {
			return false
		}
		serving = prepared.receipt.Serving
	}
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
