package attribution

import "maps"

// Policy pins operator-owned labels and requires labels at one authority boundary.
// Requirements accumulate; neither boundary can replace another boundary's pins.
type Policy struct {
	Required []string          `json:"required_attribution_keys,omitempty"`
	Defaults map[string]string `json:"attribution_defaults,omitempty"`
}

func (p Policy) Validate() error {
	if err := Validate(p.Defaults); err != nil {
		return err
	}
	seen := make(map[string]bool, len(p.Required))
	for _, key := range p.Required {
		if !KeyPattern.MatchString(key) || seen[key] {
			return &Error{Reason: "invalid_required_keys"}
		}
		seen[key] = true
	}
	for key := range p.Defaults {
		seen[key] = true
	}
	if len(seen) > MaxKeys {
		return &Error{Reason: "too_many_keys"}
	}
	return nil
}

// Resolve leaves its inputs untouched. Caller labels matching a pin are accepted;
// different values and conflicting pins fail closed. The unconfigured path
// returns the original map without allocating.
func Resolve(labels map[string]string, policies ...Policy) (map[string]string, error) {
	var pins map[string]string
	for _, p := range policies {
		for key, value := range p.Defaults {
			if previous, ok := pins[key]; ok && previous != value {
				return nil, &Error{Reason: "conflicting_defaults"}
			}
			if pins == nil {
				pins = make(map[string]string)
			}
			pins[key] = value
			if supplied, ok := labels[key]; ok && supplied != value {
				return nil, &Error{Reason: "pinned_attribution_override"}
			}
		}
	}
	result := labels
	if len(pins) != 0 {
		result = pins
		maps.Copy(result, labels)
		if err := Validate(result); err != nil {
			return nil, err
		}
	}
	for _, p := range policies {
		for _, key := range p.Required {
			if result[key] == "" {
				return nil, &Error{Reason: "missing_attribution"}
			}
		}
	}
	return result, nil
}
