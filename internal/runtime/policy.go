package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
)

type Preferences struct {
	Strategy                 *string         `json:"strategy"`
	AllowFallbacks           *bool           `json:"allow_fallbacks"`
	Order                    []string        `json:"order"`
	PreferredMaxLatencyMS    *int64          `json:"preferred_max_latency_ms"`
	PreferredMinThroughput   *int64          `json:"preferred_min_throughput"`
	DenyDataCollection       *bool           `json:"deny_data_collection"`
	Ignore                   []string        `json:"ignore"`
	MaxPrice                 json.RawMessage `json:"max_price"`
	Only                     []string        `json:"only"`
	Quantizations            []string        `json:"quantizations"`
	Regions                  []string        `json:"regions"`
	RequireParameters        *bool           `json:"require_parameters"`
	RequireZeroDataRetention *bool           `json:"require_zero_data_retention"`
	MaxAttempts              *int            `json:"max_attempts,omitempty"`
}

var Strategies = []string{"weighted", "price", "latency", "throughput"}

type Policy struct {
	AllowedStrategies []string    `json:"allowed_strategies"`
	Constraints       Preferences `json:"constraints"`
	Defaults          Preferences `json:"defaults"`
}
type PriceCeiling struct {
	Input  *string `json:"input_per_million"`
	Output *string `json:"output_per_million"`
	Unit   *string `json:"unit_price"`
}

func (p *Preferences) Validate(field string) error {
	if p == nil {
		return nil
	}
	invalid := func(message string) error { return access.Invalid(field, message) }
	if p.Strategy != nil && !slices.Contains(Strategies, *p.Strategy) {
		return invalid("Unknown routing strategy")
	}
	if p.PreferredMaxLatencyMS != nil && *p.PreferredMaxLatencyMS < 0 || p.PreferredMinThroughput != nil && (*p.PreferredMinThroughput < 0) {
		return invalid("Performance preferences must be non-negative")
	}
	if p.MaxAttempts != nil && (*p.MaxAttempts < 1 || *p.MaxAttempts > 32767) {
		return invalid("max_attempts must be positive and bounded")
	}
	for _, selectors := range [][]string{p.Only, p.Ignore, p.Order} {
		if len(selectors) > 128 {
			return invalid("Use at most 128 selectors")
		}
		for _, selector := range selectors {
			kind, value, ok := strings.Cut(selector, ":")
			if !ok || value == "" || len(value) > 200 {
				return invalid("Selectors must be vendor:<catalog-id> or provider:<uuid>")
			}
			switch kind {
			case "provider":
				if _, e := uuid.Parse(value); e != nil {
					return invalid("Invalid provider selector")
				}
			case "vendor":
				for _, ch := range value {
					if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
						return invalid("Invalid vendor selector")
					}
				}
			default:
				return invalid("Unknown selector kind")
			}
		}
	}
	for _, values := range [][]string{p.Regions, p.Quantizations} {
		if len(values) > 128 {
			return invalid("Use at most 128 fact constraints")
		}
		for _, v := range values {
			if strings.TrimSpace(v) == "" || len(v) > 128 {
				return invalid("Invalid fact constraint")
			}
		}
	}
	if len(p.MaxPrice) > 0 && string(p.MaxPrice) != "null" {
		var c PriceCeiling
		d := json.NewDecoder(bytes.NewReader(p.MaxPrice))
		d.DisallowUnknownFields()
		if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
			return invalid("Invalid price ceiling")
		}
		for _, amount := range []*string{c.Input, c.Output, c.Unit} {
			if amount != nil && !decimal(*amount) {
				return invalid("Price ceilings must be non-negative decimal strings")
			}
		}
	}
	return nil
}
func decimal(s string) bool {
	whole, fraction, has := strings.Cut(s, ".")
	if len(whole) < 1 || len(whole) > 12 || has && (len(fraction) < 1 || len(fraction) > 12) {
		return false
	}
	for _, c := range whole + fraction {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func (p *Preferences) Budget(published int) int {
	if p == nil {
		return published
	}
	budget := published
	if p.MaxAttempts != nil {
		budget = min(budget, *p.MaxAttempts)
	}
	if p.AllowFallbacks != nil && !*p.AllowFallbacks && len(p.Order) == 0 {
		budget = min(budget, 1)
	}
	return budget
}
func (p *Policy) Validate() error {
	if p == nil {
		return nil
	}
	for _, s := range p.AllowedStrategies {
		if !slices.Contains(Strategies, s) {
			return access.Invalid("allowed_strategies", "Unknown routing strategy")
		}
	}
	if e := p.Constraints.Validate("constraints"); e != nil {
		return e
	}
	if e := p.Defaults.Validate("defaults"); e != nil {
		return e
	}
	c := p.Constraints
	if c.Strategy != nil || c.AllowFallbacks != nil || c.Order != nil || c.PreferredMaxLatencyMS != nil || c.PreferredMinThroughput != nil || c.MaxAttempts != nil || p.Defaults.MaxAttempts != nil {
		return access.Invalid("constraints", "Policy constraints accept hard constraints only")
	}
	return nil
}
func ParsePreferences(data []byte) (*Preferences, error) {
	if len(data) > 16384 {
		return nil, access.Invalid("preferences", "Routing preferences exceed 16 KiB")
	}
	var p *Preferences
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p == nil || d.Decode(new(any)) != io.EOF {
		return nil, access.Invalid("preferences", "Use one routing preferences JSON object with supported controls")
	}
	return p, p.Validate("preferences")
}

type EffectivePolicy struct {
	Strategy    string
	Preferences Preferences
	Constraints []Preferences
	Allowed     []string
	Digest      string
}

func ResolvePolicy(installation, route, key *Policy, request *Preferences) (EffectivePolicy, error) {
	e := EffectivePolicy{Strategy: "weighted", Allowed: slices.Clone(Strategies)}
	apply := func(p Preferences) {
		e.Constraints = append(e.Constraints, p)
		if p.Strategy != nil {
			e.Strategy = *p.Strategy
		}
		if p.Order != nil {
			e.Preferences.Order = p.Order
		}
		if p.AllowFallbacks != nil {
			e.Preferences.AllowFallbacks = p.AllowFallbacks
		}
		if p.PreferredMaxLatencyMS != nil {
			e.Preferences.PreferredMaxLatencyMS = p.PreferredMaxLatencyMS
		}
		if p.PreferredMinThroughput != nil {
			e.Preferences.PreferredMinThroughput = p.PreferredMinThroughput
		}
		if p.MaxAttempts != nil {
			e.Preferences.MaxAttempts = p.MaxAttempts
		}
	}
	for _, p := range []*Policy{installation, route, key} {
		if p == nil {
			continue
		}
		if p.AllowedStrategies != nil {
			e.Allowed = slices.DeleteFunc(e.Allowed, func(s string) bool { return !slices.Contains(p.AllowedStrategies, s) })
		}
		e.Constraints = append(e.Constraints, p.Constraints)
		apply(p.Defaults)
	}
	if request != nil {
		if err := request.Validate("preferences"); err != nil {
			return e, err
		}
		apply(*request)
	}
	if !slices.Contains(e.Allowed, e.Strategy) {
		return e, access.Invalid("strategy", "The selected strategy is forbidden by routing policy")
	}
	data, _ := json.Marshal([]any{installation, route, key, request})
	hash := sha256.Sum256(data)
	e.Digest = hex.EncodeToString(hash[:])
	return e, nil
}
func selectorMatches(selector string, p Provider) bool {
	return selector == "provider:"+p.ID || selector == "vendor:"+p.VendorID
}
func anySelector(selectors []string, p Provider) bool {
	for _, s := range selectors {
		if selectorMatches(s, p) {
			return true
		}
	}
	return false
}
func factAllowed(allowed []string, fact *string) bool {
	return allowed == nil || fact != nil && slices.Contains(allowed, *fact)
}
func required(v *bool) bool { return v != nil && *v }
func lessOrEqual(value, ceiling *string) bool {
	if ceiling == nil {
		return true
	}
	if value == nil {
		return false
	}
	a, ok := new(big.Rat).SetString(*value)
	b, ok2 := new(big.Rat).SetString(*ceiling)
	return ok && ok2 && a.Cmp(b) <= 0
}

// Constraints serialize only hard controls, as required by the closed public
// schema. Preferences share their validation but add delivery choices.
func (p Policy) MarshalJSON() ([]byte, error) {
	c := p.Constraints
	constraints := map[string]any{"deny_data_collection": required(c.DenyDataCollection), "ignore": c.Ignore, "max_price": c.MaxPrice, "only": c.Only, "quantizations": c.Quantizations, "regions": c.Regions, "require_parameters": required(c.RequireParameters), "require_zero_data_retention": required(c.RequireZeroDataRetention)}
	if c.Ignore == nil {
		constraints["ignore"] = []string{}
	}
	return json.Marshal(map[string]any{"allowed_strategies": p.AllowedStrategies, "constraints": constraints, "defaults": p.Defaults})
}

func (p Preferences) MarshalJSON() ([]byte, error) {
	type plain Preferences
	data, err := json.Marshal(plain(p))
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err = json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	fields["deny_data_collection"] = required(p.DenyDataCollection)
	fields["require_parameters"] = required(p.RequireParameters)
	fields["require_zero_data_retention"] = required(p.RequireZeroDataRetention)
	if p.Ignore == nil {
		fields["ignore"] = []string{}
	}
	return json.Marshal(fields)
}
