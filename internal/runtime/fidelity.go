package runtime

import (
	"errors"
	"fmt"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
)

const (
	FidelityLegacy      = "legacy"
	FidelityStrict      = "strict"
	FidelityTransformed = "transformed"
)

// RouteFidelity declares the route's obligations. Native identity versus a
// qualified interaction is a per-invocation plan class, not a route-wide badge.
// A nil contract is historical legacy and must remain omitted from snapshots.
type RouteFidelity struct {
	Mode string `json:"mode"`
}

func FidelityMode(f *RouteFidelity) string {
	if f == nil {
		return FidelityLegacy
	}
	return f.Mode
}

func DecodeFidelity(raw []byte) (*RouteFidelity, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	doc, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 256, MaxDepth: 2, MaxNodes: 4})
	if err != nil || doc.Root().Kind() != oif.Object {
		return nil, errors.New("fidelity must be an object; choose its mode explicitly instead of null")
	}
	for _, member := range doc.Root().Members() {
		if member.Name != "mode" {
			return nil, errors.New("fidelity only accepts mode")
		}
	}
	mode := FidelityStrict
	if value, ok := doc.Root().Lookup("mode"); ok {
		var valid bool
		mode, valid = value.Text()
		if !valid {
			return nil, errors.New("fidelity.mode must be legacy, strict, or transformed")
		}
	}
	f := &RouteFidelity{Mode: mode}
	if err := validateFidelity(f); err != nil {
		return nil, err
	}
	return f, nil
}
func validateFidelity(f *RouteFidelity) error {
	switch FidelityMode(f) {
	case FidelityLegacy, FidelityStrict, FidelityTransformed:
		return nil
	default:
		return errors.New("fidelity.mode must be legacy, strict, or transformed")
	}
}

var ErrFidelityPolicyConflict = errors.New("redaction requires a transformed route; strict routes preserve input and output")

func ValidateRouteFidelity(f *RouteFidelity, policy *contentpolicy.Policy) error {
	if err := validateFidelity(f); err != nil {
		return err
	}
	if err := contentpolicy.Validate(policy); err != nil {
		return err
	}
	if FidelityMode(f) == FidelityStrict && policy != nil {
		for _, rule := range policy.Rules {
			if rule.Action == contentpolicy.ActionRedact {
				return fmt.Errorf("rule %s: %w", rule.ID, ErrFidelityPolicyConflict)
			}
		}
	}
	return nil
}
