package scim

import (
	"strings"
	"testing"
)

func TestCaseInsensitiveAttributesRejectAmbiguousInput(t *testing.T) {
	d, err := Decode(strings.NewReader(`{"SCHEMAS":["urn:ietf:params:scim:schemas:core:2.0:User"],"USERNAME":"a@example.com","NaMe":{"GivenNAME":"A"}}`))
	if err != nil || d["userName"] != "a@example.com" || d["name"].(map[string]any)["givenName"] != "A" {
		t.Fatalf("%v %v", d, err)
	}
	for _, raw := range []string{`{"userName":"a","USERNAME":"b"}`, `{"userName":"a","userName":"b"}`, `{"name":{"givenName":"a","givenname":"b"}}`, `null`, `[]`, `{} {}`} {
		if _, err = Decode(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

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
	for _, raw := range []string{strings.Repeat("(", 17) + `active pr` + strings.Repeat(")", 17), strings.Repeat("x", 4097), `active gt true`, `userName =~ "x"`} {
		if _, err := (&SQLFilter{}).Compile(raw, "document"); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestPatchAndProjectionPreserveOriginalDocument(t *testing.T) {
	current, _ := Decode(strings.NewReader(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"stable","userName":"a@example.com","name":{"givenName":"A","familyName":"Old"},"active":true}`))
	body, _ := Decode(strings.NewReader(`{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","path":"NAME.familyNAME","value":"New"}]}`))
	next, err := Patch(current, body, false, nil)
	if err != nil || next["name"].(map[string]any)["familyName"] != "New" || current["name"].(map[string]any)["familyName"] != "Old" {
		t.Fatalf("%v %v", next, err)
	}
	selected, err := Project(next, "name.givenName", "")
	if err != nil || selected["id"] != "stable" || len(selected["name"].(map[string]any)) != 1 {
		t.Fatalf("%v %v", selected, err)
	}
	body["operations"] = []any{map[string]any{"op": "replace", "path": "id", "value": "changed"}}
	if _, err = Patch(current, body, false, nil); err == nil {
		t.Fatal("immutable identifier changed")
	}
}

func TestMultivalueProjectionAndPatch(t *testing.T) {
	current, _ := Decode(strings.NewReader(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"id":"stable","emails":[{"value":"a@example.com","type":"home"},{"value":"b@example.com","type":"work","primary":true}]}`))
	selected, err := Project(current, "emails.value,emails.primary", "")
	if err != nil {
		t.Fatal(err)
	}
	entries := selected["emails"].([]any)
	if len(entries) != 2 || entries[1].(map[string]any)["primary"] != true || entries[0].(map[string]any)["type"] != nil {
		t.Fatal(selected)
	}
	excluded, err := Project(current, "", "emails.type")
	if err != nil || excluded["emails"].([]any)[0].(map[string]any)["type"] != nil {
		t.Fatal(excluded, err)
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
