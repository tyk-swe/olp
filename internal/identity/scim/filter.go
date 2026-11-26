package scim

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	filter "github.com/scim2/filter-parser/v2"
)

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
	parts := make([]string, 0, 3)
	if p.URIPrefix != nil && !strings.EqualFold(*p.URIPrefix, UserSchema) && !strings.EqualFold(*p.URIPrefix, GroupSchema) {
		parts = append(parts, Canonical(*p.URIPrefix))
	}
	parts = append(parts, Canonical(p.AttributeName))
	if p.SubAttribute != nil {
		parts = append(parts, Canonical(*p.SubAttribute))
	}
	return parts
}

func MultiValuedComplexAttribute(name string) bool {
	switch name {
	case "emails", "roles", "groups", "members", "projects":
		return true
	default:
		return false
	}
}

func jsonPath(p filter.AttributePath) (string, error) {
	path := "$"
	for _, part := range pathParts(p) {
		encoded, err := json.Marshal(part)
		if err != nil {
			return "", err
		}
		path += "." + string(encoded)
		if MultiValuedComplexAttribute(part) {
			path += "[*]"
		}
	}
	return path, nil
}

type SQLFilter struct {
	Schema   string
	Args     []any
	sequence int
}

func (s *SQLFilter) bind(v any) string {
	s.Args = append(s.Args, v)
	return fmt.Sprintf("$%d", len(s.Args))
}

func (s *SQLFilter) nextAlias() string {
	s.sequence++
	return fmt.Sprintf("scim_value_%d", s.sequence)
}

func (s *SQLFilter) Compile(raw, base string) (string, error) {
	if raw == "" {
		return "TRUE", nil
	}
	e, err := ParseFilter(raw)
	if err != nil {
		return "", err
	}
	return s.expression(e, base, 0, nil)
}

func (s *SQLFilter) expression(e filter.Expression, base string, depth int, scope []string) (string, error) {
	if depth > 32 {
		return "", Fail(400, "invalidFilter", "The filter is too complex.")
	}
	switch x := e.(type) {
	case *filter.LogicalExpression:
		return s.logicalExpression(x, base, depth, scope)
	case *filter.NotExpression:
		inner, err := s.expression(x.Expression, base, depth+1, scope)
		return "(NOT (" + inner + "))", err
	case *filter.ValuePath:
		return s.valuePathExpression(x, base, depth, scope)
	case *filter.AttributeExpression:
		return s.attributeExpression(x, base, scope)
	default:
		return "", Fail(400, "invalidFilter", "Unsupported filter expression.")
	}
}

func (s *SQLFilter) logicalExpression(x *filter.LogicalExpression, base string, depth int, scope []string) (string, error) {
	left, err := s.expression(x.Left, base, depth+1, scope)
	if err != nil {
		return "", err
	}
	right, err := s.expression(x.Right, base, depth+1, scope)
	if err != nil {
		return "", err
	}
	op := "AND"
	if x.Operator == filter.OR {
		op = "OR"
	}
	return "(" + left + " " + op + " " + right + ")", nil
}

func (s *SQLFilter) valuePathExpression(x *filter.ValuePath, base string, depth int, scope []string) (string, error) {
	if s.Schema != "" && !matchesCoreSchema(x.AttributePath, s.Schema) {
		return "", Fail(400, "invalidFilter", "The value path belongs to a different resource schema.")
	}
	jp, err := jsonPath(x.AttributePath)
	if err != nil {
		return "", err
	}
	alias := s.nextAlias()
	source := "jsonb_path_query(" + base + "," + s.bind(jp) + "::jsonpath) " + alias + "(value)"
	condition, err := s.expression(x.ValueFilter, alias+".value", depth+1, slices.Concat(scope, pathParts(x.AttributePath)))
	if err != nil {
		return "", err
	}
	return "EXISTS(SELECT 1 FROM " + source + " WHERE " + condition + ")", nil
}

func (s *SQLFilter) attributeExpression(x *filter.AttributeExpression, base string, scope []string) (string, error) {
	nullComparison := s.Schema != "" && x.CompareValue == nil && (x.Operator == filter.EQ || x.Operator == filter.NE)
	if nullComparison && !s.definedAttribute(x.AttributePath, scope) {
		return "FALSE", nil
	}
	if s.Schema != "" && !matchesCoreSchema(x.AttributePath, s.Schema) {
		return "", Fail(400, "invalidFilter", "The attribute belongs to a different resource schema.")
	}
	jp, err := jsonPath(x.AttributePath)
	if err != nil {
		return "", err
	}
	alias := s.nextAlias()
	v := alias + ".value"
	source := "jsonb_path_query(" + base + "," + s.bind(jp) + "::jsonpath) " + alias + "(value)"
	if nullComparison {
		assigned := "EXISTS(SELECT 1 FROM " + source + " WHERE " + v + " NOT IN ('null'::jsonb,'\"\"'::jsonb,'[]'::jsonb,'{}'::jsonb))"
		if x.Operator == filter.EQ {
			return "NOT " + assigned, nil
		}
		return assigned, nil
	}
	condition, err := s.compare(x, v)
	if err != nil {
		return "", err
	}
	return "EXISTS(SELECT 1 FROM " + source + " WHERE " + condition + ")", nil
}

func matchesCoreSchema(path filter.AttributePath, schema string) bool {
	return path.URIPrefix == nil ||
		(!strings.EqualFold(*path.URIPrefix, UserSchema) && !strings.EqualFold(*path.URIPrefix, GroupSchema)) ||
		strings.EqualFold(*path.URIPrefix, schema)
}

func (s *SQLFilter) definedAttribute(path filter.AttributePath, scope []string) bool {
	if !matchesCoreSchema(path, s.Schema) {
		return false
	}
	parts := slices.Concat(scope, pathParts(path))
	name := strings.Join(parts, ".")
	switch name {
	case "schemas", "id", "externalId", "meta", "meta.resourceType", "meta.created", "meta.lastModified", "meta.location", "meta.version":
		return true
	}
	if s.Schema == UserSchema {
		if len(parts) == 2 && parts[0] == "name" && slices.Contains(NameSubattributes(), parts[1]) {
			return true
		}
		switch name {
		case "userName", "displayName", "active", "name",
			"emails", "emails.value", "emails.type", "emails.primary", "groups", "groups.value", "groups.display", "groups.type", "groups.$ref", "roles", "roles.value", "roles.primary",
			UserExtension + ".role", UserExtension + ".accessScope":
			return true
		}
	}
	if s.Schema == GroupSchema {
		switch name {
		case "displayName", "members", "members.value", "members.display", "members.type", "members.$ref",
			GroupExtension + ".role", GroupExtension + ".accessScope", GroupExtension + ".projects", GroupExtension + ".projects.value", GroupExtension + ".projects.role":
			return true
		}
	}
	return false
}

func compareOperatorSQL(op filter.CompareOperator) string {
	switch op {
	case filter.EQ:
		return "="
	case filter.NE:
		return "<>"
	case filter.GT:
		return ">"
	case filter.GE:
		return ">="
	case filter.LT:
		return "<"
	case filter.LE:
		return "<="
	default:
		return ""
	}
}

func caseSensitiveAttribute(name string) bool {
	return name == "id" || name == "externalId" || name == "$ref"
}

func (s *SQLFilter) compare(e *filter.AttributeExpression, v string) (string, error) {
	if e.Operator == filter.PR {
		return v + " NOT IN ('null'::jsonb,'\"\"'::jsonb,'[]'::jsonb,'{}'::jsonb)", nil
	}
	op := compareOperatorSQL(e.Operator)
	switch value := e.CompareValue.(type) {
	case string:
		return s.compareString(e, v, value, op)
	case bool:
		return s.compareBool(v, value, op)
	case json.Number:
		return s.compareNumber(v, value, op)
	case nil:
		return compareNull(v, op)
	default:
		return "", Fail(400, "invalidFilter", "Unsupported comparison value.")
	}
}

func (s *SQLFilter) compareString(e *filter.AttributeExpression, v, value, op string) (string, error) {
	if strings.ContainsRune(value, 0) {
		return "", Fail(400, "invalidFilter", "Filter strings cannot contain a null character.")
	}
	text := "(" + v + "#>>'{}')"
	parts := pathParts(e.AttributePath)
	last := parts[len(parts)-1]
	if last == "created" || last == "lastModified" {
		return s.compareTimestamp(v, text, value, op)
	}
	caseSensitive := caseSensitiveAttribute(last)
	if op != "" {
		expected := s.bind(value)
		actual := text
		if !caseSensitive {
			actual = "lower(" + actual + ")"
			expected = "lower(" + expected + "::text)"
		}
		return "jsonb_typeof(" + v + ")='string' AND " + actual + op + expected, nil
	}
	return s.compareStringPattern(e.Operator, v, text, value, caseSensitive)
}

func (s *SQLFilter) compareTimestamp(v, actual, value, op string) (string, error) {
	expected := s.bind(value)
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil || op == "" {
		return "", Fail(400, "invalidFilter", "Date comparisons require an RFC 3339 timestamp.")
	}
	return "CASE WHEN jsonb_typeof(" + v + ")='string' THEN " + actual + "::timestamptz " + op + " " + expected + "::timestamptz ELSE FALSE END", nil
}

func (s *SQLFilter) compareStringPattern(op filter.CompareOperator, v, text, value string, caseSensitive bool) (string, error) {
	pattern := regexp.QuoteMeta(value)
	switch op {
	case filter.SW:
		pattern = "^" + pattern
	case filter.EW:
		pattern += "$"
	case filter.CO:
	default:
		return "", Fail(400, "invalidFilter", "Unsupported comparison.")
	}
	operator := " ~* "
	if caseSensitive {
		operator = " ~ "
	}
	return "jsonb_typeof(" + v + ")='string' AND " + text + operator + s.bind(pattern), nil
}

func (s *SQLFilter) compareBool(v string, value bool, op string) (string, error) {
	if op != "=" && op != "<>" {
		return "", Fail(400, "invalidFilter", "Booleans support eq and ne.")
	}
	return v + op + s.bind(fmt.Sprint(value)) + "::jsonb", nil
}

func (s *SQLFilter) compareNumber(v string, value json.Number, op string) (string, error) {
	if op == "" {
		return "", Fail(400, "invalidFilter", "Numbers require an ordering or equality comparison.")
	}
	return "CASE WHEN jsonb_typeof(" + v + ")='number' THEN (" + v + "#>>'{}')::numeric " + op + " " + s.bind(value.String()) + "::numeric ELSE FALSE END", nil
}

func compareNull(v, op string) (string, error) {
	if op != "=" && op != "<>" {
		return "", Fail(400, "invalidFilter", "Null supports eq and ne.")
	}
	return v + op + "'null'::jsonb", nil
}

func (s *SQLFilter) Sort(raw, base string) (string, error) {
	parts, err := SortPath(raw, s.Schema)
	if err != nil {
		return "", err
	}
	last := parts[len(parts)-1]
	if last == "name" || last == "meta" || MultiValuedComplexAttribute(last) {
		return "", Fail(400, "invalidPath", "Choose a scalar subattribute for sorting.")
	}
	path, primary := "$", "$"
	multi := false
	for _, part := range parts {
		encoded, err := json.Marshal(part)
		if err != nil {
			return "", err
		}
		step := "." + string(encoded)
		path += step
		primary += step
		if MultiValuedComplexAttribute(part) {
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
