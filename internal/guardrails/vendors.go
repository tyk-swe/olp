package guardrails

import "github.com/tyk-swe/olp/internal/guardrails/vendor"

func isModelGuardrail(kind Kind) bool {
	return kind == "model" || kind == "semantic_guard" || kind == "decision_model" || kind == "typesafe"
}

func vendorInfo(kind Kind) (vendor.Info, bool) {
	return vendor.Lookup(vendor.Type(kind))
}

func compresses(kind Kind) bool {
	info, ok := vendorInfo(kind)
	return ok && info.Compresses
}

func supportsTransformation(kind Kind) bool {
	if info, ok := vendorInfo(kind); ok {
		return info.Transforms
	}
	switch kind {
	case "builtin.regex", "builtin.pii", "builtin.secrets", "builtin.keywords", "builtin.code_execution", "webhook", "plugin":
		return true
	}
	return false
}

func blockIf(blocked bool, category string) Verdict {
	if blocked {
		return Verdict{Action: Block, Categories: []string{category}}
	}
	return Verdict{Action: Pass}
}
