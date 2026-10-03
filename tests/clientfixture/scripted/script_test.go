package scripted

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestParseDirectives(t *testing.T) {
	d, err := parseDirectives(`Read it [[olp:tool read {"paths":[[1,2],[3]],"note":"a ]] b"}]] then [[olp:also stat]] and [[olp:tool done {}]]` +
		` [[olp:reply "all \"done\""]] [[olp:think "hmm"]] [[olp:fail 503]]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rounds) != 2 || len(d.rounds[0]) != 2 || len(d.rounds[1]) != 1 {
		t.Fatalf("rounds %+v", d.rounds)
	}
	first := d.rounds[0][0]
	if first.name != "read" || string(first.args) != `{"paths":[[1,2],[3]],"note":"a ]] b"}` || first.round != 0 || first.index != 0 {
		t.Fatalf("first call %+v", first)
	}
	if second := d.rounds[0][1]; second.name != "stat" || string(second.args) != "{}" || second.index != 1 {
		t.Fatalf("parallel call %+v", second)
	}
	if d.reply == nil || *d.reply != `all "done"` || d.think != "hmm" || d.fail != 503 {
		t.Fatalf("directives %+v", d)
	}
	if id := first.id("call"); id != "call_fx_1_1" {
		t.Fatalf("id %s", id)
	}
}

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

func TestDecideWalksRoundsByToolResults(t *testing.T) {
	prompt := `go [[olp:tool a {"n":1}]] [[olp:also b {}]] [[olp:tool c {}]] [[olp:reply "finished"]]`
	steps := []struct {
		results  []string
		script   string
		calls    []string
		text     string
		finishes bool
	}{
		{nil, "tool_call", []string{"a", "b"}, "", false},
		{[]string{"ra"}, "tool_call", []string{"a", "b"}, "", false}, // one of two results: round not yet answered
		{[]string{"ra", "rb"}, "tool_call", []string{"c"}, "", false},
		{[]string{"ra", "rb", "rc"}, "reply", nil, "finished", true},
	}
	for i, s := range steps {
		tr := declared("a", "b", "c")
		tr.user(prompt)
		tr.results(s.results...)
		r := decide(tr)
		var names []string
		for _, c := range r.calls {
			names = append(names, c.name)
		}
		if r.script != s.script || !reflect.DeepEqual(names, s.calls) || r.text != s.text {
			t.Errorf("step %d: %+v, want script %s calls %v text %q", i, r, s.script, s.calls, s.text)
		}
	}
}

func TestDecideIgnoresResultsAndDirectivesOfEarlierPrompts(t *testing.T) {
	tr := declared("a")
	tr.user(`first [[olp:tool a {}]]`)
	tr.results("old result")
	tr.user(`second [[olp:tool a {"again":true}]]`)
	r := decide(tr)
	if r.script != "tool_call" || string(r.calls[0].args) != `{"again":true}` {
		t.Fatalf("the second prompt's own loop was not started: %+v", r)
	}
	tr.results("new result")
	if r := decide(tr); r.script != "tool_result" || r.text != ToolResultsPrefix+"new result" {
		t.Fatalf("after the second loop: %+v", r)
	}
}

func TestDecideSurvivesNoiseAroundTheToolResult(t *testing.T) {
	// A tool-result message may also carry unrelated text, which is not a prompt.
	tr := declared("a")
	tr.user("context")
	tr.user(`work [[olp:tool a {}]]`)
	tr.results("done")
	tr.user("")
	if r := decide(tr); r.script != "tool_result" {
		t.Fatalf("%+v", r)
	}
}

func TestDecideNeverCallsAnUndeclaredTool(t *testing.T) {
	tr := declared("other")
	tr.user(`[[olp:tool missing {}]]`)
	r := decide(tr)
	if r.script != "tool_undeclared" || len(r.calls) != 0 || !strings.Contains(r.text, "missing") {
		t.Fatalf("%+v", r)
	}
	tr.forced = "other2"
	tr.messages = nil
	tr.user("plain")
	if r := decide(tr); r.script != "tool_undeclared" {
		t.Fatalf("forced undeclared tool: %+v", r)
	}
}

func TestDecideForcedToolsAndStructuredOutput(t *testing.T) {
	tr := newTurn()
	tr.declare("json", json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`))
	tr.user("extract")
	tr.forced = "*"
	r := decide(tr)
	if r.script != "tool_call" || r.calls[0].name != "json" || string(r.calls[0].args) != `{"n":42}` {
		t.Fatalf("forced call %+v", r)
	}
	// Once the loop produced a result, a still-forced request gets its text.
	tr.results("ok")
	if r := decide(tr); r.script != "tool_result" {
		t.Fatalf("after the forced call %+v", r)
	}

	tr = newTurn()
	tr.user("x")
	tr.schema = json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	if r := decide(tr); r.script != "structured" || r.text != `{"ok":true}` {
		t.Fatalf("structured %+v", r)
	}
	tr.schema, tr.jsonMode = nil, true
	if r := decide(tr); r.script != "structured" || r.text != `{"result":"fixture"}` {
		t.Fatalf("json mode %+v", r)
	}
}

func TestDecideHidesToolsWhenToolChoiceIsNone(t *testing.T) {
	tr := declared("a")
	tr.hideTools()
	tr.user(`[[olp:tool a {}]]`)
	if r := decide(tr); r.script != "tool_undeclared" {
		t.Fatalf("%+v", r)
	}
}

func TestDecideScriptedFailuresAndMistakes(t *testing.T) {
	tr := newTurn()
	tr.user(`[[olp:fail 500]]`)
	if r := decide(tr); r.failStatus != 500 || r.script != "fail" {
		t.Fatalf("%+v", r)
	}
	tr = newTurn()
	tr.user(`[[olp:tool x {oops}]]`)
	if r := decide(tr); r.failStatus != http.StatusBadRequest || r.script != "directive_error" || !strings.Contains(r.failMessage, "malformed") {
		t.Fatalf("%+v", r)
	}
}

func TestDecideReasoning(t *testing.T) {
	tr := newTurn()
	tr.user("hi")
	if r := decide(tr); r.reasoning != "" {
		t.Fatalf("reasoning without a request for it: %q", r.reasoning)
	}
	tr.thinking = true
	if r := decide(tr); r.reasoning != DefaultReasoning {
		t.Fatalf("requested reasoning %q", r.reasoning)
	}
	tr.messages = nil
	tr.user(`[[olp:think "custom"]]`)
	if r := decide(tr); r.reasoning != "custom" {
		t.Fatalf("directive reasoning %q", r.reasoning)
	}
}

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
