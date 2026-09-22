package tokenization_test

import (
	"encoding/base64"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/tokenization"
	"strings"
	"testing"
)

func definition(t *testing.T, id string) operations.Dialect {
	t.Helper()
	for _, d := range tokenization.Definitions() {
		if d.Identity.ID == id {
			return d
		}
	}
	t.Fatal("missing dialect")
	return operations.Dialect{}
}
func request(t *testing.T, d operations.Dialect, raw string) oif.Request {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: d.Operation, Dialect: d.Identity}, doc)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func result(t *testing.T, r oif.Request, raw string) oif.Result {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := oif.NewResult(r.Descriptor(), doc, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func code(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	safe, ok := err.(interface {
		Incompatibility() (string, string, string, string)
	})
	if !ok {
		t.Fatal(err)
	}
	got, _, _, _ := safe.Incompatibility()
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestNativeCountsAreExactResultsNotUsageOrEstimates(t *testing.T) {
	for _, tc := range []struct{ id, body, response, scope, field string }{
		{"openai-input-tokens", `{"model":"route","input":"a","instructions":null,"parallel_tool_calls":false,"native":{"a":-0}}`, `{"object":"response.input_tokens","input_tokens":9007199254740993,"vendor":{"scope":"prompt"}}`, "responses-input", "input_tokens"},
		{"anthropic-count-tokens", `{"model":"route","messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}],"system":"system","tools":[],"thinking":{"type":"disabled"}}`, `{"input_tokens":9007199254740993,"opaque":{"native":false}}`, "messages-system-tools", "input_tokens"},
		{"gemini-count-tokens", `{"contents":[{"parts":[{"text":"a"}]}]}`, `{"totalTokens":9007199254740993,"cachedContentTokenCount":0,"promptTokensDetails":[{"modality":"TEXT","tokenCount":1},{"modality":"FUTURE","tokenCount":2}],"cacheTokensDetails":[]}`, "contents", "totalTokens"},
		{"bedrock-count-tokens", `{"input":{"converse":{"messages":[{"role":"user","content":[{"text":"a"}]}],"system":[{"text":"system"}]}}}`, `{"inputTokens":9007199254740993,"native":"metadata"}`, "converse", "inputTokens"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			d := definition(t, tc.id)
			r := request(t, d, tc.body)
			view, err := d.Request(r)
			if err != nil {
				t.Fatal(err)
			}
			req := view.(tokenization.CountRequest)
			if req.Scope() != tc.scope || req.Source().Document().Raw() != tc.body {
				t.Fatal("request scope or source changed")
			}
			view, err = d.Result(r, result(t, r, tc.response))
			if err != nil {
				t.Fatal(err)
			}
			res := view.(tokenization.CountResult)
			if res.Method() != "provider_native" || res.Counts()[0].Name != tc.field || res.Counts()[0].Value.Raw() != "9007199254740993" || res.Source().Source().Raw() != tc.response {
				t.Fatal("native count was normalized or lost")
			}
			if d.Usage != nil {
				t.Fatal("count results must not become provider usage")
			}
			if d.Estimate == nil || d.Estimate(req) >= 9007199254740993 {
				t.Fatal("request estimate confused with observed count")
			}
			counts := res.Counts()
			counts[0].Scope = "changed"
			if res.Counts()[0].Scope == "changed" {
				t.Fatal("mutable count view")
			}
		})
	}
}
func TestCountPresenceNativeBranchesAndExactIntegerSpellings(t *testing.T) {
	d := definition(t, "openai-input-tokens")
	for _, body := range []string{`{}`, `{"model":null,"input":null}`, `{"input":"","instructions":"","tools":null}`, `{"input":[],"parallel_tool_calls":false,"reasoning":null,"text":null}`} {
		r := request(t, d, body)
		v, err := d.Request(r)
		if err != nil {
			t.Fatal(err)
		}
		if v.(tokenization.CountRequest).Source().Document().Raw() != body {
			t.Fatal("presence changed")
		}
		for _, number := range []string{"0", "-0", "0.0", "1e3", "9007199254740993.0"} {
			body := `{"object":"response.input_tokens","input_tokens":` + number + `}`
			view, err := d.Result(r, result(t, r, body))
			if err != nil {
				t.Fatalf("native integer %s: %v", number, err)
			}
			if view.(tokenization.CountResult).Counts()[0].Value.Raw() != number {
				t.Fatal("integer spelling changed")
			}
		}
	}
	gemini := definition(t, "gemini-count-tokens")
	nested := `{"generateContentRequest":{"model":"models/route","contents":[{"parts":[{"text":"native"}]}],"systemInstruction":{"parts":[{"text":"system"}]},"generationConfig":{"temperature":-0},"tools":[{"functionDeclarations":[{"name":"tool","parameters":{"type":"object","properties":{"file_id":{"type":"string"},"__proto__":{"const":9007199254740993}}}}]}]}}`
	r := request(t, gemini, nested)
	v, err := gemini.Request(r)
	if err != nil {
		t.Fatal(err)
	}
	if v.(tokenization.CountRequest).Scope() != "generate-content-request" || v.(tokenization.CountRequest).Source().Document().Raw() != nested {
		t.Fatal("nested count controls lost")
	}
	if err := gemini.ValidateRoute(r, "route"); err != nil {
		t.Fatal(err)
	}
	changes, err := gemini.BindModel(r.Document(), "native-model")
	if err != nil {
		t.Fatal(err)
	}
	bound, err := r.WithChanges(changes...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bound.Document().Raw(), `"model":"models/native-model"`) || !strings.Contains(bound.Document().Raw(), `"__proto__":{"const":9007199254740993}`) {
		t.Fatal("nested model overlay changed other source")
	}
	absent := request(t, gemini, `{"generateContentRequest":{"contents":[]}}`)
	changes, err = gemini.BindModel(absent.Document(), "native-model")
	if err != nil || len(changes) != 0 {
		t.Fatal("absent nested model must stay absent")
	}
	bedrock := definition(t, "bedrock-count-tokens")
	payload := `{"inputText":"é","threshold":-0,"unsafe":9007199254740993}`
	body := `{"input":{"invokeModel":{"body":"` + base64.StdEncoding.EncodeToString([]byte(payload)) + `"}}}`
	v, err = bedrock.Request(request(t, bedrock, body))
	if err != nil {
		t.Fatal(err)
	}
	if v.(tokenization.CountRequest).Scope() != "invoke-model-body" || v.(tokenization.CountRequest).Source().Document().Raw() != body {
		t.Fatal("native invocation body was reencoded or degraded")
	}
}
func TestCountRejectsCorruptionAndUnownedReferencesPrecisely(t *testing.T) {
	for _, tc := range []struct{ id, body string }{
		{"openai-input-tokens", `{"input":false}`}, {"openai-input-tokens", `{"input":[0]}`}, {"openai-input-tokens", `{"parallel_tool_calls":0}`},
		{"anthropic-count-tokens", `{"model":"r","messages":null}`}, {"anthropic-count-tokens", `{"model":"r","messages":[{"role":"user","content":false}]}`}, {"anthropic-count-tokens", `{"model":"r","messages":[],"system":null}`},
		{"gemini-count-tokens", `{"contents":[],"generateContentRequest":{"contents":[]}}`}, {"gemini-count-tokens", `{"contents":[{"parts":null}]}`}, {"gemini-count-tokens", `{"generateContentRequest":false}`},
		{"bedrock-count-tokens", `{"input":{"converse":{},"invokeModel":{"body":"e30="}}}`}, {"bedrock-count-tokens", `{"input":{"invokeModel":{"body":"not-base64"}}}`}, {"bedrock-count-tokens", `{"input":{"converse":{"messages":[{"role":"user","content":"not-blocks"}]}}}`},
	} {
		d := definition(t, tc.id)
		_, err := d.Request(request(t, d, tc.body))
		code(t, err, "unsupported_parameter")
	}
	for _, id := range []string{"openai-input-tokens", "anthropic-count-tokens", "gemini-count-tokens", "bedrock-count-tokens"} {
		d := definition(t, id)
		r := request(t, d, string(d.Probe("route")))
		field := "input_tokens"
		extra := ""
		if id == "openai-input-tokens" {
			extra = `"object":"response.input_tokens",`
		}
		if id == "gemini-count-tokens" {
			field = "totalTokens"
		}
		if id == "bedrock-count-tokens" {
			field = "inputTokens"
		}
		for _, number := range []string{"null", "true", `"1"`, "-1", "1.5", "1e1000000000", "18446744073709551616"} {
			_, err := d.Result(r, result(t, r, `{`+extra+`"`+field+`":`+number+`}`))
			code(t, err, "fidelity_protocol_violation")
		}
	}
	for _, tc := range []struct{ id, body string }{
		{"openai-input-tokens", `{"input":"text","previous_response_id":"secret-reference"}`}, {"openai-input-tokens", `{"conversation":{"id":"secret-reference"}}`}, {"openai-input-tokens", `{"input":[{"type":"input_file","file_id":"secret-reference"}]}`}, {"openai-input-tokens", `{"input":[{"type":"item_reference","id":"secret-reference"}]}`},
		{"anthropic-count-tokens", `{"model":"r","messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"secret-reference"}}]}]}`},
		{"gemini-count-tokens", `{"generateContentRequest":{"cachedContent":"secret-reference","contents":[]}}`}, {"gemini-count-tokens", `{"contents":[{"parts":[{"fileData":{"fileUri":"secret-reference"}}]}]}`},
	} {
		d := definition(t, tc.id)
		_, err := d.Request(request(t, d, tc.body))
		code(t, err, "resource_affinity")
		if strings.Contains(err.Error(), "secret-reference") {
			t.Fatal("resource diagnostics leaked source")
		}
	}
	d := definition(t, "gemini-count-tokens")
	err := d.ValidateRoute(request(t, d, `{"generateContentRequest":{"model":"models/foreign","contents":[]}}`), "route")
	code(t, err, "resource_affinity")
}
func TestTokenizerPreservesOrderedTokensSpecialFlagsAndNativeOffsets(t *testing.T) {
	d := definition(t, "tei-tokenize")
	for _, tc := range []struct {
		input string
		count int
		batch bool
	}{{`"é"`, 1, false}, {`["é",""]`, 2, true}} {
		body := `{"inputs":` + tc.input + `,"add_special_tokens":false,"prompt_name":null,"unknown":{"preserved":-0}}`
		r := request(t, d, body)
		view, err := d.Request(r)
		if err != nil {
			t.Fatal(err)
		}
		req := view.(tokenization.TokenizeRequest)
		if req.Batch() != tc.batch || req.Inputs().Raw() != tc.input || req.Source().Document().Raw() != body {
			t.Fatal("tokenize input shape changed")
		}
		row := `[{"id":4294967295,"text":"<s>","special":true,"start":null,"stop":null},{"id":17,"text":"é","special":false,"start":0,"stop":3,"native":{"unknown":9007199254740993}},{"id":17,"text":"","special":false,"start":3,"stop":3}]`
		raw := "[" + row
		if tc.count == 2 {
			raw += ",[]"
		}
		raw += "]"
		view, err = d.Result(r, result(t, r, raw))
		if err != nil {
			t.Fatal(err)
		}
		out := view.(tokenization.TokenizeResult)
		tokens := out.Sets()[0].Tokens()
		if out.Method() != "provider_native" || out.Source().Source().Raw() != raw || tokens[0].ID.Raw() != "4294967295" || tokens[0].Start.Kind() != oif.Null || tokens[1].Text.Raw() != `"é"` || tokens[1].Stop.Raw() != "3" || tokens[2].ID.Raw() != "17" {
			t.Fatal("native tokens or offset units changed")
		}
		tokens[0].Position = 100
		if out.Sets()[0].Tokens()[0].Position != 0 {
			t.Fatal("mutable token view")
		}
		if d.Usage != nil {
			t.Fatal("token results are not usage")
		}
	}
}
func TestTokenizerRejectsCorruptShapeAndOffsets(t *testing.T) {
	d := definition(t, "tei-tokenize")
	r := request(t, d, `{"inputs":"text"}`)
	for _, raw := range []string{`[]`, `[[],[]]`, `[{"id":1,"text":"x","special":false,"start":0,"stop":1}]`, `[[{"id":-1,"text":"x","special":false,"start":0,"stop":1}]]`, `[[{"id":4294967296,"text":"x","special":false,"start":0,"stop":1}]]`, `[[{"id":1,"text":"x","special":null,"start":0,"stop":1}]]`, `[[{"id":1,"text":"x","special":false,"start":2,"stop":1}]]`, `[[{"id":1,"text":"x","special":true,"start":null,"stop":0}]]`, `[[{"id":1,"text":"x","special":false}]]`} {
		_, err := d.Result(r, result(t, r, raw))
		code(t, err, "fidelity_protocol_violation")
	}
	for _, raw := range []string{`{"inputs":null}`, `{"inputs":[]}`, `{"inputs":["a",false]}`, `{"inputs":"x","add_special_tokens":null}`, `{"inputs":"x","prompt_name":0}`} {
		_, err := d.Request(request(t, d, raw))
		code(t, err, "unsupported_parameter")
	}
	for _, raw := range []string{`{"inputs":""}`, `{"inputs":[""],"add_special_tokens":false,"prompt_name":""}`} {
		if _, err := d.Request(request(t, d, raw)); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCountPolicyCoversNativeSchemaTextAndRefusesOpaqueInputs(t *testing.T) {
	d := definition(t, "openai-input-tokens")
	r := request(t, d, `{"input":"visible","tools":[{"type":"function","name":"tool","description":"configured-secret","parameters":{"type":"object","properties":{"data":{"const":"also-covered"},"file_id":{"type":"string"}}}}]}`)
	texts, err := d.InputText(r)
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, text := range texts {
		all += text.Value + "|"
	}
	if !strings.Contains(all, "configured-secret") || !strings.Contains(all, "also-covered") {
		t.Fatal("effective tool/schema text escaped inspection")
	}
	for _, tc := range []struct{ id, body string }{
		{"openai-input-tokens", `{"input":"a","extension":{"hidden":"x"}}`},
		{"openai-input-tokens", `{"input":[{"type":"future_encoded","payload":"opaque"}]}`},
		{"openai-input-tokens", `{"input":"text","tools":[{"type":"web_search"}]}`},
		{"gemini-count-tokens", `{"contents":[{"parts":[{"futureBinary":"opaque"}]}]}`},
		{"anthropic-count-tokens", `{"model":"r","messages":[{"role":"user","content":[{"type":"future_encoded","payload":"opaque"}]}]}`}, {"openai-input-tokens", `{"input":[{"type":"message","content":[{"type":"input_image","image_url":"https://example.test/image"}]}]}`},
		{"anthropic-count-tokens", `{"model":"r","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`},
		{"bedrock-count-tokens", `{"input":{"invokeModel":{"body":"e30="}}}`}, {"tei-tokenize", `{"inputs":"text","prompt_name":"query"}`},
	} {
		d := definition(t, tc.id)
		r := request(t, d, tc.body)
		if _, err := d.Request(r); err != nil {
			t.Fatalf("native request unexpectedly refused before policy: %v", err)
		}
		_, err := d.InputText(r)
		code(t, err, "policy_conflict")
	}
	d = definition(t, "tei-tokenize")
	texts, err = d.OutputText(result(t, request(t, d, `{"inputs":"x"}`), `[[{"id":1,"text":"inspect-me","special":false,"start":0,"stop":1,"future":{"label":"also-visible"}}]]`))
	if err != nil {
		t.Fatal(err)
	}
	all = ""
	for _, text := range texts {
		all += text.Value + "|"
	}
	if !strings.Contains(all, "inspect-me") || !strings.Contains(all, "also-visible") {
		t.Fatal("output text escaped inspection")
	}
}
func TestDefinitionsAndOverlayPresenceAreOperationOwned(t *testing.T) {
	registry := operations.NewRegistry()
	for _, d := range tokenization.Definitions() {
		if err := registry.Register(d); err != nil {
			t.Fatal(err)
		}
		r := request(t, d, string(d.Probe("route")))
		if _, err := d.Request(r); err != nil {
			t.Fatalf("%s: %v", d.Identity.ID, err)
		}
		if d.Operation.ID != "token_count" {
			t.Fatal("counting became generation")
		}
	}
}

func TestPolicyInspectsArgumentJSONWithoutTreatingLiteralKeysAsResources(t *testing.T) {
	d := definition(t, "openai-input-tokens")
	r := request(t, d, `{"input":[{"type":"function_call","call_id":"call","name":"tool","arguments":"{\"data\":\"\\u0073ecret\",\"file_id\":\"literal\"}"}]}`)
	texts, err := d.InputText(r)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, text := range texts {
		if text.Value == "secret" {
			found = true
		}
	}
	if !found {
		t.Fatal("encoded JSON argument text escaped policy inspection")
	}
}
