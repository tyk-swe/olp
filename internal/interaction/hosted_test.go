package interaction

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func hostedSource(t *testing.T, body string) *openai.Request {
	t.Helper()
	return request(t, openai.FamilyResponses, body)
}

// The qualified direct-OpenAI Responses profile admits native web_search only
// when the key policy authorizes provider-hosted tool effects.
func TestHostedWebSearchAdmissionRequiresPermissionAndProfile(t *testing.T) {
	qualified := template(t, configuration(t, "openai-responses"))
	body := `{"model":"route","input":"hello","store":false,"tools":[{"type":"web_search"}]}`

	_, err := qualified.Bind(hostedSource(t, body), Context{})
	assertReason(t, err, "policy_conflict")

	plan := bind(t, qualified, hostedSource(t, body), Context{AllowHostedTools: true})
	if !slices.Contains(plan.Obligations().Effects, "hosted_tool_call") {
		t.Fatalf("hosted effect missing: %v", plan.Obligations().Effects)
	}

	// Every other Responses profile lacks the qualified hosted lifecycle.
	for _, id := range []string{"compatible-responses", "azure-v1-responses", "azure-legacy-responses"} {
		t.Run(id, func(t *testing.T) {
			_, err := template(t, configuration(t, id)).Bind(hostedSource(t, body), Context{AllowHostedTools: true, AllowProviderState: true, RetainedResponses: true})
			assertReason(t, err, "state_carrier")
		})
	}
	// Other dialects refuse the source before hosted admission runs.
	for _, id := range []string{"anthropic-messages", "gemini-generation", "bedrock-converse"} {
		t.Run(id, func(t *testing.T) {
			_, err := template(t, configuration(t, id)).Bind(hostedSource(t, body), Context{AllowHostedTools: true})
			assertReason(t, err, "target_capability")
		})
	}

	// Chat wire cannot declare a Responses hosted tool.
	chat := template(t, configuration(t, "openai-chat"))
	_, err = chat.Bind(request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`), Context{AllowHostedTools: true})
	assertReason(t, err, "state_carrier")

	// Chat web_search_options is a hosted search control without a declared
	// lifecycle; permission alone cannot admit it.
	_, err = chat.Bind(request(t, openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":"hi"}],"web_search_options":{"search_context_size":"low"}}`), Context{AllowHostedTools: true})
	assertReason(t, err, "state_carrier")
}

// Client function tools never need hosted-tool authorization, and mixed
// declarations record both effects distinctly.
func TestHostedWebSearchDoesNotTaxClientTools(t *testing.T) {
	compiled := template(t, configuration(t, "openai-responses"))
	plain := bind(t, compiled, hostedSource(t, `{"model":"route","input":"hello","store":false,"tools":[{"type":"function","name":"local","parameters":{"type":"object"}}]}`), Context{})
	if slices.Contains(plain.Obligations().Effects, "hosted_tool_call") {
		t.Fatalf("client function tool gained a hosted effect: %v", plain.Obligations().Effects)
	}
	mixed := bind(t, compiled, hostedSource(t, `{"model":"route","input":"hello","store":false,"tools":[{"type":"function","name":"local","parameters":{"type":"object"}},{"type":"web_search","search_context_size":"low"}]}`), Context{AllowHostedTools: true})
	effects := mixed.Obligations().Effects
	for _, effect := range []string{"inference", "client_tool_call", "hosted_tool_call"} {
		if !slices.Contains(effects, effect) {
			t.Fatalf("mixed declaration missing %s: %v", effect, effects)
		}
	}
	// Both legacy and versioned tool type names map to the web_search family.
	dated := bind(t, compiled, hostedSource(t, `{"model":"route","input":"hello","store":false,"tools":[{"type":"web_search_2025_08_26"}]}`), Context{AllowHostedTools: true})
	if !slices.Contains(dated.Obligations().Effects, "hosted_tool_call") {
		t.Fatalf("versioned tool type missed hosted effect: %v", dated.Obligations().Effects)
	}
}

// The qualified declaration schema is narrow: unknown members and unsupported
// control values refuse precisely before dispatch.
func TestHostedWebSearchToolSchemaIsBounded(t *testing.T) {
	compiled := template(t, configuration(t, "openai-responses"))
	context := Context{AllowHostedTools: true}
	admitted := []string{
		`{"type":"web_search"}`,
		`{"type":"web_search","search_context_size":"low"}`,
		`{"type":"web_search","filters":{"allowed_domains":["pubmed.ncbi.nlm.nih.gov"]}}`,
		`{"type":"web_search","filters":null,"user_location":{"type":"approximate","city":"Berlin","country":"DE","region":null,"timezone":"Europe/Berlin"}}`,
	}
	for _, tool := range admitted {
		t.Run("admitted", func(t *testing.T) {
			body := `{"model":"route","input":"hello","store":false,"tools":[` + tool + `]}`
			bind(t, compiled, hostedSource(t, body), context)
		})
	}
	refused := map[string]string{
		"unknown member":   `{"type":"web_search","external":true}`,
		"bad context size": `{"type":"web_search","search_context_size":"maximal"}`,
		"bad location":     `{"type":"web_search","user_location":{"type":"precise"}}`,
		"bad filters":      `{"type":"web_search","filters":{"blocked_domains":["x"]}}`,
		"bad domains":      `{"type":"web_search","filters":{"allowed_domains":[42]}}`,
	}
	for name, tool := range refused {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"route","input":"hello","store":false,"tools":[` + tool + `]}`
			_, err := compiled.Bind(hostedSource(t, body), context)
			assertReason(t, err, "target_capability")
		})
	}
	// Other hosted kinds keep the qualified refusal.
	for _, kind := range []string{"file_search", "computer_use_preview", "code_interpreter", "image_generation", "mcp", "tool_search"} {
		t.Run("refused/"+kind, func(t *testing.T) {
			body := `{"model":"route","input":"hello","store":false,"tools":[{"type":"` + kind + `"}]}`
			_, err := compiled.Bind(hostedSource(t, body), context)
			assertReason(t, err, "state_carrier")
		})
	}
}

// Selectors that request hosted observations or force hosted tools need the
// matching admission; every other selector fails closed.
func TestHostedSelectorsRequireAdmission(t *testing.T) {
	compiled := template(t, configuration(t, "openai-responses"))
	context := Context{AllowHostedTools: true}
	base := `"model":"route","input":"hello","store":false`

	// web_search include selectors require the declared tool.
	for _, include := range []string{"web_search_call.results", "web_search_call.action.sources"} {
		_, err := compiled.Bind(hostedSource(t, `{`+base+`,"include":["`+include+`"]}`), context)
		assertReason(t, err, "state_carrier")
	}
	// Hosted-family selectors have no admitted lifecycle.
	for _, include := range []string{"file_search_call.results", "code_interpreter_call.outputs", "computer_call_output.output.image_url"} {
		_, err := compiled.Bind(hostedSource(t, `{`+base+`,"tools":[{"type":"web_search"}],"include":["`+include+`"]}`), context)
		assertReason(t, err, "state_carrier")
	}
	// Unknown selectors fail closed.
	_, err := compiled.Bind(hostedSource(t, `{`+base+`,"tools":[{"type":"web_search"}],"include":["future_call.details"]}`), context)
	assertReason(t, err, "target_capability")
	// Qualified selectors and admitted hosted selectors pass.
	for _, include := range []string{"reasoning.encrypted_content", "message.output_text.logprobs", "message.input_image.image_url"} {
		bind(t, compiled, hostedSource(t, `{`+base+`,"include":["`+include+`"]}`), context)
	}
	bind(t, compiled, hostedSource(t, `{`+base+`,"tools":[{"type":"web_search"}],"include":["web_search_call.results","web_search_call.action.sources"]}`), context)

	// Forcing a hosted tool through tool_choice needs the admission.
	for _, choice := range []string{"web_search", "web_search_preview", "web_search_2025_08_26"} {
		_, err := compiled.Bind(hostedSource(t, `{`+base+`,"tool_choice":{"type":"`+choice+`"}}`), context)
		assertReason(t, err, "state_carrier")
	}
	bind(t, compiled, hostedSource(t, `{`+base+`,"tools":[{"type":"web_search"}],"tool_choice":{"type":"web_search_preview"}}`), context)
	// Unqualified hosted selectors and client selectors keep their behavior.
	_, err = compiled.Bind(hostedSource(t, `{`+base+`,"tools":[{"type":"web_search"}],"tool_choice":{"type":"file_search"}}`), context)
	assertReason(t, err, "state_carrier")
	bind(t, compiled, hostedSource(t, `{`+base+`,"tool_choice":{"type":"function","name":"local"}}`), context)
	bind(t, compiled, hostedSource(t, `{`+base+`,"tool_choice":"required"}`), context)
}

// Hosted output items, action variants, statuses and citations are bounded;
// undeclared hosted work is a provider contract violation.
func TestHostedResultContractBoundsOutput(t *testing.T) {
	compiled := template(t, configuration(t, "openai-responses"))
	admitted := bind(t, compiled, hostedSource(t, `{"model":"route","input":"hello","store":false,"tools":[{"type":"web_search"}]}`), Context{AllowHostedTools: true})
	plain := bind(t, compiled, hostedSource(t, `{"model":"route","input":"hello","store":false}`), Context{})

	response := func(output string) []byte {
		return []byte(`{"id":"resp_1","object":"response","status":"completed","model":"route","output":[` + output + `],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}`)
	}
	item := func(action string) string {
		return `{"id":"ws_1","type":"web_search_call","status":"completed","action":` + action + `}`
	}
	admittedOK := []string{
		item(`{"type":"search","queries":["a","b"],"sources":[{"type":"url","url":"https://example.com"}]}`),
		item(`{"type":"search","query":"deprecated scalar"}`),
		item(`{"type":"search","query":["legacy","list"]}`),
		item(`{"type":"open_page","url":"https://example.com"}`),
		item(`{"type":"open_page","url":null}`),
		item(`{"type":"find_in_page","url":"https://example.com","pattern":"needle"}`),
		`{"id":"ws_1","type":"web_search_call","status":"failed"}`,
		`{"id":"ws_1","type":"web_search_call","status":"in_progress"}`,
		`{"id":"ws_1","type":"web_search_call","status":"searching","action":{"type":"search","queries":[]}}`,
		`{"id":"m_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"cited","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":5}]}]}`,
	}
	for _, output := range admittedOK {
		if err := admitted.ValidateUnary(response(output)); err != nil {
			t.Fatalf("admitted output refused: %v / %s", err, output)
		}
	}
	admittedBad := []string{
		`{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","queries":["a"],"extra":1}}`,
		`{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"teleport","url":"x"}}`,
		`{"id":"ws_1","type":"web_search_call","status":"queued"}`,
		`{"type":"web_search_call","status":"completed","action":{"type":"search","queries":["a"]}}`,
		`{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","queries":[4]}}`,
		`{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"find_in_page","url":"https://x"}}`,
		`{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","sources":[{"type":"url"}]}}`,
		`{"id":"m_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"file_citation","file_id":"f","filename":"n","index":0}]}]}`,
		`{"id":"m_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"url_citation","url":"https://x","title":"t","start_index":"0","end_index":1}]}]}`,
	}
	for _, output := range admittedBad {
		if err := admitted.ValidateUnary(response(output)); err == nil {
			t.Fatalf("malformed hosted output delivered: %s", output)
		} else {
			assertReason(t, err, "fidelity_protocol_violation")
		}
	}
	// Hosted observations without admission are contract violations.
	for _, output := range []string{
		item(`{"type":"search","queries":["a"]}`),
		`{"id":"fs_1","type":"file_search_call","status":"completed","queries":[],"results":[]}`,
		`{"id":"ci_1","type":"code_interpreter_call","status":"completed","code":"print(1)","outputs":[]}`,
		`{"id":"m_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"url_citation","url":"https://x","title":"t","start_index":0,"end_index":1}]}]}`,
	} {
		if err := plain.ValidateUnary(response(output)); err == nil {
			t.Fatalf("undeclared hosted output delivered: %s", output)
		} else {
			assertReason(t, err, "fidelity_protocol_violation")
		}
	}
	// Client-function and reasoning output remain admitted native surface.
	for _, output := range []string{
		`{"id":"fc_1","type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`,
		`{"id":"r_1","type":"reasoning","summary":[]}`,
		`{"id":"m_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}`,
	} {
		if err := plain.ValidateUnary(response(output)); err != nil {
			t.Fatalf("native output refused: %v / %s", err, output)
		}
		if err := admitted.ValidateUnary(response(output)); err != nil {
			t.Fatalf("native output under hosted admission refused: %v / %s", err, output)
		}
	}
}

// Stream events for hosted work are admitted only with the declared tool and
// keep their member contract; embedded output items follow the same rule.
func TestHostedEventContract(t *testing.T) {
	compiled := template(t, configuration(t, "openai-responses"))
	body := `{"model":"route","input":"hello","store":false,"stream":true`
	admitted := bind(t, compiled, hostedSource(t, body+`,"tools":[{"type":"web_search"}]}`), Context{AllowHostedTools: true})
	plain := bind(t, compiled, hostedSource(t, body+`}`), Context{})

	for _, name := range []string{"response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed"} {
		event, err := openai.LiftEvent(openai.FamilyResponses, `{"type":"`+name+`","item_id":"ws_1","output_index":0,"sequence_number":3}`, name, 1, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := admitted.ValidateEvent(event); err != nil {
			t.Fatalf("admitted %s refused: %v", name, err)
		}
		assertReason(t, plain.ValidateEvent(event), "fidelity_protocol_violation")
	}
	bad, err := openai.LiftEvent(openai.FamilyResponses, `{"type":"response.web_search_call.completed","item_id":"ws_1","output_index":0,"sequence_number":3,"extra":true}`, "response.web_search_call.completed", 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	assertReason(t, admitted.ValidateEvent(bad), "fidelity_protocol_violation")
	// Other hosted lifecycle families are never admitted.
	for _, name := range []string{"response.file_search_call.in_progress", "response.code_interpreter_call.executing", "response.mcp_call.completed", "response.computer_call.completed"} {
		event, err := openai.LiftEvent(openai.FamilyResponses, `{"type":"`+name+`","item_id":"x_1","output_index":0,"sequence_number":3}`, name, 1, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		assertReason(t, admitted.ValidateEvent(event), "fidelity_protocol_violation")
	}
	// output_item events carrying hosted items follow the item contract.
	for _, name := range []string{"response.output_item.added", "response.output_item.done"} {
		event, err := openai.LiftEvent(openai.FamilyResponses, `{"type":"`+name+`","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}`, name, 1, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := admitted.ValidateEvent(event); err != nil {
			t.Fatalf("admitted output_item event refused: %v", err)
		}
		assertReason(t, plain.ValidateEvent(event), "fidelity_protocol_violation")
	}
	annotation, err := openai.LiftEvent(openai.FamilyResponses, `{"type":"response.output_text.annotation.added","item_id":"m_1","output_index":0,"content_index":0,"annotation_index":0,"annotation":{"type":"url_citation","url":"https://x","title":"t","start_index":0,"end_index":1}}`, "response.output_text.annotation.added", 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := admitted.ValidateEvent(annotation); err != nil {
		t.Fatalf("admitted citation event refused: %v", err)
	}
	assertReason(t, plain.ValidateEvent(annotation), "fidelity_protocol_violation")
	// Terminal events embed the observed output and are bounded the same way.
	terminal, err := openai.LiftEvent(openai.FamilyResponses, `{"type":"response.incomplete","response":{"id":"resp_1","object":"response","status":"incomplete","model":"route","output":[{"id":"ws_1","type":"web_search_call","status":"in_progress"},{"id":"m_1","type":"message","status":"incomplete","role":"assistant","content":[{"type":"output_text","text":"partial","annotations":[{"type":"url_citation","url":"https://x","title":"t","start_index":0,"end_index":3}]}]}],"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}}`, "response.incomplete", 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := admitted.ValidateEvent(terminal); err != nil {
		t.Fatalf("admitted incomplete terminal refused: %v", err)
	}
	assertReason(t, plain.ValidateEvent(terminal), "fidelity_protocol_violation")
	failed, err := openai.LiftEvent(openai.FamilyResponses, `{"type":"response.failed","response":{"id":"resp_1","object":"response","status":"failed","model":"route","output":[{"id":"ws_1","type":"web_search_call","status":"failed","action":{"type":"search","queries":["a"]}}],"error":{"code":"server_error","message":"search backend unavailable"},"usage":{"input_tokens":4,"output_tokens":0,"total_tokens":4}}}`, "response.failed", 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := admitted.ValidateEvent(failed); err != nil {
		t.Fatalf("admitted failed terminal refused: %v", err)
	}
}

// Content policies can still inspect a web_search invocation: request tool
// configuration and replayed hosted observations are declared text data, and
// the output contract covers hosted items and citations.
func TestHostedWebSearchKeepsPolicyCoverage(t *testing.T) {
	config := configuration(t, "openai-responses")
	config.Policy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "in", Phase: "input", Action: "block", Pattern: "forbidden"}, {ID: "out", Phase: "output", Action: "block", Pattern: "forbidden"}}}
	compiled := template(t, config)
	context := Context{AllowHostedTools: true}

	// Request tool configuration and replayed web_search_call input items are
	// inspectable; an input policy sees them instead of refusing coverage.
	plan := bind(t, compiled, hostedSource(t, `{"model":"route","store":false,"tools":[{"type":"web_search","search_context_size":"low"}],"input":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","queries":["prior"]}},{"type":"message","role":"user","content":[{"type":"input_text","text":"follow up"}]}]}`), context)
	if _, err := plan.CheckInput(); err != nil {
		t.Fatalf("input policy lost coverage: %v", err)
	}
	// Output policy accepts the admitted web_search tool declarations.
	result := `{"id":"r","object":"response","status":"completed","model":"route","output":[{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","queries":["q"],"sources":[{"type":"url","url":"https://x"}]}},{"id":"m_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"cited","annotations":[{"type":"url_citation","url":"https://x","title":"t","start_index":0,"end_index":4}]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	if err := plan.ValidateUnary([]byte(result)); err != nil {
		t.Fatalf("output policy coverage refused qualified hosted output: %v", err)
	}
	// Mixed client tools stay uninspectable for output policy.
	_, err := compiled.Bind(hostedSource(t, `{"model":"route","input":"hello","store":false,"tools":[{"type":"function","name":"f","parameters":{"type":"object"}},{"type":"web_search"}]}`), context)
	assertReason(t, err, "policy_conflict")
	// A file_citation annotation remains uninspectable hosted output.
	badCitation := `{"id":"r","object":"response","status":"completed","model":"route","output":[{"id":"m_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"file_citation","file_id":"f","filename":"n","index":0}]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	assertReason(t, plan.ValidateUnary([]byte(badCitation)), "fidelity_protocol_violation")
}
