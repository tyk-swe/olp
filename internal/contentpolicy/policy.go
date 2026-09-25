package contentpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"
)

const (
	PhaseInput  = "input"
	PhaseOutput = "output"

	ActionBlock  = "block"
	ActionRedact = "redact"

	OutcomeBlocked  = "blocked"
	OutcomeRedacted = "redacted"
	OutcomePassed   = "passed"

	MaxRules             = 64
	MaxPatternBytes      = 512
	MaxTotalPatternBytes = 16 << 10
	MaxReplacementChars  = 128
	DefaultReplacement   = "[REDACTED]"
	MaxDecisions         = 64
)

var RuleID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

type Rule struct {
	ID          string `json:"id"`
	Phase       string `json:"phase"`
	Pattern     string `json:"pattern"`
	Action      string `json:"action"`
	Replacement string `json:"replacement,omitempty"`
	// emptyReplacement marks an explicit empty redact replacement accepted by
	// Decode, which would otherwise re-read the omitted member as the default.
	emptyReplacement bool
}

// MarshalJSON keeps a decoded explicit empty replacement. Every other rule,
// including one read from a published snapshot, keeps its historical bytes so
// snapshot digests are unchanged.
func (r Rule) MarshalJSON() ([]byte, error) {
	type plain Rule
	if !r.emptyReplacement {
		return json.Marshal(plain(r))
	}
	return json.Marshal(struct {
		plain
		Replacement string `json:"replacement"`
	}{plain(r), r.Replacement})
}

type Policy struct {
	Rules []Rule `json:"rules"`
}

type Decision struct {
	RuleID  string `json:"rule_id"`
	Phase   string `json:"phase"`
	Action  string `json:"action"`
	Outcome string `json:"outcome"`
}

type ruleWire struct {
	ID          *string `json:"id"`
	Phase       *string `json:"phase"`
	Pattern     *string `json:"pattern"`
	Action      *string `json:"action"`
	Replacement *string `json:"replacement"`
}

type policyWire struct {
	Rules *[]ruleWire `json:"rules"`
}

func Decode(raw json.RawMessage) (*Policy, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire policyWire
	var trailing any
	if err := decoder.Decode(&wire); err != nil || decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("content_policy must be one JSON object")
	}
	if wire.Rules == nil {
		return &Policy{Rules: []Rule{}}, nil
	}
	return validateRules(*wire.Rules)
}

func validateRules(wire []ruleWire) (*Policy, error) {
	if len(wire) > MaxRules {
		return nil, fmt.Errorf("content_policy allows at most %d rules", MaxRules)
	}
	seen := map[string]bool{}
	patternBytes := 0
	rules := make([]Rule, 0, len(wire))
	for i, w := range wire {
		if w.ID == nil || !RuleID.MatchString(*w.ID) {
			return nil, fmt.Errorf("content_policy rule %d id must match %s", i, RuleID.String())
		}
		if seen[*w.ID] {
			return nil, fmt.Errorf("content_policy rule id %q is not unique", *w.ID)
		}
		seen[*w.ID] = true
		if w.Phase == nil || (*w.Phase != PhaseInput && *w.Phase != PhaseOutput) {
			return nil, fmt.Errorf("content_policy rule %q phase must be input or output", *w.ID)
		}
		if w.Action == nil || (*w.Action != ActionBlock && *w.Action != ActionRedact) {
			return nil, fmt.Errorf("content_policy rule %q action must be block or redact", *w.ID)
		}
		if w.Pattern == nil || len(*w.Pattern) < 1 || len(*w.Pattern) > MaxPatternBytes {
			return nil, fmt.Errorf("content_policy rule %q pattern must be 1-%d bytes", *w.ID, MaxPatternBytes)
		}
		re, err := regexp.Compile(*w.Pattern)
		if err != nil {
			return nil, fmt.Errorf("content_policy rule %q pattern is not a valid RE2 expression", *w.ID)
		}
		if re.MatchString("") {
			return nil, fmt.Errorf("content_policy rule %q pattern must not match the empty string", *w.ID)
		}
		patternBytes += len(*w.Pattern)
		if patternBytes > MaxTotalPatternBytes {
			return nil, fmt.Errorf("content_policy patterns may not exceed %d bytes combined", MaxTotalPatternBytes)
		}
		rule := Rule{ID: *w.ID, Phase: *w.Phase, Pattern: *w.Pattern, Action: *w.Action}
		switch *w.Action {
		case ActionBlock:
			if w.Replacement != nil {
				return nil, fmt.Errorf("content_policy rule %q must not carry a replacement for the block action", *w.ID)
			}
		case ActionRedact:
			if w.Replacement == nil {
				rule.Replacement = DefaultReplacement
			} else {
				if !utf8.ValidString(*w.Replacement) || utf8.RuneCountInString(*w.Replacement) > MaxReplacementChars {
					return nil, fmt.Errorf("content_policy rule %q replacement must be at most %d UTF-8 characters", *w.ID, MaxReplacementChars)
				}
				rule.Replacement, rule.emptyReplacement = *w.Replacement, *w.Replacement == ""
			}
		}
		rules = append(rules, rule)
	}
	return &Policy{Rules: rules}, nil
}

func Validate(p *Policy) error {
	if p == nil {
		return nil
	}
	wire := make([]ruleWire, 0, len(p.Rules))
	for i := range p.Rules {
		r := &p.Rules[i]
		replacement := &r.Replacement
		if r.Action == ActionBlock {
			replacement = nil
		}
		wire = append(wire, ruleWire{ID: &r.ID, Phase: &r.Phase, Pattern: &r.Pattern, Action: &r.Action, Replacement: replacement})
	}
	_, err := validateRules(wire)
	return err
}

type CompiledRule struct {
	Rule
	Re *regexp.Regexp
}

type Compiled struct {
	Policy *Policy
	Input  []CompiledRule
	Output []CompiledRule
}

func Compile(p *Policy) (*Compiled, error) {
	if p == nil || len(p.Rules) == 0 {
		return nil, nil
	}
	c := &Compiled{Policy: p}
	for i := range p.Rules {
		rule := &p.Rules[i]
		re, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return nil, fmt.Errorf("content_policy rule %q pattern is not a valid RE2 expression", rule.ID)
		}
		compiled := CompiledRule{Rule: *rule, Re: re}
		switch rule.Phase {
		case PhaseInput:
			c.Input = append(c.Input, compiled)
		case PhaseOutput:
			c.Output = append(c.Output, compiled)
		}
	}
	return c, nil
}

func (c *Compiled) HasInput() bool { return c != nil && len(c.Input) > 0 }

func (c *Compiled) HasOutput() bool { return c != nil && len(c.Output) > 0 }

func ValidateDecisions(decisions []Decision) error {
	if len(decisions) > MaxDecisions {
		return fmt.Errorf("policy_decisions allows at most %d entries", MaxDecisions)
	}
	for i, d := range decisions {
		if !RuleID.MatchString(d.RuleID) {
			return fmt.Errorf("policy_decisions %d rule_id is malformed", i)
		}
		if d.Phase != PhaseInput && d.Phase != PhaseOutput {
			return fmt.Errorf("policy_decisions %d phase is malformed", i)
		}
		if d.Action != ActionBlock && d.Action != ActionRedact {
			return fmt.Errorf("policy_decisions %d action is malformed", i)
		}
		if d.Outcome != OutcomeBlocked && d.Outcome != OutcomeRedacted && d.Outcome != OutcomePassed {
			return fmt.Errorf("policy_decisions %d outcome is malformed", i)
		}
	}
	return nil
}

func DecisionsJSON(decisions []Decision) []byte {
	if len(decisions) == 0 {
		return []byte("[]")
	}
	encoded, err := json.Marshal(decisions)
	if err != nil {
		return []byte("[]")
	}
	return encoded
}
