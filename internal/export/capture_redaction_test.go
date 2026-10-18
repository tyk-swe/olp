package export

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func redactedPolicy(t *testing.T) CapturePolicy {
	t.Helper()
	p := sampledPolicy("1")
	p.Redact = []string{"private"}
	p.RedactorRevisions = []CaptureRedactor{{Name: "private", GuardrailID: "g", RevisionID: "r1", Revision: 1, Type: "builtin.regex"}}
	if err := p.compileRedactors("project", map[string]guardrails.Revision{"r1": {ID: "r1", GuardrailID: "g", ProjectID: "project", Type: "builtin.regex", TimeoutMS: 1000, OnError: guardrails.FailOpen, Configuration: json.RawMessage(`{"rules":[{"id":"secret","pattern":"secret","replacement":"[REMOVED]"}]}`)}}); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestCaptureRedactionDecodedFieldsAndUnchangedSource(t *testing.T) {
	p := redactedPolicy(t)
	m := testManager(p)
	c := m.Resolve("project", "", "", "", "req")
	raw := json.RawMessage(`[{"role":"user","content":"se\u0063ret","tool_calls":[{"function":{"name":"f","arguments":"{\"key\":\"se\\u0063ret\"}"}}]}]`)
	original := string(raw)
	c.CaptureDocument(map[string]json.RawMessage{"messages": raw})
	c.CaptureText("output", "secret")
	call := json.RawMessage(`{"function":{"name":"f","arguments":"{\"key\":\"se\\u0063ret\"}"}}`)
	c.CaptureToolCall(call)
	if !c.Finish(CaptureIdentity{ProjectID: "project"}, time.Now()) {
		t.Fatal("redaction did not queue")
	}
	queued := <-m.queue
	if strings.Contains(string(queued.record), "secret") || strings.Contains(string(queued.record), `0063`) {
		t.Fatal("raw content in queue")
	}
	var record struct {
		Data struct {
			Input  map[string]json.RawMessage
			Output string
			Tools  []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
		}
	}
	if err := json.Unmarshal(queued.record, &record); err != nil {
		t.Fatal(err)
	}
	if record.Data.Output != "[REMOVED]" || !json.Valid([]byte(record.Data.Tools[0].Function.Arguments)) || !strings.Contains(record.Data.Tools[0].Function.Arguments, "[REMOVED]") {
		t.Fatal("replacement broke capture JSON")
	}
	if string(raw) != original || string(call) != `{"function":{"name":"f","arguments":"{\"key\":\"se\\u0063ret\"}"}}` {
		t.Fatal("inference source mutated")
	}
}
func TestCaptureRedactionEligibilityAndBindings(t *testing.T) {
	for _, kind := range []string{"builtin.regex", "builtin.pii", "builtin.secrets", "builtin.keywords", "builtin.schema", "builtin.tools", "webhook", "model", "plugin", "presidio"} {
		want := kind == "builtin.regex" || kind == "builtin.pii" || kind == "builtin.secrets" || kind == "builtin.keywords"
		if captureDetectorEligible(kind) != want {
			t.Fatalf("eligibility %s", kind)
		}
	}
	for _, test := range []string{"missing", "scope", "owner", "name", "type"} {
		t.Run(test, func(t *testing.T) {
			p := redactedPolicy(t)
			d := guardrails.Revision{ID: "r1", GuardrailID: "g", ProjectID: "project", Type: "builtin.regex", OnError: guardrails.FailOpen, TimeoutMS: 1, Configuration: json.RawMessage(`{"rules":[]}`)}
			defs := map[string]guardrails.Revision{"r1": d}
			switch test {
			case "missing":
				delete(defs, "r1")
			case "scope":
				d.ProjectID = "other"
				defs["r1"] = d
			case "owner":
				d.GuardrailID = "other"
				defs["r1"] = d
			case "name":
				p.Redact[0] = "other"
			case "type":
				d.Type = "webhook"
				defs["r1"] = d
			}
			if p.compileRedactors("project", defs) == nil || !p.redactorInvalid {
				t.Fatal("invalid binding accepted")
			}
			m := testManager(p)
			if m.Resolve("project", "", "", "", "req") != nil {
				t.Fatal("invalid cache policy served")
			}
			_, _, dropped, _, failed := m.Metrics()
			if dropped != 1 || failed != 1 {
				t.Fatal("missing content-free drop counters")
			}
		})
	}
}

func TestCaptureRedactsAllJSONScalarTypes(t *testing.T) {
	p := sampledPolicy("1")
	p.Redact = []string{"scalars"}
	p.RedactorRevisions = []CaptureRedactor{{Name: "scalars", GuardrailID: "g", RevisionID: "r", Type: "builtin.regex"}}
	if err := p.compileRedactors("project", map[string]guardrails.Revision{"r": {ID: "r", GuardrailID: "g", ProjectID: "project", Type: "builtin.regex", TimeoutMS: 1000, Configuration: json.RawMessage(`{"rules":[{"id":"scalars","pattern":"^(true|false|null|123)$","replacement":"[REMOVED]"}]}`)}}); err != nil {
		t.Fatal(err)
	}
	input := map[string]json.RawMessage{"input": json.RawMessage(`[true,false,null,123,"text",456]`)}
	calls := []json.RawMessage{json.RawMessage(`{"name":"f","arguments":"{\"a\":true,\"b\":false,\"c\":null}"}`)}
	redacted, _, tools, err := redactCapture(t.Context(), p.redactor, input, "", calls)
	if err != nil || string(redacted["input"]) != `["[REMOVED]","[REMOVED]","[REMOVED]","[REMOVED]","text",456]` {
		t.Fatalf("scalar inspection: %s %v", redacted["input"], err)
	}
	if len(tools) != 1 || strings.Count(string(tools[0]), "[REMOVED]") != 3 {
		t.Fatalf("nested scalar inspection: %s", tools)
	}
	unchanged := redactedPolicy(t)
	preserved, _, preservedTools, err := redactCapture(t.Context(), unchanged.redactor, input, "", calls)
	if err != nil || string(preserved["input"]) != string(input["input"]) || len(preservedTools) != 1 || string(preservedTools[0]) != `{"arguments":"{\"a\":true,\"b\":false,\"c\":null}","name":"f"}` {
		t.Fatalf("unchanged scalar types lost: %s %s %v", preserved["input"], preservedTools, err)
	}
}

func TestCaptureRedactsOrdinaryToolArgumentKeys(t *testing.T) {
	p := redactedPolicy(t)
	arguments := `{"source":"secret","document":{"image":"secret"},"audio":"secret","arguments":"secret","type":"image","inlineData":{"data":"secret"}}`
	encoded, _ := json.Marshal(arguments)
	for _, call := range []string{
		`{"function":{"name":"f","arguments":` + string(encoded) + `}}`,
		`{"name":"f","arguments":` + arguments + `}`,
		`{"type":"tool_use","name":"f","input":` + arguments + `}`,
		`{"name":"f","args":` + arguments + `}`,
	} {
		input := map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"assistant","tool_calls":[` + call + `]}]`)}
		redacted, _, calls, err := redactCapture(t.Context(), p.redactor, input, "", []json.RawMessage{json.RawMessage(call)})
		if err != nil || len(calls) != 1 {
			t.Fatalf("inspectable arguments dropped: %s: %v", call, err)
		}
		for _, raw := range []json.RawMessage{redacted["messages"], calls[0]} {
			if strings.Contains(string(raw), "secret") || strings.Count(string(raw), "[REMOVED]") != 5 || !json.Valid(raw) {
				t.Fatalf("arguments not redacted: %s", raw)
			}
		}
	}
	for _, block := range []string{
		`{"type":"image_url","image_url":{"url":"secret"}}`,
		`{"type":"image","source":{"type":"base64","data":"secret"}}`,
		`{"type":"input_file","file_data":"secret"}`,
		`{"inlineData":{"mimeType":"image/png","data":"secret"}}`,
		`{"image":{"format":"png","source":{"bytes":"secret"}}}`,
	} {
		input := map[string]json.RawMessage{"messages": json.RawMessage(`[{"content":[` + block + `]}]`)}
		if _, _, _, err := redactCapture(t.Context(), p.redactor, input, "", nil); err == nil {
			t.Fatalf("opaque media accepted: %s", block)
		}
	}
}

func TestCaptureRedactsStructuredToolResultJSON(t *testing.T) {
	p := redactedPolicy(t)
	for _, result := range []string{
		`{"arguments":"--version secret"}`,
		`{"name":"example","arguments":"--version secret","source":"secret","type":"image","image":{"data":"secret"}}`,
	} {
		for _, input := range []map[string]json.RawMessage{
			{"messages": json.RawMessage(`[{"role":"user","content":[{"toolResult":{"toolUseId":"call","content":[{"json":` + result + `}]}}]}]`)},
			{"contents": json.RawMessage(`[{"role":"user","parts":[{"functionResponse":{"name":"f","response":` + result + `}}]}]`)},
		} {
			redacted, _, _, err := redactCapture(t.Context(), p.redactor, input, "", nil)
			if err != nil {
				t.Fatalf("ordinary tool result dropped: %v", err)
			}
			for _, raw := range redacted {
				if strings.Contains(string(raw), "secret") || !strings.Contains(string(raw), "--version [REMOVED]") || !json.Valid(raw) {
					t.Fatalf("tool result not inspected: %s", raw)
				}
			}
		}
	}
}

func TestCaptureMalformedCallMetadataCannotBypassArgumentDecoding(t *testing.T) {
	for _, name := range []string{"", `"name":null,`, `"name":123,`, `"name":"",`} {
		for _, input := range []map[string]json.RawMessage{
			{"messages": json.RawMessage(`[{"role":"assistant","tool_calls":[{"function":{` + name + `"arguments":"{\"value\":\"se\\u0063ret\"}"}}]}]`)},
			{"messages": json.RawMessage(`[{"role":"assistant","function_call":{` + name + `"arguments":"{\"value\":\"se\\u0063ret\"}"}}]`)},
			{"input": json.RawMessage(`[{"type":"function_call",` + name + `"arguments":"{\"value\":\"se\\u0063ret\"}"}]`)},
		} {
			p := redactedPolicy(t)
			p.Include = []string{"input"}
			m := testManager(p)
			c := m.Resolve("project", "", "", "", "req")
			c.CaptureDocument(input)
			if !c.Finish(CaptureIdentity{ProjectID: "project", Mode: "unary", Outcome: "failure"}, time.Now()) {
				t.Fatal("inspectable arguments dropped")
			}
			raw := string((<-m.queue).record)
			if strings.Contains(raw, "0063") || strings.Contains(raw, "secret") || !strings.Contains(raw, "[REMOVED]") {
				t.Fatalf("malformed metadata bypassed argument decoding: %s", raw)
			}
		}
	}
}

func TestCaptureDropsTruncatedUnaryOutput(t *testing.T) {
	p := redactedPolicy(t)
	p.Include = []string{"output"}
	m := testManager(p)
	c := m.Resolve("project", "", "", "", "req")
	c.CaptureResponse(wire.Chat, []byte(`{"choices":[{"index":0,"message":{"content":"sec"},"finish_reason":"length"}]}`))
	if c.Finish(CaptureIdentity{ProjectID: "project"}, time.Now()) || len(m.queue) != 0 {
		t.Fatal("truncated sensitive content queued")
	}
}

func TestCompletionCaptureRetainsInputAndRedactsOutput(t *testing.T) {
	for _, stream := range []bool{false, true} {
		p := redactedPolicy(t)
		p.Include = []string{"input", "output", "tool_calls"}
		m := testManager(p)
		c := m.Resolve("project", "", "", "", "req")
		c.CaptureDocument(map[string]json.RawMessage{"prompt": json.RawMessage(`"input secret"`)})
		mode := "unary"
		if stream {
			mode = "streaming"
			c.CaptureStreamFrame(wire.Completions, []byte("data: "+`{"choices":[{"index":0,"text":"output secret","finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n"))
		} else {
			c.CaptureResponse(wire.Completions, []byte(`{"choices":[{"index":0,"text":"output secret","finish_reason":"stop"}]}`))
		}
		if !c.Finish(CaptureIdentity{ProjectID: "project", Mode: mode, Outcome: "success"}, time.Now()) {
			t.Fatalf("%s completion capture dropped", mode)
		}
		raw := string((<-m.queue).record)
		if strings.Contains(raw, "secret") || !strings.Contains(raw, "input [REMOVED]") || !strings.Contains(raw, "output [REMOVED]") {
			t.Fatalf("%s completion capture: %s", mode, raw)
		}
	}
}

func TestCaptureDropsFailedStreamsEvenWithCompleteSelectedContent(t *testing.T) {
	for _, include := range [][]string{{"input"}, {"input", "output"}} {
		for _, outcome := range []string{"failure", "cancelled", ""} {
			p := redactedPolicy(t)
			p.Include = include
			m := testManager(p)
			c := m.Resolve("project", "", "", "", "req")
			c.CaptureDocument(map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"secret"}]`)})
			c.CaptureStreamFrame(wire.Chat, []byte("data: "+`{"choices":[{"index":0,"delta":{"content":"secret"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n"))
			if c.Finish(CaptureIdentity{ProjectID: "project", Mode: "streaming", Outcome: outcome}, time.Now()) || len(m.queue) != 0 {
				t.Fatalf("stream outcome %q queued selected content %v", outcome, include)
			}
		}
	}
}
func TestCaptureRedactionDropsMalformedUnavailableCancelledAndBounds(t *testing.T) {
	for _, test := range []string{"malformed", "opaque", "missing", "field", "segments", "cancelled", "closed", "expanded"} {
		t.Run(test, func(t *testing.T) {
			p := redactedPolicy(t)
			m := testManager(p)
			c := m.Resolve("project", "", "", "", "req")
			c.CaptureDocument(map[string]json.RawMessage{"messages": json.RawMessage(`[{"content":"secret"}]`)})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch test {
			case "malformed":
				c.CaptureToolCall(json.RawMessage(`{"name":"f","arguments":"{broken"}`))
			case "opaque":
				c.CaptureDocument(map[string]json.RawMessage{"messages": json.RawMessage(`[{"content":[{"type":"image_url","image_url":"secret"}]}]`)})
			case "missing":
				c.CaptureDocument(nil)
			case "field":
				c.CaptureText("output", strings.Repeat("s", captureFieldMax+1))
			case "segments":
				c.CaptureDocument(map[string]json.RawMessage{"input": json.RawMessage(`["` + strings.Repeat(`x","`, guardrails.MaxSegments) + `x"]`)})
			case "cancelled":
				cancel()
			case "closed":
				c.Close()
			case "expanded":
				c.CaptureText("output", strings.Repeat("secret ", captureFieldMax/7))
			}
			if c.FinishContext(ctx, CaptureIdentity{}, time.Now()) {
				t.Fatal("unsafe capture queued")
			}
			if len(m.queue) != 0 || m.collectors != 0 {
				t.Fatal("drop retained content or collector")
			}
			if test != "closed" {
				_, _, d, _, r := m.Metrics()
				if d != 1 || r != 1 {
					t.Fatalf("counters dropped=%d failed=%d", d, r)
				}
			}
		})
	}
}
func TestCaptureRedactionBudgetAndInvalidVerdicts(t *testing.T) {
	for _, test := range []string{"timeout", "span", "unavailable"} {
		t.Run(test, func(t *testing.T) {
			evaluator := guardrails.EvaluatorFunc(func(ctx context.Context, _ guardrails.Revision, e guardrails.Evaluation) (guardrails.Verdict, error) {
				if test == "timeout" {
					<-ctx.Done()
					return guardrails.Verdict{}, ctx.Err()
				}
				if test == "unavailable" {
					return guardrails.Verdict{}, errors.New("unavailable")
				}
				return guardrails.Verdict{Action: guardrails.Redact, Redactions: []guardrails.Redaction{{Segment: e.Segments[0].ID, Start: 0, End: 10000}}}, nil
			})
			engine, err := guardrails.Compile([]guardrails.Entry{{GuardrailID: "g", RevisionID: "r", Phase: guardrails.Input, Action: guardrails.Redact}}, map[string]guardrails.Revision{"r": {ID: "r", GuardrailID: "g", Type: "webhook", Configuration: json.RawMessage(`{"endpoint":"https://example.com","content_egress":true,"credential_ref":"credential"}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}}, evaluator)
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			_, _, _, err = redactCapture(t.Context(), engine, nil, "secret", nil)
			if err == nil {
				t.Fatal("invalid evaluation accepted")
			}
			if time.Since(started) > 500*time.Millisecond {
				t.Fatal("budget not bounded")
			}
		})
	}
}
func TestCaptureRedactionSplitStreamAndTerminalSnapshots(t *testing.T) {
	p := redactedPolicy(t)
	p.Include = []string{"output", "tool_calls"}
	m := testManager(p)
	c := m.Resolve("project", "", "", "", "req")
	frames := []string{
		`{"choices":[{"index":0,"delta":{"content":"sec","tool_calls":[{"index":0,"function":{"name":"a","arguments":"{\"x\":\"sec"}},{"index":1,"function":{"name":"b","arguments":"{\"x\":\"sec"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"ret","tool_calls":[{"index":1,"function":{"arguments":"ret\"}"}},{"index":0,"function":{"arguments":"ret\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	for _, frame := range frames {
		c.CaptureStreamFrame(wire.Chat, []byte("data: "+frame+"\n\n"))
	}
	c.CaptureStreamFrame(wire.Chat, []byte("data: [DONE]\n\n"))
	if !c.Finish(CaptureIdentity{Mode: "streaming", Outcome: "success"}, time.Now()) {
		t.Fatal("complete split stream dropped")
	}
	raw := (<-m.queue).record
	if strings.Contains(string(raw), "secret") || strings.Count(string(raw), "[REMOVED]") != 3 {
		t.Fatalf("split secret not removed: %s", raw)
	}
}

func TestCaptureLocalDetectorsAndOrderedTransforms(t *testing.T) {
	for _, tc := range []struct{ kind, config, text string }{
		{"builtin.pii", `{"categories":["pii.email"]}`, "alice@example.com"},
		{"builtin.secrets", `{"categories":["secret.aws_access_key"]}`, "AKIAIOSFODNN7EXAMPLE"},
		{"builtin.keywords", `{"words":["secret"]}`, "SECRET"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			p := sampledPolicy("1")
			p.Include = []string{"output"}
			p.Redact = []string{"local"}
			p.RedactorRevisions = []CaptureRedactor{{Name: "local", GuardrailID: "g", RevisionID: "r", Type: tc.kind}}
			definitions := map[string]guardrails.Revision{"r": {ID: "r", GuardrailID: "g", ProjectID: "p", Type: tc.kind, Configuration: json.RawMessage(tc.config), TimeoutMS: 1000, OnError: guardrails.FailOpen}}
			if err := p.compileRedactors("p", definitions); err != nil {
				t.Fatal(err)
			}
			// Changing live management data after compilation cannot change this pin.
			d := definitions["r"]
			d.Configuration = json.RawMessage(`{}`)
			definitions["r"] = d
			m := testManager(p)
			c := m.Resolve("p", "", "", "", "r")
			c.CaptureText("output", tc.text)
			if !c.Finish(CaptureIdentity{}, time.Now()) {
				t.Fatal("local detector dropped")
			}
			if raw := (<-m.queue).record; strings.Contains(string(raw), tc.text) {
				t.Fatal("local detector did not redact")
			}
		})
	}
	p := sampledPolicy("1")
	p.Include = []string{"output"}
	p.Redact = []string{"first", "second"}
	defs := map[string]guardrails.Revision{}
	for i, rule := range []string{`{"rules":[{"id":"one","pattern":"secret","replacement":"intermediate"}]}`, `{"rules":[{"id":"two","pattern":"intermediate","replacement":"final"}]}`} {
		name := p.Redact[i]
		defs[name] = guardrails.Revision{ID: name, GuardrailID: name, ProjectID: "p", Type: "builtin.regex", Configuration: json.RawMessage(rule), TimeoutMS: 1000, OnError: guardrails.FailOpen}
		p.RedactorRevisions = append(p.RedactorRevisions, CaptureRedactor{Name: name, GuardrailID: name, RevisionID: name, Type: "builtin.regex"})
	}
	if err := p.compileRedactors("p", defs); err != nil {
		t.Fatal(err)
	}
	m := testManager(p)
	c := m.Resolve("p", "", "", "", "r")
	c.CaptureText("output", "secret")
	if !c.Finish(CaptureIdentity{}, time.Now()) {
		t.Fatal("ordered transform dropped")
	}
	if raw := string((<-m.queue).record); !strings.Contains(raw, `"output":"final"`) {
		t.Fatal("detector order was not applied")
	}
}
func TestCaptureMalformedJSONUnicodeAndKeyCollisions(t *testing.T) {
	p := redactedPolicy(t)
	for _, raw := range []string{`{"x":"\ud800"}`, `{"x":"secret","x":"safe"}`, `{"secret":"value","[REMOVED]":"other"}`} {
		if _, _, _, err := redactCapture(t.Context(), p.redactor, map[string]json.RawMessage{"input": json.RawMessage(raw)}, "", nil); err == nil {
			t.Fatalf("malformed JSON accepted: %s", raw)
		}
	}
	var names captureRedactorNames
	if json.Unmarshal([]byte(`null`), &names) == nil {
		t.Fatal("null redactors accepted")
	}
}

func TestCaptureIndependentlyInspectsServingTransformedInput(t *testing.T) {
	engine, err := guardrails.Compile([]guardrails.Entry{{GuardrailID: "serving", RevisionID: "serving", Phase: guardrails.Input, Action: guardrails.Redact}}, map[string]guardrails.Revision{"serving": {ID: "serving", GuardrailID: "serving", Type: "builtin.regex", Configuration: json.RawMessage(`{"rules":[{"id":"name","pattern":"original","replacement":"[SERVED]"}]}`), TimeoutMS: 1000, OnError: guardrails.FailClosed}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	served, err := engine.Evaluate(t.Context(), guardrails.Evaluation{Phase: guardrails.Input, Segments: []guardrails.Segment{{ID: "text", Text: "original secret"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	document, _ := json.Marshal([]map[string]string{{"role": "user", "content": served.Segments[0].Text}})
	p := redactedPolicy(t)
	p.Include = []string{"input"}
	m := testManager(p)
	c := m.Resolve("project", "", "", "", "req")
	c.CaptureDocument(map[string]json.RawMessage{"messages": document})
	if !c.Finish(CaptureIdentity{}, time.Now()) {
		t.Fatal("transformed input dropped")
	}
	raw := string((<-m.queue).record)
	if strings.Contains(raw, "secret") || !strings.Contains(raw, "[SERVED] [REMOVED]") {
		t.Fatal("serving transform bypassed independent capture redaction")
	}
	if served.Segments[0].Text != "[SERVED] secret" {
		t.Fatal("capture changed serving result")
	}
}
