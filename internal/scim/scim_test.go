package scim

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	filter "github.com/scim2/filter-parser/v2"
)

const storedUser = `{
	"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"],
	"id": "user-1",
	"userName": "ada@example.com",
	"displayName": "Ada",
	"name": {"givenName": "Ada", "familyName": "Lovelace"},
	"emails": [{"value": "ada@example.com", "type": "work", "primary": true}],
	"meta": {"resourceType": "User"}
}`

func decode(t *testing.T, raw string) Document {
	t.Helper()
	document, err := Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return document
}

func canonical(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func problem(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Type
	}
	return fmt.Sprint(err)
}

func patch(operations string) string {
	return `{"schemas":["` + PatchSchema + `"],"Operations":[` + operations + `]}`
}

func unfiltered([]any, filter.Expression) ([]int, error) {
	return nil, errors.New("no operation selects values")
}

func TestDecodeRefusesNamesThatDifferOnlyByCase(t *testing.T) {
	document := decode(t, `{"USERNAME":"ada","Name":{"GIVENNAME":"Ada"}}`)
	if want := `{"name":{"givenName":"Ada"},"userName":"ada"}`; canonical(t, document) != want {
		t.Errorf("got %s, want %s", canonical(t, document), want)
	}
	for raw, want := range map[string]string{
		`{"userName":"ada","USERNAME":"eve"}`:                 "invalidSyntax",
		`{"name":{"givenName":"Ada","givenname":"Eve"}}`:      "invalidSyntax",
		`{"userName":"ada","userName":"eve"}`:                 "invalidSyntax",
		`["userName"]`:                                        "invalidSyntax",
		`{"userName":"` + strings.Repeat("a", MaxBody) + `"}`: "tooLarge",
	} {
		if _, err := Decode(strings.NewReader(raw)); problem(err) != want {
			t.Errorf("%.60s: got %s, want %s", raw, problem(err), want)
		}
	}
}

func TestPatchAppliesOperationsToACopy(t *testing.T) {
	current := decode(t, storedUser)
	body := decode(t, patch(`
		{"op":"Replace","path":"displayName","value":"Countess"},
		{"op":"add","path":"emails","value":[{"value":"ada@home.example","primary":true}]},
		{"op":"add","path":"emails","value":{"value":"ada@example.com","type":"archive"}},
		{"op":"remove","path":"name.familyName"},
		{"op":"add","value":{"EXTERNALID":"directory-7"}}`))
	patched, err := Patch(current, body, false, unfiltered)
	if err != nil {
		t.Fatal(err)
	}
	want := decode(t, `{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"],
		"id": "user-1",
		"externalId": "directory-7",
		"userName": "ada@example.com",
		"displayName": "Countess",
		"name": {"givenName": "Ada"},
		"emails": [
			{"value": "ada@example.com", "type": "archive", "primary": false},
			{"value": "ada@home.example", "primary": true}
		],
		"meta": {"resourceType": "User"}
	}`)
	if canonical(t, patched) != canonical(t, want) {
		t.Errorf("patched document\n got %s\nwant %s", canonical(t, patched), canonical(t, want))
	}
	if canonical(t, current) != canonical(t, decode(t, storedUser)) {
		t.Errorf("the stored document changed: %s", canonical(t, current))
	}
}

func TestPatchRefusesOperationsItCannotApply(t *testing.T) {
	for operation, want := range map[string]string{
		`{"op":"replace","path":"id","value":"other"}`:                "mutability",
		`{"op":"replace","path":"meta.created","value":"2020-01-01"}`: "mutability",
		`{"op":"replace","path":"password","value":"secret"}`:         "mutability",
		`{"op":"add","path":"groups","value":[{"value":"admins"}]}`:   "mutability",
		`{"op":"replace","value":{"ID":"other"}}`:                     "mutability",
		`{"op":"remove"}`:                                                                                        "noTarget",
		`{"op":"remove","path":"externalId"}`:                                                                    "noTarget",
		`{"op":"replace","path":"emails[type eq \"home\"]","value":1}`:                                           "noTarget",
		`{"op":"move","path":"displayName","value":"Countess"}`:                                                  "invalidValue",
		`{"op":"replace","path":"displayName"}`:                                                                  "invalidValue",
		`{"op":"replace","path":7,"value":"Countess"}`:                                                           "invalidPath",
		`{"op":"replace","path":"emails[","value":"Countess"}`:                                                   "invalidPath",
		`{"op":"replace","path":"` + GroupSchema + `:displayName","value":"Countess"}`:                           "invalidPath",
		strings.TrimSuffix(strings.Repeat(`{"op":"replace","path":"displayName","value":"Countess"},`, 33), ","): "invalidValue",
	} {
		selectNothing := func([]any, filter.Expression) ([]int, error) { return nil, nil }
		if _, err := Patch(decode(t, storedUser), decode(t, patch(operation)), false, selectNothing); problem(err) != want {
			t.Errorf("%.80s: got %s, want %s", operation, problem(err), want)
		}
	}
	withoutSchema := decode(t, `{"schemas":["`+UserSchema+`"],"Operations":[{"op":"remove","path":"displayName"}]}`)
	if _, err := Patch(decode(t, storedUser), withoutSchema, false, unfiltered); problem(err) != "invalidValue" {
		t.Errorf("a body without the PATCH schema: got %s", problem(err))
	}
}

func TestProjectionAlwaysReturnsIdentifyingAttributes(t *testing.T) {
	for _, c := range []struct{ attributes, excluded, want string }{
		{"userName,emails.value", "", `{
			"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"], "id": "user-1", "meta": {"resourceType": "User"},
			"userName": "ada@example.com", "emails": [{"value": "ada@example.com"}]}`},
		{"", "ID, emails.type, NAME, meta", `{
			"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"], "id": "user-1", "meta": {"resourceType": "User"},
			"userName": "ada@example.com", "displayName": "Ada", "emails": [{"value": "ada@example.com", "primary": true}]}`},
		{"", "", storedUser},
	} {
		projection, err := ParseProjection(c.attributes, c.excluded, UserSchema)
		if err != nil {
			t.Errorf("attributes=%q excludedAttributes=%q: %v", c.attributes, c.excluded, err)
			continue
		}
		if got, want := canonical(t, projection.Apply(decode(t, storedUser))), canonical(t, decode(t, c.want)); got != want {
			t.Errorf("attributes=%q excludedAttributes=%q\n got %s\nwant %s", c.attributes, c.excluded, got, want)
		}
	}
	for _, c := range []struct{ attributes, excluded string }{
		{"userName", "emails"},
		{`emails[type eq "work"]`, ""},
		{GroupSchema + ":displayName", ""},
		{"userName,", ""},
	} {
		if _, err := ParseProjection(c.attributes, c.excluded, UserSchema); err == nil {
			t.Errorf("attributes=%q excludedAttributes=%q was accepted", c.attributes, c.excluded)
		}
	}
}

func TestParseFilterBoundsSizeAndNesting(t *testing.T) {
	nested := func(depth int) string {
		return strings.Repeat("(", depth) + `userName pr` + strings.Repeat(")", depth)
	}
	for raw, accepted := range map[string]bool{
		nested(16): true,
		nested(17): false,
		`userName eq "` + strings.Repeat("(", 64) + `"`:   true,
		`userName eq "` + strings.Repeat("a", 4096) + `"`: false,
		`userName eq "ada`:   false,
		`userName eq "ada")`: false,
		`userName is "ada"`:  false,
	} {
		if _, err := ParseFilter(raw); (err == nil) != accepted {
			t.Errorf("%.40s: accepted=%v, want %v", raw, err == nil, accepted)
		}
	}
}

func FuzzCompileKeepsCallerValuesOutOfSQL(f *testing.F) {
	for _, seed := range []string{"ada", "x' OR 1=1 --", `"; DROP TABLE olp.users; --`, `\`, "$1", "%_", "resource", "2026-01-02T03:04:05Z", "[*]", ""} {
		f.Add(seed)
	}
	compile := func(filter string) (string, []any, error) {
		compiler := SQLFilter{Schema: UserSchema}
		sql, err := compiler.Compile(filter, "resource")
		return sql, compiler.Args, err
	}
	f.Fuzz(func(t *testing.T, value string) {
		literal, err := json.Marshal(value)
		if err != nil {
			t.Skip()
		}
		for _, attribute := range []string{"userName", "id", "meta.created", `emails[value`} {
			for _, operator := range []string{"eq", "ne", "co", "sw", "ew", "gt", "le"} {
				shape := func(literal string) string {
					return attribute + " " + operator + " " + literal + strings.Repeat("]", strings.Count(attribute, "["))
				}
				reference, _, referenceErr := compile(shape(`"2020-02-03T04:05:06Z"`))
				sql, arguments, err := compile(shape(string(literal)))
				if referenceErr != nil || err != nil {
					continue
				}
				if sql != reference {
					t.Errorf("%s: the SQL text depends on the compared value\n got %s\nwant %s", shape(string(literal)), sql, reference)
				}
				if strings.Count(sql, "$") != len(arguments) {
					t.Errorf("%s: %d placeholders for %d arguments in %s", shape(string(literal)), strings.Count(sql, "$"), len(arguments), sql)
				}
			}
		}
	})
}
