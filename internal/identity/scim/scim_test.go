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

func TestPatchAddsFirstEmailFromOneObject(t *testing.T) {
	current := decode(t, `{"schemas":["`+UserSchema+`"],"userName":"ada@example.com"}`)
	body := decode(t, patch(`{"op":"add","path":"emails","value":{"value":"ada@example.com","primary":true}}`))
	patched, err := Patch(current, body, false, unfiltered)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canonical(t, patched["emails"]), `[{"primary":true,"value":"ada@example.com"}]`; got != want {
		t.Fatalf("emails = %s, want %s", got, want)
	}
}

func TestPatchRejectsRoleChangesAtomically(t *testing.T) {
	current := decode(t, `{"schemas":["`+UserSchema+`"],"userName":"ada@example.com","displayName":"Ada","roles":[{"value":"viewer"}]}`)
	before := canonical(t, current)
	for _, operation := range []string{
		`{"op":"remove","path":"roles"}`,
		`{"op":"remove","path":"Roles[value eq \"viewer\"]"}`,
		`{"op":"add","value":{"Roles":[{"value":"viewer"}]}}`,
	} {
		body := decode(t, patch(`{"op":"replace","path":"displayName","value":"Countess"},`+operation))
		patched, err := Patch(current, body, false, unfiltered)
		if problem(err) != "mutability" || patched != nil {
			t.Errorf("%s: patched = %v, error = %v", operation, patched, err)
		}
		if canonical(t, current) != before {
			t.Fatalf("the stored document changed: %s", canonical(t, current))
		}
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
			t.Fatal(err)
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

func TestNormalizeDoesNotMutateInputSlices(t *testing.T) {
	innerMap := map[string]any{"USERNAME": "ada", "VALUE": "ada@example.com"}
	innerSlice := []any{innerMap}
	input := []any{innerSlice, map[string]any{"DISPLAYNAME": "Ada"}}

	got, err := normalize(input)
	if err != nil {
		t.Fatalf("normalize failed: %v", err)
	}
	if _, stillUpper := innerMap["USERNAME"]; !stillUpper {
		t.Fatalf("input map was mutated: %v", innerMap)
	}
	if origSlice, ok := input[0].([]any); !ok || origSlice[0].(map[string]any)["USERNAME"] != "ada" {
		t.Fatalf("input nested slice was mutated in place: %v", input)
	}
	want := `[[{"userName":"ada","value":"ada@example.com"}],{"displayName":"Ada"}]`
	if canonical(t, got) != want {
		t.Fatalf("normalized = %s, want %s", canonical(t, got), want)
	}
}

func TestPatchNestedAndFilteredEdgeCases(t *testing.T) {
	base := decode(t, `{
		"schemas": ["`+UserSchema+`"],
		"id": "user-1",
		"userName": "ada@example.com",
		"name": {"givenName": "Ada"},
		"emails": [
			{"value": "ada@work.example", "type": "work", "primary": true},
			{"value": "ada@home.example", "type": "home", "primary": false}
		]
	}`)

	selectIndex := func(idx ...int) Select {
		return func([]any, filter.Expression) ([]int, error) { return idx, nil }
	}

	t.Run("filtered primary true clears existing primary", func(t *testing.T) {
		body := decode(t, patch(`{"op":"replace","path":"emails[type eq \"home\"].primary","value":true}`))
		patched, err := Patch(base, body, false, selectIndex(1))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `[{"primary":false,"type":"work","value":"ada@work.example"},{"primary":true,"type":"home","value":"ada@home.example"}]`
		if got := canonical(t, patched["emails"]); got != want {
			t.Fatalf("emails = %s, want %s", got, want)
		}
	})

	t.Run("filtered remove drops selected element", func(t *testing.T) {
		body := decode(t, patch(`{"op":"remove","path":"emails[type eq \"home\"]"}`))
		patched, err := Patch(base, body, false, selectIndex(1))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `[{"primary":true,"type":"work","value":"ada@work.example"}]`
		if got := canonical(t, patched["emails"]); got != want {
			t.Fatalf("emails = %s, want %s", got, want)
		}
	})

	t.Run("filtered add merges object fields into selected element", func(t *testing.T) {
		body := decode(t, patch(`{"op":"add","path":"emails[type eq \"work\"]","value":{"display":"Work"}}`))
		patched, err := Patch(base, body, false, selectIndex(0))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := `[{"display":"Work","primary":true,"type":"work","value":"ada@work.example"},{"primary":false,"type":"home","value":"ada@home.example"}]`
		if got := canonical(t, patched["emails"]); got != want {
			t.Fatalf("emails = %s, want %s", got, want)
		}
	})

	t.Run("nested complex object add merges keys and multi-valued subattribute updates all items", func(t *testing.T) {
		body := decode(t, patch(`
			{"op":"add","path":"name","value":{"familyName":"Lovelace"}},
			{"op":"replace","path":"emails.type","value":"other"}`))
		patched, err := Patch(base, body, false, unfiltered)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got, want := canonical(t, patched["name"]), `{"familyName":"Lovelace","givenName":"Ada"}`; got != want {
			t.Fatalf("name = %s, want %s", got, want)
		}
		wantEmails := `[{"primary":true,"type":"other","value":"ada@work.example"},{"primary":false,"type":"other","value":"ada@home.example"}]`
		if got := canonical(t, patched["emails"]); got != wantEmails {
			t.Fatalf("emails = %s, want %s", got, wantEmails)
		}
	})

	t.Run("filtered subattribute on non-complex item and non-slice target fail", func(t *testing.T) {
		scalarItems := decode(t, `{"schemas":["`+UserSchema+`"],"userName":"ada@example.com","emails":["raw"]}`)
		body := decode(t, patch(`{"op":"replace","path":"emails[value eq \"raw\"].value","value":"next"}`))
		if _, err := Patch(scalarItems, body, false, selectIndex(0)); problem(err) != "invalidPath" {
			t.Fatalf("got %q, want invalidPath", problem(err))
		}
		noEmails := decode(t, `{"schemas":["`+UserSchema+`"],"userName":"ada@example.com"}`)
		if _, err := Patch(noEmails, body, false, selectIndex(0)); problem(err) != "noTarget" {
			t.Fatalf("got %q, want noTarget", problem(err))
		}
	})

	t.Run("group patch rejects cross-schema filter", func(t *testing.T) {
		groupDoc := decode(t, `{"schemas":["`+GroupSchema+`"],"displayName":"Eng","members":[{"value":"u1"}]}`)
		body := decode(t, patch(`{"op":"remove","path":"members[`+UserSchema+`:value eq \"u1\"]"}`))
		if _, err := Patch(groupDoc, body, true, selectIndex(0)); problem(err) != "invalidFilter" {
			t.Fatalf("got %q, want invalidFilter", problem(err))
		}
	})
}
