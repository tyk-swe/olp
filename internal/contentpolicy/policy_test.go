package contentpolicy

import (
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, raw string) *Policy {
	t.Helper()
	policy, err := Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("Decode(%s) failed: %v", raw, err)
	}
	return policy
}

func decodeErr(t *testing.T, raw string) error {
	t.Helper()
	_, err := Decode(json.RawMessage(raw))
	if err == nil {
		t.Fatalf("Decode(%s) unexpectedly succeeded", raw)
	}
	return err
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"not an object":          `[{"id":"a"}]`,
		"trailing document":      `{"rules":[]} {"rules":[]}`,
		"trailing token":         `{"rules":[]} true`,
		"unknown field":          `{"rules":[],"extra":1}`,
		"rule unknown field":     `{"rules":[{"id":"r1","phase":"input","pattern":"x","action":"block","bogus":1}]}`,
		"missing id":             `{"rules":[{"phase":"input","pattern":"x","action":"block"}]}`,
		"bad id leading digit":   `{"rules":[{"id":"1bad","phase":"input","pattern":"x","action":"block"}]}`,
		"bad id character":       `{"rules":[{"id":"has space","phase":"input","pattern":"x","action":"block"}]}`,
		"duplicate id":           `{"rules":[{"id":"dup","phase":"input","pattern":"x","action":"block"},{"id":"dup","phase":"output","pattern":"y","action":"block"}]}`,
		"missing phase":          `{"rules":[{"id":"r1","pattern":"x","action":"block"}]}`,
		"bad phase":              `{"rules":[{"id":"r1","phase":"side","pattern":"x","action":"block"}]}`,
		"missing action":         `{"rules":[{"id":"r1","phase":"input","pattern":"x"}]}`,
		"bad action":             `{"rules":[{"id":"r1","phase":"input","pattern":"x","action":"drop"}]}`,
		"missing pattern":        `{"rules":[{"id":"r1","phase":"input","action":"block"}]}`,
		"empty pattern":          `{"rules":[{"id":"r1","phase":"input","pattern":"","action":"block"}]}`,
		"invalid regex":          `{"rules":[{"id":"r1","phase":"input","pattern":"a(","action":"block"}]}`,
		"empty match star":       `{"rules":[{"id":"r1","phase":"input","pattern":"a*","action":"block"}]}`,
		"empty match anchor":     `{"rules":[{"id":"r1","phase":"input","pattern":"^","action":"block"}]}`,
		"empty match optional":   `{"rules":[{"id":"r1","phase":"input","pattern":"(a)?","action":"block"}]}`,
		"block with replacement": `{"rules":[{"id":"r1","phase":"input","pattern":"x","action":"block","replacement":"y"}]}`,
		"oversized replacement":  `{"rules":[{"id":"r1","phase":"input","pattern":"x","action":"redact","replacement":"` + strings.Repeat("r", 129) + `"}]}`,
		"oversized pattern":      `{"rules":[{"id":"r1","phase":"input","pattern":"` + strings.Repeat("p", 513) + `","action":"block"}]}`,
	}
	for name, raw := range cases {
		if err := decodeErr(t, raw); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}
