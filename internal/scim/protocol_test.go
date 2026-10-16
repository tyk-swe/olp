package scim

import (
	"strings"
	"testing"
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
	for _, raw := range []string{strings.Repeat("(", 17) + `active pr` + strings.Repeat(")", 17), strings.Repeat("x", 4097), `active gt true`, `userName =~ "x"`} {
		if _, err := (&SQLFilter{}).Compile(raw, "document"); err == nil {
			t.Fatalf("accepted %s", raw)
		}
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
