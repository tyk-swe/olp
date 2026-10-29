package scim

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/scim2/filter-parser/v2"
)

func TestFilterBindsEveryCallerValueAndBoundsComplexity(t *testing.T) {
	for _, raw := range []string{`userName eq "O'Reilly"`, `emails[type eq "work" and value co "x' OR true --"]`, `not (active eq false)`, `meta.created gt "2026-01-01T00:00:00Z"`} {
		compiler := SQLFilter{}
		sql, err := compiler.Compile(raw, "document")
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if strings.Contains(sql, "O'Reilly") || strings.Contains(sql, "OR true --") || len(compiler.Args) == 0 {
			t.Fatal("caller text reached SQL", sql)
		}
	}
	for _, raw := range []string{strings.Repeat("(", 17) + `active pr` + strings.Repeat(")", 17), strings.Repeat("x", 4097), `emails[` + strings.Repeat(`type eq "work" or `, 32) + `value pr]`, `active gt true`, `userName =~ "x"`} {
		if _, err := (&SQLFilter{}).Compile(raw, "document"); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestMultivalueProjectionAndPatch(t *testing.T) {
	current, _ := Decode(strings.NewReader(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"stable","emails":[{"value":"a@example.com","type":"home"},{"value":"b@example.com","type":"work","primary":true}]}`))
	include, err := ParseProjection("emails.value,emails.primary", "", UserSchema)
	if err != nil {
		t.Fatal(err)
	}
	selected := include.Apply(current)
	entries := selected["emails"].([]any)
	if len(entries) != 2 || entries[1].(map[string]any)["primary"] != true || entries[0].(map[string]any)["type"] != nil {
		t.Fatal(selected)
	}
	exclude, err := ParseProjection("", "emails.type", UserSchema)
	if err != nil {
		t.Fatal(err)
	}
	if excluded := exclude.Apply(current); excluded["emails"].([]any)[0].(map[string]any)["type"] != nil {
		t.Fatal(excluded)
	}
	body, _ := Decode(strings.NewReader(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"replace","path":"emails.type","value":"work"}]}`))
	patched, err := Patch(current, body, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range patched["emails"].([]any) {
		if entry.(map[string]any)["type"] != "work" {
			t.Fatal(patched)
		}
	}
	if current["emails"].([]any)[0].(map[string]any)["type"] != "home" {
		t.Fatal("mutated original")
	}
}

func TestNullAttributeMembershipRespectsResourceSchema(t *testing.T) {
	for _, tc := range []struct {
		name       string
		schema     string
		expression string
		scope      []string
		defined    bool
	}{
		{"common-user", UserSchema, "externalId eq null", nil, true},
		{"common-group", GroupSchema, "externalId eq null", nil, true},
		{"user-subattribute", UserSchema, "name.givenName eq null", nil, true},
		{"user-attribute-on-group", GroupSchema, "userName eq null", nil, false},
		{"group-attribute", GroupSchema, "members eq null", nil, true},
		{"group-attribute-on-user", UserSchema, "members eq null", nil, false},
		{"unknown-user-attribute", UserSchema, "undeclared eq null", nil, false},
		{"unknown-group-attribute", GroupSchema, "undeclared eq null", nil, false},
		{"qualified-user", UserSchema, UserSchema + ":externalId eq null", nil, true},
		{"wrong-core-schema", UserSchema, GroupSchema + ":externalId eq null", nil, false},
		{"user-extension", UserSchema, UserExtension + ":role eq null", nil, true},
		{"user-access-scope", UserSchema, UserExtension + ":accessScope eq null", nil, true},
		{"group-extension-role", GroupSchema, GroupExtension + ":role eq null", nil, true},
		{"group-access-scope", GroupSchema, GroupExtension + ":accessScope eq null", nil, true},
		{"group-projects", GroupSchema, GroupExtension + ":projects eq null", nil, true},
		{"group-extension", GroupSchema, GroupExtension + ":projects.value eq null", nil, true},
		{"group-project-role", GroupSchema, GroupExtension + ":projects.role eq null", nil, true},
		{"wrong-extension", UserSchema, GroupExtension + ":projects.value eq null", nil, false},
		{"value-path-child", UserSchema, "primary eq null", []string{"emails"}, true},
		{"unknown-value-path-child", UserSchema, "undeclared eq null", []string{"emails"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParseFilter(tc.expression)
			if err != nil {
				t.Fatal(err)
			}
			comparison, ok := parsed.(*filter.AttributeExpression)
			if !ok {
				t.Fatalf("expected attribute comparison, got %T", parsed)
			}
			compiler := &SQLFilter{Schema: tc.schema}
			if got := compiler.definedAttribute(comparison.AttributePath, tc.scope); got != tc.defined {
				t.Fatalf("%s defined=%v, want %v", tc.expression, got, tc.defined)
			}
		})
	}
}

func TestSchemaFiltersPreserveValuesAndRejectInvalidNullComparisons(t *testing.T) {
	for _, schema := range []string{"", UserSchema} {
		for _, value := range []string{"O'Reilly", "x' OR true --"} {
			compiler := SQLFilter{Schema: schema}
			if _, err := compiler.Compile(`externalId eq "`+value+`"`, "document"); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(compiler.Args, any(value)) {
				t.Fatalf("filter lost comparison value %q", value)
			}
		}
		for _, operator := range []string{"gt", "ge", "lt", "le", "co", "sw", "ew"} {
			if _, err := (&SQLFilter{Schema: schema}).Compile("externalId "+operator+" null", "document"); err == nil {
				t.Fatalf("accepted invalid null comparison %s", operator)
			}
		}
	}
}

func TestValuePathCoreSchemaQualification(t *testing.T) {
	for _, tc := range []struct {
		schema string
		path   string
		wrong  string
	}{
		{UserSchema, "emails", GroupSchema},
		{GroupSchema, "members", UserSchema},
	} {
		for _, predicate := range []string{`type eq null`, `type ne null`, `type eq "work"`, `type pr`} {
			for _, prefix := range []string{"", tc.schema + ":", strings.ToUpper(tc.schema) + ":"} {
				if _, err := (&SQLFilter{Schema: tc.schema}).Compile(prefix+tc.path+"["+predicate+"]", "document"); err != nil {
					t.Fatalf("valid value path rejected: %v", err)
				}
			}
			_, err := (&SQLFilter{Schema: tc.schema}).Compile(tc.wrong+":"+tc.path+"["+predicate+"]", "document")
			var failure *Error
			if !errors.As(err, &failure) || failure.Status != 400 || failure.Type != "invalidFilter" {
				t.Fatalf("wrong core schema for %s: got %v, want invalidFilter", tc.schema, err)
			}
		}
	}
}

func TestAttributeCoreSchemaQualification(t *testing.T) {
	for _, tc := range []struct {
		schema string
		wrong  string
	}{
		{UserSchema, GroupSchema},
		{GroupSchema, UserSchema},
	} {
		for _, predicate := range []string{`eq "work"`, `ne "work"`, `co "work"`, `pr`} {
			for _, prefix := range []string{"", tc.schema + ":", strings.ToUpper(tc.schema) + ":", UserExtension + ":"} {
				if _, err := (&SQLFilter{Schema: tc.schema}).Compile(prefix+"displayName "+predicate, "document"); err != nil {
					t.Fatalf("valid attribute rejected: %v", err)
				}
			}
			for _, expression := range []string{tc.wrong + ":displayName " + predicate, `not (id pr and ` + tc.wrong + ":displayName " + predicate + `)`} {
				_, err := (&SQLFilter{Schema: tc.schema}).Compile(expression, "document")
				var failure *Error
				if !errors.As(err, &failure) || failure.Status != 400 || failure.Type != "invalidFilter" {
					t.Errorf("wrong core schema for %s: got %v, want invalidFilter", expression, err)
				}
			}
		}
		for _, operator := range []string{"eq", "ne"} {
			sql, err := (&SQLFilter{Schema: tc.schema}).Compile(tc.wrong+":externalId "+operator+" null", "document")
			if err != nil || sql != "FALSE" {
				t.Fatalf("wrong-core null membership changed: %s, %v", sql, err)
			}
		}
	}
}

func TestSortAndProjectionCoreSchemaQualification(t *testing.T) {
	for _, tc := range []struct {
		schema string
		wrong  string
	}{
		{UserSchema, GroupSchema},
		{GroupSchema, UserSchema},
	} {
		current := Document{"schemas": []any{tc.schema}, "id": "stable", "displayName": "work", "externalId": "external"}
		for _, prefix := range []string{"", tc.schema + ":", strings.ToUpper(tc.schema) + ":", tc.wrong + ":"} {
			t.Run(tc.schema+prefix, func(t *testing.T) {
				path := prefix + "displayName"
				_, sortErr := (&SQLFilter{Schema: tc.schema}).Sort(path, "document")
				include, includeErr := ParseProjection(path, "", tc.schema)
				exclude, excludeErr := ParseProjection("", path, tc.schema)
				if prefix == tc.wrong+":" {
					for _, err := range []error{sortErr, includeErr, excludeErr} {
						var failure *Error
						if !errors.As(err, &failure) || failure.Status != 400 || failure.Type != "invalidPath" {
							t.Errorf("wrong core path accepted: %v", err)
						}
					}
					return
				}
				if sortErr != nil || includeErr != nil || excludeErr != nil {
					t.Fatalf("valid core path rejected: %v, %v, %v", sortErr, includeErr, excludeErr)
				}
				included, excluded := include.Apply(current), exclude.Apply(current)
				if included["displayName"] != "work" || included["externalId"] != nil || included["id"] != "stable" || excluded["displayName"] != nil || excluded["externalId"] != "external" {
					t.Fatalf("incorrect selection: included=%v excluded=%v", included, excluded)
				}
			})
		}
	}
}

func TestPatchCoreSchemaQualification(t *testing.T) {
	for _, tc := range []struct {
		schema string
		wrong  string
		array  string
		group  bool
	}{
		{UserSchema, GroupSchema, "emails", false},
		{GroupSchema, UserSchema, "members", true},
	} {
		current := Document{"schemas": []any{tc.schema}, "displayName": "before", tc.array: []any{map[string]any{"value": "before", "type": "work"}}}
		original := Clone(current)
		for _, kind := range []string{"add", "replace", "remove"} {
			for _, prefix := range []string{"", tc.schema + ":", strings.ToUpper(tc.schema) + ":", tc.wrong + ":"} {
				for _, path := range []string{prefix + "displayName", prefix + tc.array + `[type eq "work"]`} {
					value := func() any {
						if strings.Contains(path, "[") {
							return map[string]any{"value": "after", "type": "work"}
						}
						return "after"
					}()
					body := Document{"schemas": []any{PatchSchema}, "operations": []any{map[string]any{"op": kind, "path": path, "value": value}}}
					patched, err := Patch(current, body, tc.group, func(_ []any, _ filter.Expression) ([]int, error) { return []int{0}, nil })
					if prefix == tc.wrong+":" {
						var failure *Error
						if !errors.As(err, &failure) || failure.Status != 400 || failure.Type != "invalidPath" {
							t.Errorf("%s %s: got %v, want invalidPath", kind, path, err)
						}
						continue
					}
					if err != nil {
						t.Fatalf("%s %s: %v", kind, path, err)
					}
					if strings.Contains(path, "[") {
						values := patched[tc.array].([]any)
						if kind == "remove" && len(values) != 0 || kind != "remove" && (len(values) != 1 || values[0].(map[string]any)["value"] != "after") {
							t.Fatalf("filtered %s did not change the selected value: %v", kind, patched)
						}
					} else if kind == "remove" && patched["displayName"] != nil || kind != "remove" && patched["displayName"] != "after" {
						t.Fatalf("plain %s did not change displayName: %v", kind, patched)
					}
				}
			}
		}
		for _, prefix := range []string{"", tc.schema + ":", tc.wrong + ":"} {
			for _, predicate := range []string{prefix + `type eq "work"`, `not(type pr and ` + prefix + `value pr)`, prefix + `type pr or value pr`, prefix + "type eq null"} {
				body := Document{"schemas": []any{PatchSchema}, "operations": []any{map[string]any{"op": "remove", "path": tc.array + "[" + predicate + "]"}}}
				patched, err := Patch(current, body, tc.group, func(_ []any, _ filter.Expression) ([]int, error) { return []int{0}, nil })
				if prefix == tc.wrong+":" {
					var failure *Error
					if !errors.As(err, &failure) || failure.Status != 400 || failure.Type != "invalidFilter" {
						t.Errorf("wrong inner schema %s: got %v, want invalidFilter", predicate, err)
					}
					continue
				}
				if err != nil || len(patched[tc.array].([]any)) != 0 {
					t.Fatalf("valid inner schema rejected for %s: %v, %v", predicate, patched, err)
				}
			}
		}
		if !reflect.DeepEqual(current, original) {
			t.Fatalf("PATCH mutated its input: %v", current)
		}
	}
}

func TestExtensionPathsRemainQualified(t *testing.T) {
	for _, tc := range []struct {
		schema    string
		extension string
		group     bool
	}{
		{UserSchema, UserExtension, false},
		{GroupSchema, GroupExtension, true},
	} {
		current := Document{"schemas": []any{tc.schema, tc.extension}, "displayName": "name", tc.extension: map[string]any{"role": "viewer", "accessScope": "assigned"}}
		path := tc.extension + ":role"
		if _, err := (&SQLFilter{Schema: tc.schema}).Compile(path+` eq "viewer"`, "document"); err != nil {
			t.Fatal(err)
		}
		if _, err := (&SQLFilter{Schema: tc.schema}).Sort(path, "document"); err != nil {
			t.Fatal(err)
		}
		include, err := ParseProjection(path, "", tc.schema)
		if err != nil {
			t.Fatal(err)
		}
		included := include.Apply(current)
		if included["displayName"] != nil || !reflect.DeepEqual(included[tc.extension], map[string]any{"role": "viewer"}) {
			t.Fatal(included)
		}
		exclude, err := ParseProjection("", path, tc.schema)
		if err != nil {
			t.Fatal(err)
		}
		excluded := exclude.Apply(current)
		if excluded["displayName"] != "name" || !reflect.DeepEqual(excluded[tc.extension], map[string]any{"accessScope": "assigned"}) {
			t.Fatal(excluded)
		}
		body := Document{"schemas": []any{PatchSchema}, "operations": []any{map[string]any{"op": "replace", "path": path, "value": "developer"}}}
		patched, err := Patch(current, body, tc.group, nil)
		if err != nil || patched[tc.extension].(map[string]any)["role"] != "developer" || patched["role"] != nil || current[tc.extension].(map[string]any)["role"] != "viewer" {
			t.Fatalf("extension PATCH changed the wrong field: %v, %v", patched, err)
		}
	}
}

func TestProjectionValidatesBeforeApplyingToResources(t *testing.T) {
	for _, tc := range []struct {
		attributes string
		excluded   string
		problem    string
	}{
		{"displayName", "externalId", "invalidValue"},
		{strings.Repeat("a", 4097), "", "invalidValue"},
		{"emails[", "", "invalidPath"},
		{"displayName,", "", "invalidPath"},
		{GroupSchema + ":displayName", "", "invalidPath"},
		{"", GroupSchema + ":displayName", "invalidPath"},
	} {
		_, err := ParseProjection(tc.attributes, tc.excluded, UserSchema)
		var failure *Error
		if !errors.As(err, &failure) || failure.Status != 400 || failure.Type != tc.problem {
			t.Fatalf("selection %q/%q: got %v, want %s", tc.attributes, tc.excluded, err, tc.problem)
		}
	}
	projection, err := ParseProjection("", "", UserSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		current := Document{"displayName": name, "emails": []any{map[string]any{"value": name + "@example.com"}}}
		if got := projection.Apply(current); !reflect.DeepEqual(got, current) {
			t.Fatalf("empty selection changed the resource: %v", got)
		}
	}
}
