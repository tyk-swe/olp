package guardrails

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/crc32"
	"net/netip"
	"path"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/tyk-swe/olp/internal/contentpolicy"
)

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

func compileDetector(d Revision, external Evaluator) (detector, error) {
	switch d.Type {
	case "builtin.code_execution":
		return compileCodeExecution(d.Configuration)
	case "builtin.tool_permission":
		return compileToolPermission(d.Configuration)
	case "builtin.regex":
		var config RegexConfig
		if err := decodeConfig(d.Configuration, &config); err != nil {
			return nil, err
		}
		if len(config.Rules) > contentpolicy.MaxRules {
			return nil, errors.New("too many regex rules")
		}
		return compilePatterns(config.Rules, "regex", "")
	case "builtin.pii", "builtin.secrets":
		var config EntityConfig
		if err := decodeConfig(d.Configuration, &config); err != nil {
			return nil, err
		}
		available := piiPatterns
		if d.Type == "builtin.secrets" {
			available = secretPatterns
		}
		if len(config.Categories) == 0 {
			for name := range available {
				config.Categories = append(config.Categories, name)
			}
			slices.Sort(config.Categories)
		}
		if len(config.Categories) > 32 || len(config.Replacement) > 512 || !utf8.ValidString(config.Replacement) {
			return nil, errors.New("invalid entity configuration")
		}
		p := &patternDetector{}
		for _, name := range config.Categories {
			t, ok := available[name]
			if !ok {
				return nil, errors.New("unknown detector category")
			}
			r := entityPattern{re: regexp.MustCompile(t.pattern), category: name, replacement: config.Replacement, check: t.check}
			if r.replacement == "" {
				r.replacement = "[REDACTED]"
			}
			p.patterns = append(p.patterns, r)
		}
		custom, err := compilePatterns(config.Patterns, "pii.custom", config.Replacement)
		if err != nil {
			return nil, err
		}
		p.patterns = append(p.patterns, custom.patterns...)
		p.maxWidth = maximumPatternWidth(p.patterns)
		return p, nil
	case "builtin.keywords":
		return compileKeywords(d.Configuration)
	case "builtin.schema":
		var config SchemaConfig
		if err := decodeConfig(d.Configuration, &config); err != nil {
			return nil, err
		}
		schema, err := compileSchema(config.Schema)
		return &schemaDetector{schema: schema}, err
	case "builtin.tools":
		var config ToolsConfig
		if err := decodeConfig(d.Configuration, &config); err != nil {
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
		t := &toolsDetector{config: config, schemas: map[string]*jsonschema.Schema{}}
		for name, raw := range config.Schemas {
			if !identifier.MatchString(name) {
				return nil, errors.New("invalid tool name")
			}
			schema, err := compileSchema(raw)
			if err != nil {
				return nil, err
			}
			t.schemas[name] = schema
		}
		return t, nil
	case "typesafe":
		return compileTypeSafe(d.Configuration)
	case "webhook", "bedrock", "azure.content_safety", "azure.prompt_shields", "model_armor", "lakera", "presidio", "model", "semantic_guard", "decision_model", "plugin":
		if err := ValidateExternal(d); err != nil {
			return nil, err
		}
		return externalDetector{definition: d, evaluator: external}, nil
	default:
		if _, ok := lookupNamedVendor(d.Type); ok {
			if err := ValidateExternal(d); err != nil {
				return nil, err
			}
			return externalDetector{definition: d, evaluator: external}, nil
		}
		return nil, errors.New("unknown guardrail type")
	}
}

// ValidateDefinition validates without resolving credentials or performing I/O.
func ValidateDefinition(d Revision) error {
	if d.TimeoutMS < 1 || d.TimeoutMS > 300000 || (d.OnError != FailClosed && d.OnError != FailOpen) {
		return errors.New("invalid guardrail timeout or failure policy")
	}
	_, err := compileDetector(d, nil)
	return err
}

func compilePatterns(rules []Pattern, category, replacement string) (*patternDetector, error) {
	p := &patternDetector{}
	total := 0
	seen := map[string]bool{}
	if len(rules) > contentpolicy.MaxRules {
		return nil, errors.New("too many entity patterns")
	}
	for _, r := range rules {
		total += len(r.Pattern)
		if !contentpolicy.RuleID.MatchString(r.ID) || seen[r.ID] || len(r.Pattern) < 1 || len(r.Pattern) > contentpolicy.MaxPatternBytes || total > contentpolicy.MaxTotalPatternBytes || (r.Replacement != nil && (!utf8.ValidString(*r.Replacement) || utf8.RuneCountInString(*r.Replacement) > contentpolicy.MaxReplacementChars)) {
			return nil, errors.New("invalid bounded entity pattern")
		}
		seen[r.ID] = true
		re, err := regexp.Compile(r.Pattern)
		if err != nil || re.MatchString("") {
			return nil, errors.New("invalid bounded entity pattern")
		}
		value := replacement
		if value == "" {
			value = contentpolicy.DefaultReplacement
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
	v := Verdict{Action: "pass"}
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
				return v, err
			}
			matches := r.re.FindAllStringIndex(s.Text, MaxFindings+1)
			if len(matches) > MaxFindings {
				return v, errEvaluation
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
				v.Action = Redact
				v.Categories = appendCategory(v.Categories, r.category)
				v.Redactions = append(v.Redactions, Redaction{s.ID, span[0], span[1], r.replacement})
				if len(v.Redactions) > MaxFindings {
					return v, errEvaluation
				}
			}
		}
	}
	// Detectors can overlap (a national phone number may also be a card). Merge
	// overlapping spans before applying replacements and retain every category.
	v.Redactions = mergeRedactions(v.Redactions)
	return v, nil
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

type entityTemplate struct {
	pattern string
	check   func(string) bool
}

var piiPatterns = map[string]entityTemplate{
	"pii.email":        {`[\p{L}\p{N}.!#$%&'*+/=?^_` + "`" + `{|}~-]{1,64}@[\p{L}\p{N}](?:[\p{L}\p{N}-]{0,61}[\p{L}\p{N}])?(?:\.[\p{L}\p{N}](?:[\p{L}\p{N}-]{0,61}[\p{L}\p{N}])?){1,10}`, nil},
	"pii.phone":        {`\+?[0-9(][0-9() .-]{5,28}[0-9]`, validPhone},
	"pii.payment_card": {`[0-9](?:[ -]?[0-9]){12,18}`, validLuhn},
	"pii.iban":         {`[A-Z]{2}[0-9]{2}(?: ?[A-Z0-9]){11,30}`, validIBAN},
	"pii.us_ssn":       {`[0-9]{3}[- ]?[0-9]{2}[- ]?[0-9]{4}`, validSSN},
	"pii.ip_address":   {`(?:[0-9]{1,3}\.){3}[0-9]{1,3}|[0-9A-Fa-f]{0,4}:[0-9A-Fa-f:]{1,40}`, func(v string) bool { _, err := netip.ParseAddr(v); return err == nil }},
}
var secretPatterns = map[string]entityTemplate{
	"secret.private_key":    {`-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----[\s\S]+?-----END (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----`, nil},
	"secret.jwt":            {`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`, validJWT},
	"secret.aws_access_key": {`(?:AKIA|ASIA|AIDA|AROA)[A-Z0-9]{16}`, nil},
	"secret.google_cloud":   {`AIza[0-9A-Za-z_-]{35}`, nil},
	"secret.azure":          {`(?i)(?:AccountKey=[A-Za-z0-9+/]{86}==|SharedAccessSignature[ =][^\r\n]+|[?&]sig=[A-Za-z0-9%+/=_-]{20,256})`, nil},
	"secret.github":         {`(?:ghs_[0-9]+_eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{82})`, validGitHub},
	"secret.gitlab":         {`glpat-[A-Za-z0-9_-]{20,300}(?:\.[0-9a-z]{2}\.[0-9a-z]{9})?`, validGitLab},
	"secret.slack":          {`(?:xox[baprs]-[A-Za-z0-9-]{10,200}|xapp-[A-Za-z0-9-]{10,200})`, nil},
	"secret.stripe":         {`(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,128}`, nil},
	"secret.openai":         {`sk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,200}`, func(v string) bool { return !strings.HasPrefix(v, "sk-ant-") }},
	"secret.anthropic":      {`sk-ant-(?:api[0-9]{2}-)?[A-Za-z0-9_-]{20,200}`, nil},
}

func digits(v string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, v)
}
func validPhone(v string) bool {
	d := digits(v)
	if strings.HasPrefix(v, "+") {
		return len(d) >= 8 && len(d) <= 15 && d[0] != '0'
	}
	return len(d) == 10 || (len(d) == 11 && d[0] == '1')
}
func validLuhn(v string) bool {
	v = digits(v)
	if len(v) < 13 || len(v) > 19 || strings.Trim(v, "0") == "" {
		return false
	}
	sum := 0
	for i := len(v) - 1; i >= 0; i-- {
		n := int(v[i] - '0')
		if (len(v)-1-i)%2 == 1 {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

// Country lengths: SWIFT IBAN Registry, release 103 (September 2026).
// https://www.swift.com/resource/iban-registry-pdf
var ibanLengths = map[string]int{"AL": 28, "AD": 24, "AT": 20, "AZ": 28, "BH": 22, "BE": 16, "BA": 20, "BR": 29, "BG": 22, "CR": 22, "HR": 21, "CY": 28, "CZ": 24, "DK": 18, "DO": 28, "EE": 20, "FO": 18, "FI": 18, "FR": 27, "GE": 22, "DE": 22, "GI": 23, "GR": 27, "GL": 18, "GT": 28, "HU": 28, "IS": 26, "IE": 22, "IL": 23, "IT": 27, "JO": 30, "KZ": 20, "XK": 20, "KW": 30, "LV": 21, "LB": 28, "LI": 21, "LT": 20, "LU": 20, "MT": 31, "MR": 27, "MU": 30, "MC": 27, "MD": 24, "ME": 22, "NL": 18, "MK": 19, "NO": 15, "PK": 24, "PS": 29, "PL": 28, "PT": 25, "QA": 29, "RO": 24, "SM": 27, "SA": 24, "RS": 22, "SK": 24, "SI": 19, "ES": 24, "SE": 24, "CH": 21, "TL": 23, "TN": 24, "TR": 26, "UA": 29, "AE": 23, "GB": 22, "VA": 22, "BY": 28, "SV": 28, "SC": 31, "IQ": 23, "LC": 32, "ST": 25, "BI": 27, "DJ": 27, "EG": 29, "FK": 18, "HN": 28, "LY": 25, "MN": 20, "NI": 28, "OM": 23, "RU": 33, "SD": 18, "SO": 23, "VG": 24, "YE": 30}

func validIBAN(v string) bool {
	v = strings.ReplaceAll(v, " ", "")
	if len(v) < 15 || ibanLengths[v[:2]] != len(v) {
		return false
	}
	v = v[4:] + v[:4]
	n := 0
	for _, c := range v {
		if c >= '0' && c <= '9' {
			n = (n*10 + int(c-'0')) % 97
		} else if c >= 'A' && c <= 'Z' {
			n = (n*100 + int(c-'A') + 10) % 97
		} else {
			return false
		}
	}
	return n == 1
}
func validSSN(v string) bool {
	v = digits(v)
	return len(v) == 9 && !strings.HasPrefix(v, "000") && !strings.HasPrefix(v, "666") && v[0] != '9' && v[3:5] != "00" && v[5:] != "0000"
}
func validJWT(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var h map[string]any
	if json.Unmarshal(header, &h) != nil || h["alg"] == nil {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	return err == nil && json.Valid(body)
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
		return Verdict{Action: Block, Categories: []string{"schema.invalid"}}, nil
	}
	for _, s := range segments {
		if err := ctx.Err(); err != nil {
			return Verdict{}, err
		}
		v, err := jsonschema.UnmarshalJSON(strings.NewReader(s.Text))
		if err != nil || d.schema.Validate(v) != nil {
			return Verdict{Action: Block, Categories: []string{"schema.invalid"}}, nil
		}
	}
	return Verdict{Action: "pass"}, nil
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
		if s.Kind == "tool_result" && (s.Untrusted || globsMatch(d.config.Untrusted, s.ToolName)) {
			untrusted = true
		}
	}
	for _, s := range segments {
		if err := ctx.Err(); err != nil {
			return Verdict{}, err
		}
		if s.Kind != "tool_call" && s.Kind != "tool_definition" {
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
			return Verdict{Action: Block, Categories: []string{category}}, nil
		}
		if schema := d.schemas[s.ToolName]; schema != nil && s.Kind == "tool_call" {
			v, err := jsonschema.UnmarshalJSON(strings.NewReader(s.Text))
			if err != nil || schema.Validate(v) != nil {
				return Verdict{Action: Block, Categories: []string{"tool.schema_invalid"}}, nil
			}
		}
	}
	return Verdict{Action: "pass"}, nil
}

// Word characters include letters and numbers in every script.
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func validGitHub(value string) bool {
	if strings.HasPrefix(value, "github_pat_") {
		return true
	}
	if strings.HasPrefix(value, "ghs_") && strings.Contains(value[4:], "_") {
		_, token, ok := strings.Cut(value[4:], "_")
		return ok && validJWT(token)
	}
	if len(value) != 40 {
		return false
	}
	want := uint64(crc32.ChecksumIEEE([]byte(value[4:34])))
	for _, alphabet := range []string{"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"} {
		var n uint64
		for _, r := range value[34:] {
			i := strings.IndexRune(alphabet, r)
			if i < 0 {
				return false
			}
			n = n*62 + uint64(i)
		}
		if n == want {
			return true
		}
	}
	return false
}
func validGitLab(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) == 1 {
		return len(value) >= 26 && len(value) <= 70
	}
	if len(parts) != 3 || len(parts[1]) != 2 || len(parts[2]) != 9 {
		return false
	}
	length, err := strconv.ParseUint(parts[2][:2], 36, 16)
	if err != nil || int(length) != len(parts[0])-len("glpat-") {
		return false
	}
	checksum, err := strconv.ParseUint(parts[2][2:], 36, 32)
	if err != nil {
		return false
	}
	return uint32(checksum) == crc32.ChecksumIEEE([]byte(value[:len(value)-7]))
}
