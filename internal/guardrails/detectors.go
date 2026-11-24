package guardrails

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
)

const (
	maxPatternRules      = 64
	maxPatternBytes      = 512
	maxTotalPatternBytes = 16 << 10
	maxReplacementChars  = 128
	defaultReplacement   = "[REDACTED]"
)

var patternID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

type Pattern struct {
	ID          string  `json:"id"`
	Pattern     string  `json:"pattern"`
	Replacement *string `json:"replacement,omitempty"`
}
type RegexConfig struct {
	Rules []Pattern `json:"rules"`
}
type EntityConfig struct {
	Categories  []string  `json:"categories,omitempty"`
	Patterns    []Pattern `json:"patterns,omitempty"`
	Replacement string    `json:"replacement,omitempty"`
}
type SchemaConfig struct {
	Schema json.RawMessage `json:"schema"`
}
type ToolsConfig struct {
	Allow      []string                   `json:"allow,omitempty"`
	Deny       []string                   `json:"deny,omitempty"`
	Privileged []string                   `json:"privileged,omitempty"`
	Untrusted  []string                   `json:"untrusted,omitempty"`
	Schemas    map[string]json.RawMessage `json:"schemas,omitempty"`
}
type patternDetector struct {
	patterns []entityPattern
	maxWidth int
}
type entityPattern struct {
	re                    *regexp.Regexp
	category, replacement string
	check                 func(string) bool
}

func (p *patternDetector) width() int { return p.maxWidth }

func compileRegex(raw json.RawMessage) (*patternDetector, error) {
	var config RegexConfig
	if err := decodeConfig(raw, &config); err != nil {
		return nil, err
	}
	if len(config.Rules) > maxPatternRules {
		return nil, errors.New("too many regex rules")
	}
	return compilePatterns(config.Rules, "regex", "")
}

func compileEntities(available map[string]entityTemplate) func(json.RawMessage) (*patternDetector, error) {
	return func(raw json.RawMessage) (*patternDetector, error) {
		var config EntityConfig
		if err := decodeConfig(raw, &config); err != nil {
			return nil, err
		}
		categories := config.Categories
		if len(categories) == 0 {
			categories = slices.Sorted(maps.Keys(available))
		}
		if len(categories) > 32 || len(config.Replacement) > 512 || !utf8.ValidString(config.Replacement) {
			return nil, errors.New("invalid entity configuration")
		}
		replacement := cmp.Or(config.Replacement, "[REDACTED]")
		builtins := make([]entityPattern, 0, len(categories))
		for _, name := range categories {
			t, ok := available[name]
			if !ok {
				return nil, errors.New("unknown detector category")
			}
			builtins = append(builtins, entityPattern{re: t.re, category: name, replacement: replacement, check: t.check})
		}
		custom, err := compilePatterns(config.Patterns, "pii.custom", config.Replacement)
		if err != nil {
			return nil, err
		}
		patterns := slices.Concat(builtins, custom.patterns)
		return &patternDetector{patterns: patterns, maxWidth: maximumPatternWidth(patterns)}, nil
	}
}

func compileSchemaDetector(raw json.RawMessage) (*schemaDetector, error) {
	var config SchemaConfig
	if err := decodeConfig(raw, &config); err != nil {
		return nil, err
	}
	schema, err := compileSchema(config.Schema)
	if err != nil {
		return nil, err
	}
	return &schemaDetector{schema: schema}, nil
}

func compileTools(raw json.RawMessage) (*toolsDetector, error) {
	var config ToolsConfig
	if err := decodeConfig(raw, &config); err != nil {
		return nil, err
	}
	if len(config.Allow)+len(config.Deny)+len(config.Privileged)+len(config.Untrusted) > 256 || len(config.Schemas) > 128 {
		return nil, errors.New("too many tool rules")
	}
	for _, group := range [][]string{config.Allow, config.Deny, config.Privileged, config.Untrusted} {
		for _, g := range group {
			if len(g) < 1 || len(g) > 128 {
				return nil, errors.New("invalid tool glob")
			}
			if _, err := path.Match(g, ""); err != nil {
				return nil, errors.New("invalid tool glob")
			}
		}
	}
	schemas := make(map[string]*jsonschema.Schema, len(config.Schemas))
	for name, raw := range config.Schemas {
		if !guardrailpolicy.ValidIdentifier(name) {
			return nil, errors.New("invalid tool name")
		}
		schema, err := compileSchema(raw)
		if err != nil {
			return nil, err
		}
		schemas[name] = schema
	}
	return &toolsDetector{config: config, schemas: schemas}, nil
}

func compilePatterns(rules []Pattern, category, replacement string) (*patternDetector, error) {
	p := &patternDetector{}
	total := 0
	seen := map[string]bool{}
	if len(rules) > maxPatternRules {
		return nil, errors.New("too many entity patterns")
	}
	for _, r := range rules {
		total += len(r.Pattern)
		if !patternID.MatchString(r.ID) || seen[r.ID] || len(r.Pattern) < 1 || len(r.Pattern) > maxPatternBytes || total > maxTotalPatternBytes || (r.Replacement != nil && (!utf8.ValidString(*r.Replacement) || utf8.RuneCountInString(*r.Replacement) > maxReplacementChars)) {
			return nil, errors.New("invalid bounded entity pattern")
		}
		seen[r.ID] = true
		re, err := regexp.Compile(r.Pattern)
		if err != nil || re.MatchString("") {
			return nil, errors.New("invalid bounded entity pattern")
		}
		value := replacement
		if value == "" {
			value = defaultReplacement
		}
		if r.Replacement != nil {
			value = *r.Replacement
		}
		p.patterns = append(p.patterns, entityPattern{re: re, category: category, replacement: value})
	}
	p.maxWidth = maximumPatternWidth(p.patterns)
	return p, nil
}
func maximumPatternWidth(patterns []entityPattern) int {
	width := 1
	for _, p := range patterns {
		re, err := syntax.Parse(p.re.String(), syntax.Perl)
		if err != nil {
			return 0
		}
		w := regexpWidth(re)
		if w == 0 {
			return 0
		}
		width = max(width, w)
	}
	return width
}
func regexpWidth(r *syntax.Regexp) int {
	switch r.Op {
	case syntax.OpLiteral:
		width := 0
		for _, c := range r.Rune {
			w := utf8.RuneLen(c)
			if r.Flags&syntax.FoldCase != 0 {
				for folded := unicode.SimpleFold(c); folded != c; folded = unicode.SimpleFold(folded) {
					w = max(w, utf8.RuneLen(folded))
				}
			}
			width += w
		}
		return max(1, width)
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return utf8.UTFMax
	case syntax.OpCapture:
		return regexpWidth(r.Sub[0])
	case syntax.OpQuest:
		return regexpWidth(r.Sub[0])
	case syntax.OpRepeat:
		w := regexpWidth(r.Sub[0])
		if r.Max < 0 || w == 0 || w*r.Max > MaxWindowBytes {
			return 0
		}
		return max(1, w*r.Max)
	case syntax.OpConcat:
		w := 0
		for _, sub := range r.Sub {
			n := regexpWidth(sub)
			if n == 0 {
				return 0
			}
			w += n
		}
		return max(1, w)
	case syntax.OpAlternate:
		w := 0
		for _, sub := range r.Sub {
			n := regexpWidth(sub)
			if n == 0 {
				return 0
			}
			w = max(w, n)
		}
		return max(1, w)
	case syntax.OpEmptyMatch:
		return 1
	default:
		return 0 // Repetition, anchors and word boundaries require whole input.
	}
}
func (p *patternDetector) detect(ctx context.Context, e Evaluation) (Verdict, error) {
	var categories []string
	var redactions []Redaction
	for _, s := range e.Segments {
		hasDigits := strings.ContainsAny(s.Text, "0123456789")
		for _, r := range p.patterns {
			if strings.HasPrefix(r.category, "pii.") && r.category != "pii.email" && r.category != "pii.custom" && !hasDigits && !strings.Contains(s.Text, ":") {
				continue
			}
			if r.category == "pii.email" && !strings.Contains(s.Text, "@") {
				continue
			}
			if err := ctx.Err(); err != nil {
				return Verdict{}, err
			}
			matches := r.re.FindAllStringIndex(s.Text, MaxFindings+1)
			if len(matches) > MaxFindings {
				return Verdict{}, errEvaluation
			}
			for _, span := range matches {
				value := s.Text[span[0]:span[1]]
				if r.check != nil && !r.check(value) {
					accepted := false
					if r.category == "pii.iban" || r.category == "pii.payment_card" {
						for end := len(value) - 1; end > 0; end-- {
							if r.check(strings.TrimRight(value[:end], " ")) {
								span[1] = span[0] + len(strings.TrimRight(value[:end], " "))
								accepted = true
								break
							}
						}
					}
					if !accepted {
						continue
					}
				}
				categories = appendCategory(categories, r.category)
				redactions = append(redactions, Redaction{s.ID, span[0], span[1], r.replacement})
				if len(redactions) > MaxFindings {
					return Verdict{}, errEvaluation
				}
			}
		}
	}
	// Detectors can overlap (a national phone number may also be a card). Merge
	// overlapping spans before applying replacements and retain every category.
	return Redacting(mergeRedactions(redactions), categories...), nil
}
func appendCategory(categories []string, c string) []string {
	if !slices.Contains(categories, c) {
		categories = append(categories, c)
	}
	return categories
}
func mergeRedactions(values []Redaction) []Redaction {
	slices.SortStableFunc(values, func(a, b Redaction) int {
		if a.Segment != b.Segment {
			return strings.Compare(a.Segment, b.Segment)
		}
		return a.Start - b.Start
	})
	out := values[:0]
	for _, r := range values {
		if len(out) > 0 {
			last := &out[len(out)-1]
			if r.Segment == last.Segment && r.Start < last.End {
				last.End = max(last.End, r.End)
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

type noSchemaNetwork struct{}

func (noSchemaNetwork) Load(string) (any, error) {
	return nil, errors.New("remote schema references are disabled")
}
func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return nil, errors.New("invalid bounded JSON schema")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("invalid JSON schema")
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noSchemaNetwork{})
	if c.AddResource("https://olp.invalid/guardrail-schema", doc) != nil {
		return nil, errors.New("invalid JSON schema")
	}
	s, err := c.Compile("https://olp.invalid/guardrail-schema")
	if err != nil {
		return nil, errors.New("invalid or remote JSON schema")
	}
	return s, nil
}

type schemaDetector struct{ schema *jsonschema.Schema }

func (*schemaDetector) width() int { return 0 }
func (d *schemaDetector) detect(ctx context.Context, e Evaluation) (Verdict, error) {
	segments, projectionErr := jsonToolSegments(e.Segments)
	if projectionErr != nil {
		return Verdict{}, projectionErr
	}
	if len(segments) == 0 {
		return Flag("schema.invalid"), nil
	}
	for _, s := range segments {
		if err := ctx.Err(); err != nil {
			return Verdict{}, err
		}
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(s.Text))
		if err != nil || d.schema.Validate(v) != nil {
			return Flag("schema.invalid"), nil
		}
	}
	return Verdict{}, nil
}

type toolsDetector struct {
	config  ToolsConfig
	schemas map[string]*jsonschema.Schema
}

func (*toolsDetector) width() int { return 0 }
func globsMatch(patterns []string, value string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, value); ok {
			return true
		}
	}
	return false
}
func (d *toolsDetector) detect(ctx context.Context, e Evaluation) (Verdict, error) {
	segments, projectionErr := jsonToolSegments(e.Segments)
	if projectionErr != nil {
		return Verdict{}, projectionErr
	}
	untrusted := false
	for _, s := range segments {
		if s.Kind == ToolResult && (s.Untrusted || globsMatch(d.config.Untrusted, s.ToolName)) {
			untrusted = true
		}
	}
	for _, s := range segments {
		if err := ctx.Err(); err != nil {
			return Verdict{}, err
		}
		if s.Kind != ToolCall && s.Kind != ToolDefinition {
			continue
		}
		category := ""
		switch {
		case globsMatch(d.config.Deny, s.ToolName):
			category = "tool.denied"
		case len(d.config.Allow) > 0 && !globsMatch(d.config.Allow, s.ToolName):
			category = "tool.not_allowed"
		case untrusted && globsMatch(d.config.Privileged, s.ToolName):
			category = "tool.untrusted_to_privileged"
		}
		if category != "" {
			return Flag(category), nil
		}
		if schema := d.schemas[s.ToolName]; schema != nil && s.Kind == ToolCall {
			v, err := jsonschema.UnmarshalJSON(strings.NewReader(s.Text))
			if err != nil || schema.Validate(v) != nil {
				return Flag("tool.schema_invalid"), nil
			}
		}
	}
	return Verdict{}, nil
}
