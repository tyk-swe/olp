package runtime

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
)

const (
	FidelityStrict      = "strict"
	FidelityTransformed = "transformed"
)

// RouteFidelity declares the route's obligations. Native identity versus a
// qualified interaction is a per-invocation plan class, not a route-wide badge.
// Every published route carries an explicit strict or transformed mode.
type RouteFidelity struct {
	Mode string `json:"mode"`
}

// Strict reports whether the route preserves the selected target's native
// invocation. Every other valid route is transformed.
func (f RouteFidelity) Strict() bool { return f.Mode == FidelityStrict }

var errFidelityMode = errors.New("fidelity.mode must be strict or transformed")

// DecodeFidelity reads a route fidelity declaration. An omitted or null
// declaration, and an object without a mode, mean strict.
func DecodeFidelity(raw []byte) (RouteFidelity, error) {
	strict := RouteFidelity{Mode: FidelityStrict}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return strict, nil
	}
	doc, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 256, MaxDepth: 2, MaxNodes: 4})
	if err != nil || doc.Root().Kind() != oif.Object {
		return RouteFidelity{}, errors.New("fidelity must be an object with a strict or transformed mode")
	}
	for _, member := range doc.Root().Members() {
		if member.Name != "mode" {
			return RouteFidelity{}, errors.New("fidelity only accepts mode")
		}
	}
	value, ok := doc.Root().Lookup("mode")
	if !ok {
		return strict, nil
	}
	mode, valid := value.Text()
	if !valid {
		return RouteFidelity{}, errFidelityMode
	}
	f := RouteFidelity{Mode: mode}
	if err := validateFidelity(f); err != nil {
		return RouteFidelity{}, err
	}
	return f, nil
}

func validateFidelity(f RouteFidelity) error {
	switch f.Mode {
	case FidelityStrict, FidelityTransformed:
		return nil
	default:
		return errFidelityMode
	}
}

var ErrFidelityPolicyConflict = errors.New("redaction requires a transformed route; strict routes preserve input and output")

func ValidateRouteFidelity(f RouteFidelity, policy *contentpolicy.Policy) error {
	if err := validateFidelity(f); err != nil {
		return err
	}
	if err := contentpolicy.Validate(policy); err != nil {
		return err
	}
	if f.Strict() && policy != nil {
		for _, rule := range policy.Rules {
			if rule.Action == contentpolicy.ActionRedact {
				return fmt.Errorf("rule %s: %w", rule.ID, ErrFidelityPolicyConflict)
			}
		}
	}
	return nil
}
