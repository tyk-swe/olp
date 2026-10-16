package scripted

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseDirectivesReportsMistakes(t *testing.T) {
	for text, fragment := range map[string]string{
		`[[olp:tool]]`:                  "tool name",
		`[[olp:tool x {bad}]]`:          "not JSON",
		`[[olp:tool x [1]]]`:            "JSON object",
		`[[olp:tool x {"a":1}`:          "closing",
		`[[olp:also x]]`:                "before it",
		`[[olp:reply hello]]`:           "not JSON",
		`[[olp:reply 3]]`:               "JSON string",
		`[[olp:fail 200]]`:              "error status",
		`[[olp:fail "x"]]`:              "error status",
		`[[olp:sing "x"]]`:              "unknown keyword",
		`[[olp:reply]]`:                 "missing argument",
		`[[olp:tool a {}]][[olp:oops]]`: "unknown keyword",
	} {
		if _, err := parseDirectives(text); err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("%s: error %v, want one about %q", text, err, fragment)
		}
	}
	if d, err := parseDirectives("no directive here, even [[1,2]] or [[olpx]]"); err != nil || len(d.rounds) != 0 {
		t.Fatalf("plain text parsed as %+v, %v", d, err)
	}
}

func declared(names ...string) *turn {
	t := newTurn()
	for _, n := range names {
		t.declare(n, json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`))
	}
	return t
}

// one of two results: round not yet answered

// A tool-result message may also carry unrelated text, which is not a prompt.

// Once the loop produced a result, a still-forced request gets its text.

func TestSampleFollowsTheSchema(t *testing.T) {
	for name, tc := range map[string]struct{ schema, want string }{
		"object":      {`{"type":"object","properties":{"s":{"type":"string"},"i":{"type":"integer"},"n":{"type":"number"},"b":{"type":"boolean"},"z":{"type":"null"}}}`, `{"b":true,"i":42,"n":4.5,"s":"fixture","z":null}`},
		"array":       {`{"type":"array","items":{"type":"integer"},"minItems":2}`, `[42,42]`},
		"enum":        {`{"enum":["red","green"]}`, `"red"`},
		"const":       {`{"const":7}`, `7`},
		"nullable":    {`{"type":["null","string"]}`, `"fixture"`},
		"anyOf":       {`{"anyOf":[{"type":"null"},{"type":"integer"}]}`, `42`},
		"allOf":       {`{"allOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"boolean"}}}]}`, `{"a":"fixture","b":true}`},
		"ref":         {`{"type":"object","properties":{"item":{"$ref":"#/$defs/item"}},"$defs":{"item":{"type":"string","format":"date"}}}`, `{"item":"2025-01-01"}`},
		"bounds":      {`{"type":"integer","minimum":100}`, `100`},
		"upper":       {`{"type":"integer","maximum":10}`, `10`},
		"minLength":   {`{"type":"string","minLength":9}`, `"fixturexx"`},
		"maxLength":   {`{"type":"string","maxLength":3}`, `"fix"`},
		"upperCase":   {`{"type":"OBJECT","properties":{"a":{"type":"STRING"}}}`, `{"a":"fixture"}`},
		"inferred":    {`{"properties":{"a":{"type":"string"}}}`, `{"a":"fixture"}`},
		"emptySchema": {`{}`, `{}`},
		"cycle":       {`{"$ref":"#"}`, `null`},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(sample(json.RawMessage(tc.schema)))
			if err != nil || string(encoded) != tc.want {
				t.Fatalf("sample %s = %s (%v), want %s", tc.schema, encoded, err, tc.want)
			}
		})
	}
	if got, _ := json.Marshal(sample(nil)); string(got) != "{}" {
		t.Fatalf("no schema %s", got)
	}
}
