package scim

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	filter "github.com/scim2/filter-parser/v2"
)

// ParseFilter bounds input before invoking the RFC grammar parser.
func ParseFilter(raw string) (filter.Expression, error) {
	if !boundedExpression(raw) {
		return nil, Fail(400, "invalidFilter", "The filter is too large or deeply nested.")
	}
	e, err := filter.ParseFilterNumber([]byte(raw))
	if err != nil {
		return nil, Fail(400, "invalidFilter", "The SCIM filter is invalid.")
	}
	return e, nil
}

func boundedExpression(s string) bool {
	if len(s) > 4096 {
		return false
	}
	depth := 0
	quoted, escape := false, false
	for _, r := range s {
		if escape {
			escape = false
			continue
		}
		if quoted && r == '\\' {
			escape = true
			continue
		}
		if r == '"' {
			quoted = !quoted
		}
		if quoted {
			continue
		}
		if r == '(' || r == '[' {
			depth++
			if depth > 16 {
				return false
			}
		}
		if r == ')' || r == ']' {
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !quoted
}

func pathParts(p filter.AttributePath) []string {
	parts := []string{}
	if p.URIPrefix != nil && !strings.EqualFold(*p.URIPrefix, UserSchema) && !strings.EqualFold(*p.URIPrefix, GroupSchema) {
		parts = append(parts, Canonical(*p.URIPrefix))
	}
	parts = append(parts, Canonical(p.AttributeName))
	if p.SubAttribute != nil {
		parts = append(parts, Canonical(*p.SubAttribute))
	}
	return parts
}

func jsonPath(p filter.AttributePath) string {
	path := "$"
	for _, part := range pathParts(p) {
		b, _ := json.Marshal(part)
		path += "." + string(b)
		switch part {
		case "emails", "roles", "groups", "members", "projects":
			path += "[*]"
		}
	}
	return path
}

type SQLFilter struct {
	Args     []any
	sequence int
}

func (s *SQLFilter) bind(v any) string {
	s.Args = append(s.Args, v)
	return fmt.Sprintf("$%d", len(s.Args))
}

// Compile returns SQL with exclusively bound values and paths. base is a trusted
// column expression supplied by the server, never a caller-selected SQL name.
func (s *SQLFilter) Compile(raw, base string) (string, error) {
	if raw == "" {
		return "TRUE", nil
	}
	e, err := ParseFilter(raw)
	if err != nil {
		return "", err
	}
	return s.expression(e, base, 0)
}

func (s *SQLFilter) expression(e filter.Expression, base string, depth int) (string, error) {
	if depth > 32 {
		return "", Fail(400, "invalidFilter", "The filter is too complex.")
	}
	switch x := e.(type) {
	case *filter.LogicalExpression:
		a, err := s.expression(x.Left, base, depth+1)
		if err != nil {
			return "", err
		}
		b, err := s.expression(x.Right, base, depth+1)
		if err != nil {
			return "", err
		}
		op := "AND"
		if x.Operator == filter.OR {
			op = "OR"
		}
		return "(" + a + " " + op + " " + b + ")", nil
	case *filter.NotExpression:
		a, err := s.expression(x.Expression, base, depth+1)
		return "(NOT (" + a + "))", err
	case *filter.ValuePath:
		s.sequence++
		alias := fmt.Sprintf("scim_value_%d", s.sequence)
		source := "jsonb_path_query(" + base + "," + s.bind(jsonPath(x.AttributePath)) + "::jsonpath) " + alias + "(value)"
		condition, err := s.expression(x.ValueFilter, alias+".value", depth+1)
		return "EXISTS(SELECT 1 FROM " + source + " WHERE " + condition + ")", err
	case *filter.AttributeExpression:
		s.sequence++
		alias := fmt.Sprintf("scim_value_%d", s.sequence)
		v := alias + ".value"
		source := "jsonb_path_query(" + base + "," + s.bind(jsonPath(x.AttributePath)) + "::jsonpath) " + alias + "(value)"
		condition, err := s.compare(x, v)
		if err != nil {
			return "", err
		}
		return "EXISTS(SELECT 1 FROM " + source + " WHERE " + condition + ")", nil
	}
	return "", Fail(400, "invalidFilter", "Unsupported filter expression.")
}

func (s *SQLFilter) compare(e *filter.AttributeExpression, v string) (string, error) {
	if e.Operator == filter.PR {
		return v + " NOT IN ('null'::jsonb,'\"\"'::jsonb,'[]'::jsonb,'{}'::jsonb)", nil
	}
	op := map[filter.CompareOperator]string{filter.EQ: "=", filter.NE: "<>", filter.GT: ">", filter.GE: ">=", filter.LT: "<", filter.LE: "<="}[e.Operator]
	switch value := e.CompareValue.(type) {
	case string:
		if strings.ContainsRune(value, 0) {
			return "", Fail(400, "invalidFilter", "Filter strings cannot contain a null character.")
		}
		text := "(" + v + "#>>'{}')"
		actual := text
		expected := ""
		parts := pathParts(e.AttributePath)
		last := parts[len(parts)-1]
		if last == "created" || last == "lastModified" {
			expected = s.bind(value)
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil || op == "" {
				return "", Fail(400, "invalidFilter", "Date comparisons require an RFC 3339 timestamp.")
			}
			return "CASE WHEN jsonb_typeof(" + v + ")='string' THEN " + actual + "::timestamptz " + op + " " + expected + "::timestamptz ELSE FALSE END", nil
		}
		if op != "" {
			expected = s.bind(value)
		}
		if last != "id" && last != "externalId" && last != "$ref" {
			actual = "lower(" + actual + ")"
			expected = "lower(" + expected + "::text)"
		}
		if op != "" {
			return "jsonb_typeof(" + v + ")='string' AND " + actual + op + expected, nil
		}
		pattern := regexp.QuoteMeta(value)
		switch e.Operator {
		case filter.SW:
			pattern = "^" + pattern
		case filter.EW:
			pattern += "$"
		case filter.CO:
		default:
			return "", Fail(400, "invalidFilter", "Unsupported comparison.")
		}
		operator := " ~* "
		if last == "id" || last == "externalId" || last == "$ref" {
			operator = " ~ "
		}
		return "jsonb_typeof(" + v + ")='string' AND " + text + operator + s.bind(pattern), nil
	case bool:
		if op != "=" && op != "<>" {
			return "", Fail(400, "invalidFilter", "Booleans support eq and ne.")
		}
		return v + op + s.bind(fmt.Sprint(value)) + "::jsonb", nil
	case json.Number:
		if op == "" {
			return "", Fail(400, "invalidFilter", "Numbers require an ordering or equality comparison.")
		}
		return "CASE WHEN jsonb_typeof(" + v + ")='number' THEN (" + v + "#>>'{}')::numeric " + op + " " + s.bind(value.String()) + "::numeric ELSE FALSE END", nil
	case nil:
		if op != "=" && op != "<>" {
			return "", Fail(400, "invalidFilter", "Null supports eq and ne.")
		}
		return v + op + "'null'::jsonb", nil
	}
	return "", Fail(400, "invalidFilter", "Unsupported comparison value.")
}

// Sort honors primary values for multi-valued complex attributes, then uses the
// first value. The caller adds the resource ID as a stable pagination tie-break.
func (s *SQLFilter) Sort(raw, base string) (string, error) {
	parts, err := SortPath(raw)
	if err != nil {
		return "", err
	}
	last := parts[len(parts)-1]
	switch last {
	case "name", "meta", "emails", "members", "roles", "groups", "projects":
		return "", Fail(400, "invalidPath", "Choose a scalar subattribute for sorting.")
	}
	path, primary := "$", "$"
	multi := false
	for _, part := range parts {
		encoded, _ := json.Marshal(part)
		step := "." + string(encoded)
		path += step
		primary += step
		switch part {
		case "emails", "roles", "groups", "members", "projects":
			path += "[*]"
			primary += "[*] ? (@.primary == true)"
			multi = true
		}
	}
	first := "jsonb_path_query_first(" + base + "," + s.bind(path) + "::jsonpath)"
	if multi {
		first = "COALESCE(jsonb_path_query_first(" + base + "," + s.bind(primary) + "::jsonpath)," + first + ")"
	}
	return "lower((" + first + ") #>> '{}')", nil
}
