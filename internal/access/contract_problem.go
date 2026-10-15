package access

import "errors"

// ContractProblem reports an obligation a strict route's target cannot meet as a
// 422, or returns nil when err is not such an obligation. A refusal that exists
// only because the target has no provider profile, its profile is not strict or
// it needs translation tells the author to declare the route transformed.
func ContractProblem(err error) *Problem {
	var incompatible interface {
		Incompatibility() (code, field, requirement, message string)
	}
	if !errors.As(err, &incompatible) {
		return nil
	}
	code, _, requirement, message := incompatible.Incompatibility()
	switch requirement {
	case "explicit_profile":
		message += " Declare the route transformed to use a provider without a profile."
	case "strict_profile":
		message += " Declare the route transformed to use this profile."
	case "operation_contract", "native_media_contract", "native_batch_contract", "native_realtime_contract":
		message += " Declare the route transformed to translate for this target."
	}
	return &Problem{Status: 422, Code: code, Detail: message}
}
