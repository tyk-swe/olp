package estimate

import (
	"encoding/json"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/tests/fixtures"
)

func mustParse(t testing.TB, family openai.Family, body string) *openai.Request {
	t.Helper()
	request, err := openai.Parse(family, []byte(body))
	if err != nil {
		t.Fatalf("parse %s: %v", body, err)
	}
	return request
}

// native builds a request of a dialect this package does not parse, the way
// the dialect's codec hands one to admission: its fields, unvalidated.
func native(t testing.TB, family openai.Family, body string) *openai.Request {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	return openai.NewEnvelope(family, "route", false, fields)
}

// TestWalkerReproducesTheCookbookConversations walks each conversation the
// cookbook counted as a chat request: the tokens of every field plus the
// framing, which is the number OpenAI's formula gives.
func TestWalkerReproducesTheCookbookConversations(t *testing.T) {
	data, err := fixtures.Files.ReadFile("tokens/framing.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []framingFixture
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, f := range cases {
		t.Run(f.Name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"model": "m", "messages": f.Messages})
			if err != nil {
				t.Fatal(err)
			}
			e := Walk(mustParse(t, openai.FamilyChat, string(body))).Estimate(ForModel(f.Model), nil)
			if e.Input != f.Tokens || e.Provenance != ProvenanceTokenizer {
				t.Fatalf("input %d (%s), the cookbook formula counts %d", e.Input, e.Provenance, f.Tokens)
			}
		})
	}
}

// TestPromptEstimates pins literals: each count below was made by OpenAI's
// tiktoken, and the framing arithmetic is written out. An estimate recomputed
// the way the implementation computes it would assert nothing.
func TestPromptEstimates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		family     openai.Family
		body       string
		model      string
		defaults   string
		input      int64
		total      int64
		provenance Provenance
	}{
		{
			name: "one message", family: openai.FamilyChat, model: "gpt-4o",
			body: `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			// "hello" one token and "user" one, three for the message, three
			// priming the reply.
			input: 1 + 1 + 3 + 3, total: 8 + 10, provenance: ProvenanceTokenizer,
		},
		{
			name: "the same request on a heuristic family", family: openai.FamilyChat, model: "claude-sonnet-4-5",
			body: `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			// Five characters are two tokens, and nothing frames them.
			input: 2, total: 12, provenance: ProvenanceHeuristic,
		},
		{
			name: "a script the heuristic overcharges", family: openai.FamilyChat, model: "gpt-4o",
			body:  `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"日本語で"}]}`,
			input: 3 + 1 + 3 + 3, total: 10 + 1, provenance: ProvenanceTokenizer,
		},
		{
			name: "the older encoding counts the same text differently", family: openai.FamilyChat, model: "gpt-4",
			body:  `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"日本語で"}]}`,
			input: 5 + 1 + 3 + 3, total: 12 + 1, provenance: ProvenanceTokenizer,
		},
		{
			name: "a tool catalogue with its schema", family: openai.FamilyChat, model: "gpt-4o",
			body: `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"What is 2+2?"}],` +
				`"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`,
			// The question is seven, the name two, the description three and
			// the compact schema fourteen; then "user", the message and the
			// reply. These are the tokens of the text. The model reads the
			// catalogue in a rendering of its own, which no provider documents and
			// the count does not include, so the request's count is calibrated.
			input: 7 + 2 + 3 + 14 + 1 + 3 + 3, total: 33 + 1, provenance: ProvenanceCalibrated,
		},
		{
			name: "an image costs its flat charge and makes the count calibrated", family: openai.FamilyChat, model: "gpt-4o",
			body:  `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
			input: 1 + ImageTokens + 1 + 3 + 3, total: 8 + ImageTokens + 1, provenance: ProvenanceCalibrated,
		},
		{
			name: "an image on a heuristic family stays a heuristic", family: openai.FamilyChat, model: "claude-sonnet-4-5",
			body:  `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`,
			input: 2 + MediaTokens, total: 2 + MediaTokens + 1, provenance: ProvenanceHeuristic,
		},
		{
			name: "responses input is one user message", family: openai.FamilyResponses, model: "gpt-4.1",
			body:  `{"model":"m","max_output_tokens":10,"input":"What is 2+2?"}`,
			input: 7 + 1 + 3 + 3, total: 14 + 10, provenance: ProvenanceTokenizer,
		},
		{
			name: "responses instructions are a system message", family: openai.FamilyResponses, model: "gpt-4.1",
			body:  `{"model":"m","max_output_tokens":10,"input":"What is 2+2?","instructions":"Be concise and kind."}`,
			input: 7 + 1 + 3 + 5 + 1 + 3 + 3, total: 23 + 10, provenance: ProvenanceTokenizer,
		},
		{
			name: "instructions a provider supplies", family: openai.FamilyResponses, model: "gpt-4.1",
			body: `{"model":"m","input":"What is 2+2?"}`, defaults: `{"instructions":"Be concise and kind.","max_output_tokens":10}`,
			// The reply is primed once however many inputs make a message.
			input: 7 + 1 + 3 + 5 + 1 + 3 + 3, total: 23 + 10, provenance: ProvenanceTokenizer,
		},
		{
			name: "an Anthropic request sent to an OpenAI model", family: openai.FamilyAnthropic, model: "gpt-4o",
			body: `{"model":"m","max_tokens":10,"system":"Be brief.","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			// "Be brief." three and "hi" one; "system" and "user" one each, two
			// messages of three, and the reply.
			input: 3 + 1 + 1 + 1 + 6 + 3, total: 15 + 10, provenance: ProvenanceTokenizer,
		},
		{
			name: "an Anthropic request sent to Claude", family: openai.FamilyAnthropic, model: "claude-sonnet-4-5",
			body:  `{"model":"m","max_tokens":10,"system":"Be brief.","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			input: 3 + 1, total: 4 + 10, provenance: ProvenanceHeuristic,
		},
		{
			name: "embedding text", family: openai.FamilyEmbeddings, model: "text-embedding-3-small",
			body:  `{"model":"m","input":["hello","日本語で"]}`,
			input: 1 + 5, total: 6, provenance: ProvenanceTokenizer,
		},
		{
			name: "embedding token ids are one token each", family: openai.FamilyEmbeddings, model: "text-embedding-3-small",
			body:  `{"model":"m","input":[[1,2,3],[4]]}`,
			input: 4, total: 4, provenance: ProvenanceTokenizer,
		},
		{
			name: "an empty embedding reserves one", family: openai.FamilyEmbeddings, model: "text-embedding-3-small",
			body:  `{"model":"m","input":""}`,
			input: 1, total: 1, provenance: ProvenanceTokenizer,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request *openai.Request
			switch tc.family {
			case openai.FamilyAnthropic:
				request = native(t, tc.family, tc.body)
			default:
				request = mustParse(t, tc.family, tc.body)
			}
			var defaults map[string]json.RawMessage
			if tc.defaults != "" {
				if err := json.Unmarshal([]byte(tc.defaults), &defaults); err != nil {
					t.Fatal(err)
				}
			}
			e := Walk(request).Estimate(ForModel(tc.model), defaults)
			if e.Input != tc.input || e.Tokens() != tc.total || e.Provenance != tc.provenance {
				t.Fatalf("input %d, reserved %d, %s; want %d, %d, %s", e.Input, e.Tokens(), e.Provenance, tc.input, tc.total, tc.provenance)
			}
		})
	}
}

// TestOnlyAnExactReadingOfTheRequestIsATokenizerCount holds the provenance of a
// request on a tokenizer family to what the count can know. A count is
// tokenizer when every part of the prompt is text, which the encoder counts, or
// message framing, which OpenAI documents. A part the model reads in a form the
// count does not have, which is an image or media part at its flat charge, a
// tool schema and a tool call, makes it calibrated, in every dialect.
func TestOnlyAnExactReadingOfTheRequestIsATokenizerCount(t *testing.T) {
	const image = `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}`
	for _, tc := range []struct {
		name   string
		family openai.Family
		body   string
		want   Provenance
	}{
		{"chat text", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi","name":"bot"}]}`, ProvenanceTokenizer},
		{"responses text", openai.FamilyResponses, `{"model":"m","input":"hello","instructions":"Be brief."}`, ProvenanceTokenizer},
		{"anthropic text", openai.FamilyAnthropic, `{"system":"Be brief.","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`, ProvenanceTokenizer},
		{"gemini text", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, ProvenanceTokenizer},

		{"chat image", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":[` + image + `]}]}`, ProvenanceCalibrated},
		{"chat audio", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`, ProvenanceCalibrated},
		{"chat file", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"f"}}]}]}`, ProvenanceCalibrated},
		{"responses image", openai.FamilyResponses, `{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`, ProvenanceCalibrated},
		{"anthropic image", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}`, ProvenanceCalibrated},
		{"gemini inline data", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}]}`, ProvenanceCalibrated},
		{"gemini file data", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"fileData":{"fileUri":"gs://b/o"}}]}]}`, ProvenanceCalibrated},

		{"chat tool catalogue", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`, ProvenanceCalibrated},
		{"responses tool catalogue", openai.FamilyResponses, `{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`, ProvenanceCalibrated},
		{"anthropic tool catalogue", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`, ProvenanceCalibrated},
		{"chat tool call", openai.FamilyChat, `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`, ProvenanceCalibrated},
		{"chat tool result", openai.FamilyChat, `{"model":"m","messages":[{"role":"tool","tool_call_id":"c","content":"22C"}]}`, ProvenanceCalibrated},
		{"responses tool call", openai.FamilyResponses, `{"model":"m","input":[{"type":"function_call","name":"f","call_id":"c","arguments":"{}"}]}`, ProvenanceCalibrated},
		{"anthropic tool result", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"22C"}]}]}`, ProvenanceCalibrated},
		{"anthropic tool use", openai.FamilyAnthropic, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"f","input":{"city":"Paris"}}]}]}`, ProvenanceCalibrated},
		{"gemini function call", openai.FamilyGemini, `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{"city":"Paris"}}}]}]}`, ProvenanceCalibrated},
		{"gemini function response", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"output":"22C"}}}]}]}`, ProvenanceCalibrated},

		// What a model reads that is neither plain text nor a part with a flat
		// charge: a schema its reply must follow, its own reasoning, a document.
		{"chat structured output", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"o","schema":{"type":"object"}}}}`, ProvenanceCalibrated},
		{"chat json mode", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`, ProvenanceTokenizer},
		{"responses structured output", openai.FamilyResponses, `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"o","schema":{"type":"object"}}}}`, ProvenanceCalibrated},
		{"responses plain text format", openai.FamilyResponses, `{"model":"m","input":"hi","text":{"format":{"type":"text"}}}`, ProvenanceTokenizer},
		{"responses reasoning item", openai.FamilyResponses, `{"model":"m","input":[{"type":"reasoning","id":"r","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"AAAA"}]}`, ProvenanceCalibrated},
		{"responses reasoning item without a summary", openai.FamilyResponses, `{"model":"m","input":[{"type":"reasoning","id":"r","summary":[],"encrypted_content":"AAAA"}]}`, ProvenanceCalibrated},
		{"anthropic structured output", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`, ProvenanceCalibrated},
		{"anthropic output format", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":"hi"}],"output_format":{"type":"json_schema","schema":{"type":"object"}}}`, ProvenanceCalibrated},
		{"anthropic effort", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`, ProvenanceTokenizer},
		{"anthropic thinking", openai.FamilyAnthropic, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"s"}]}]}`, ProvenanceCalibrated},
		{"anthropic redacted thinking", openai.FamilyAnthropic, `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"AAAA"}]}]}`, ProvenanceCalibrated},
		{"anthropic pdf document", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AAAA"}}]}]}`, ProvenanceCalibrated},
		{"anthropic text document", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"the text"}}]}]}`, ProvenanceCalibrated},
		{"anthropic document of text blocks", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"the text"}]}}]}]}`, ProvenanceCalibrated},
		{"gemini response schema", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"OBJECT"}}}`, ProvenanceCalibrated},
		{"gemini response json schema", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json","responseJsonSchema":{"type":"object"}}}`, ProvenanceCalibrated},
		{"gemini json mode", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json","maxOutputTokens":9}}`, ProvenanceTokenizer},
		{"gemini count response schema", openai.FamilyGeminiCount, `{"generateContentRequest":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseSchema":{"type":"OBJECT"}}}}`, ProvenanceCalibrated},
		{"gemini count tools", openai.FamilyGeminiCount, `{"generateContentRequest":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f"}]}]}}`, ProvenanceCalibrated},
		{"bedrock tool catalogue", openai.FamilyBedrock, `{"messages":[{"role":"user","content":[{"text":"hi"}]}],"toolConfig":{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":{"type":"object"}}}}]}}`, ProvenanceCalibrated},
		{"bedrock structured output", openai.FamilyBedrock, `{"messages":[{"role":"user","content":[{"text":"hi"}]}],"outputConfig":{"textFormat":{"type":"json_schema","structure":{"jsonSchema":{"name":"o","schema":"{\"type\":\"object\"}"}}}}}`, ProvenanceCalibrated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request *openai.Request
			if tc.family == openai.FamilyChat || tc.family == openai.FamilyResponses {
				request = mustParse(t, tc.family, tc.body)
			} else {
				request = native(t, tc.family, tc.body)
			}
			prompt := Walk(request)
			for _, model := range []string{"gpt-4o", "gpt-4"} {
				if e := prompt.Estimate(ForModel(model), nil); e.Provenance != tc.want || e.Input < 1 {
					t.Errorf("%s: %d tokens, %s; want %s", model, e.Input, e.Provenance, tc.want)
				}
			}
			// A family without a tokenizer says heuristic whatever the prompt holds.
			if e := prompt.Estimate(ForModel("claude-sonnet-4-5"), nil); e.Provenance != ProvenanceHeuristic {
				t.Errorf("claude: %s, want heuristic", e.Provenance)
			}
		})
	}
}

// TestDefaultsThatAreApproximateWeakenTheCount covers a provider whose defaults
// supply prompt content the caller left out: the estimate is as trustworthy as
// the weaker of the request and what the provider added.
func TestDefaultsThatAreApproximateWeakenTheCount(t *testing.T) {
	c := ForModel("gpt-4o")
	for _, tc := range []struct {
		name     string
		request  *openai.Request
		defaults string
		want     Provenance
	}{
		{"instructions", mustParse(t, openai.FamilyResponses, `{"model":"m","input":"hello"}`), `{"instructions":"Be brief."}`, ProvenanceTokenizer},
		{"a system prompt", native(t, openai.FamilyAnthropic, `{"messages":[{"role":"user","content":"hello"}]}`), `{"system":"Be brief."}`, ProvenanceTokenizer},
		{"a system prompt with an image", native(t, openai.FamilyAnthropic, `{"messages":[{"role":"user","content":"hello"}]}`), `{"system":[{"type":"image","source":{"type":"url","url":"x"}}]}`, ProvenanceCalibrated},
		{"tools", mustParse(t, openai.FamilyResponses, `{"model":"m","input":"hello"}`), `{"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`, ProvenanceCalibrated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var defaults map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.defaults), &defaults); err != nil {
				t.Fatal(err)
			}
			prompt := Walk(tc.request)
			plain := prompt.Estimate(c, nil)
			if plain.Provenance != ProvenanceTokenizer {
				t.Fatalf("without defaults: %s, want tokenizer", plain.Provenance)
			}
			with := prompt.Estimate(c, defaults)
			if with.Provenance != tc.want || with.Input <= plain.Input {
				t.Errorf("with defaults: %d tokens against %d, %s; want more, %s", with.Input, plain.Input, with.Provenance, tc.want)
			}
		})
	}
}

func TestWeakerIsTheLessTrustworthyProvenance(t *testing.T) {
	order := []Provenance{ProvenanceTokenizer, ProvenanceCalibrated, ProvenanceHeuristic}
	for i, a := range order {
		for j, b := range order {
			if got, want := Weaker(a, b), order[max(i, j)]; got != want {
				t.Errorf("Weaker(%s, %s) = %s, want %s", a, b, got, want)
			}
		}
	}
	// A provenance this package does not know is the least trustworthy.
	if got := Weaker(ProvenanceTokenizer, "other"); got != "other" {
		t.Errorf("Weaker(tokenizer, other) = %s", got)
	}
}

// TestEachFamilyIsCountedOnce is what lets admission price a request for
// every attempt it may make: counting happens once for each family, and the
// rest read it back.
func TestEachFamilyIsCountedOnce(t *testing.T) {
	var mu sync.Mutex
	counted := map[Family]int{}
	prompt := Walk(mustParse(t, openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hello"}]}`)).
		Observe(func(f Family) { mu.Lock(); counted[f]++; mu.Unlock() })
	models := []string{"gpt-4o", "gpt-4o-mini", "gpt-4.1", "o3", "gpt-4", "gpt-3.5-turbo", "claude-sonnet-4-5", "us.anthropic.claude-haiku", "gemini-2.5-pro", "mistral-large", "my-deployment", "gpt-4o"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for _, model := range models {
				prompt.Estimate(ForModel(model), nil)
				prompt.Estimate(ForModel(model), map[string]json.RawMessage{"max_tokens": json.RawMessage(`5`)})
			}
		})
	}
	wg.Wait()
	want := map[Family]int{FamilyOpenAIO200k: 1, FamilyOpenAICL100k: 1, FamilyAnthropic: 1, FamilyGemini: 1, FamilyOther: 1}
	if len(counted) != len(want) {
		t.Fatalf("counted %v, want %v", counted, want)
	}
	for family, n := range want {
		if counted[family] != n {
			t.Errorf("%s counted %d times, want %d", family, counted[family], n)
		}
	}
}

func TestDefaultsAreNotCountedAgainstThePrompt(t *testing.T) {
	counted := 0
	prompt := Walk(mustParse(t, openai.FamilyResponses, `{"model":"m","input":"hello"}`)).Observe(func(Family) { counted++ })
	c := ForModel("gpt-4o")
	plain := prompt.Estimate(c, nil)
	withDefaults := prompt.Estimate(c, map[string]json.RawMessage{"instructions": json.RawMessage(`"Be brief."`)})
	if counted != 1 {
		t.Fatalf("the prompt was counted %d times", counted)
	}
	if withDefaults.Input <= plain.Input {
		t.Fatalf("instructions from defaults added nothing: %d against %d", withDefaults.Input, plain.Input)
	}
	// The caller's own instructions, even a null one, win over the default.
	for _, body := range []string{`{"model":"m","input":"hello","instructions":null}`, `{"model":"m","input":"hello","instructions":"x"}`} {
		p := Walk(mustParse(t, openai.FamilyResponses, body))
		before := p.Estimate(c, nil)
		after := p.Estimate(c, map[string]json.RawMessage{"instructions": json.RawMessage(`"Be brief and nothing else."`)})
		if before.Input != after.Input {
			t.Errorf("%s: a default replaced the caller's instructions: %d, then %d", body, before.Input, after.Input)
		}
	}
}

// TestFollowSharesTheCountOfTheSameText covers the request a provider receives,
// which is usually what the caller sent in another dialect.
func TestFollowSharesTheCountOfTheSameText(t *testing.T) {
	counted := 0
	source := Walk(mustParse(t, openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hello there"}]}`)).Observe(func(Family) { counted++ })
	c := ForModel("gpt-4o")
	want := source.Estimate(c, nil).Input

	// The responses spelling of the same conversation, and the same text in
	// another shape.
	same := Walk(mustParse(t, openai.FamilyResponses, `{"model":"m","input":"hello there"}`))
	same.Follow(source)
	if got := same.Estimate(c, nil).Input; got != want || counted != 1 {
		t.Fatalf("an equal prompt counted %d after %d counts, want %d after one", got, counted, want)
	}

	different := Walk(mustParse(t, openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hello there!"}]}`))
	different.Follow(source)
	if got := different.Estimate(c, nil).Input; got == want {
		t.Fatalf("a different prompt took the count of another: %d", got)
	}
	for _, other := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hello there","name":"n"}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":"hello there"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hello there"},{"role":"user","content":""}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hello there"},{"type":"image_url","image_url":{"url":"x"}}]}]}`,
	} {
		p := Walk(mustParse(t, openai.FamilyChat, other))
		p.Follow(source)
		if p.tally == source.tally {
			t.Errorf("%s shares the count of a prompt that reads differently", other)
		}
	}
}

// TestFollowIgnoresTheOrderOfText covers the request a provider is sent when it
// reads the caller's with its parts in another order, as a chat request puts the
// system prompt where the Anthropic and Responses dialects put it after the
// messages. Text counted exactly is a sum over its segments, so the count is
// shared; text that is extrapolated depends on what came first, so it is not.
func TestFollowIgnoresTheOrderOfText(t *testing.T) {
	c := ForModel("gpt-4o")
	chat := `{"model":"m","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hello there"}]}`
	want := Walk(mustParse(t, openai.FamilyChat, chat)).Estimate(c, nil)
	for name, source := range map[string]*openai.Request{
		"anthropic": native(t, openai.FamilyAnthropic, `{"system":"Be brief.","messages":[{"role":"user","content":"hello there"}]}`),
		"responses": mustParse(t, openai.FamilyResponses, `{"model":"m","input":"hello there","instructions":"Be brief."}`),
	} {
		counted := 0
		from := Walk(source).Observe(func(Family) { counted++ })
		from.Estimate(c, nil)
		effective := Walk(mustParse(t, openai.FamilyChat, chat))
		effective.Follow(from)
		got := effective.Estimate(c, nil)
		if counted != 1 || effective.tally != from.tally {
			t.Errorf("%s: the family was counted %d times for the request and its translation, want once", name, counted)
		}
		if got.Input != want.Input || got.Provenance != want.Provenance {
			t.Errorf("%s: shared count %d (%s), the translation alone counts %d (%s)", name, got.Input, got.Provenance, want.Input, want.Provenance)
		}
	}

	// Other text, or the same text under other roles, is another prompt.
	source := Walk(native(t, openai.FamilyAnthropic, `{"system":"Be brief.","messages":[{"role":"user","content":"hello there"}]}`))
	for _, other := range []string{
		`{"model":"m","messages":[{"role":"system","content":"Be brief!"},{"role":"user","content":"hello there"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"Be brief."},{"role":"user","content":"hello there"}]}`,
		`{"model":"m","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hello there"},{"role":"user","content":"hello there"}]}`,
	} {
		p := Walk(mustParse(t, openai.FamilyChat, other))
		p.Follow(source)
		if p.tally == source.tally {
			t.Errorf("%s shares the count of a prompt that reads differently", other)
		}
	}

	// Past what is counted exactly, a count is extrapolated from the start, and
	// the same segments in another order are another count.
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 2*ExactBytes/45)
	for _, tc := range []struct {
		name         string
		first, again []string
		shared       bool
	}{
		{"in the same order", []string{long, "short"}, []string{long, "short"}, true},
		{"in another order", []string{long, "short"}, []string{"short", long}, false},
		{"within the bound, in another order", []string{"short", "another short one"}, []string{"another short one", "short"}, true},
	} {
		walked := func(texts []string) *Prompt {
			var messages []any
			for _, text := range texts {
				messages = append(messages, map[string]any{"role": "user", "content": text})
			}
			body, _ := json.Marshal(map[string]any{"model": "m", "messages": messages})
			return Walk(mustParse(t, openai.FamilyChat, string(body)))
		}
		first, again := walked(tc.first), walked(tc.again)
		again.Follow(first)
		if shared := again.tally == first.tally; shared != tc.shared {
			t.Errorf("%s: shared %v, want %v", tc.name, shared, tc.shared)
		}
	}
}

// TestFollowCountsRolesAgainstTheBound keeps the order-free comparison to the
// text a counter counts whole, and a counter reads a message's role as text too.
// The two prompts below hold the same two messages in either order, over text
// that leaves the exact budget seven bytes, which "user" fits in and "system"
// does not: the prompt that ends on "user" counts the role the other leaves to
// the tail, and the two counts differ by a token.
func TestFollowCountsRolesAgainstTheBound(t *testing.T) {
	x := strings.Repeat("ab ", 5000)
	rest := ExactBytes - 7 - len(x)
	y := strings.Repeat("cd ", rest/3) + strings.Repeat("e", rest%3)
	user := map[string]any{"role": "user", "content": x}
	system := map[string]any{"role": "system", "content": y}
	walked := func(messages ...map[string]any) *Prompt {
		body, _ := json.Marshal(map[string]any{"model": "m", "messages": messages})
		return Walk(mustParse(t, openai.FamilyChat, string(body)))
	}
	first, second := walked(user, system), walked(system, user)
	if first.in.bytes+roleBytes(first.in.roles) <= ExactBytes || first.in.bytes > ExactBytes {
		t.Fatalf("%d bytes of text and %d of roles; the case needs the text inside the bound and the roles past it", first.in.bytes, roleBytes(first.in.roles))
	}
	c := ForModel("gpt-4o")
	if a, b := walked(user, system).Input(c), walked(system, user).Input(c); a == b {
		t.Fatalf("both orders count %+v; the case proves nothing", a)
	}
	second.Follow(first)
	if second.tally == first.tally {
		t.Error("two prompts that count differently share one count")
	}
}

// walkKeeping walks a request into an input that keeps at most retain bytes of
// its text.
func walkKeeping(request *openai.Request, retain int) *input {
	in := &input{retain: retain}
	walkInput(in, request.Family, func(name string) json.RawMessage {
		if raw := request.Field(name); len(raw) > 0 {
			return raw
		}
		return nil
	})
	return in
}

// TestAPromptKeepsOnlyWhatACounterReads holds a prompt to the memory it is for.
// A request is held while its upstream answers, and a stream may take an hour,
// so a prompt that kept all the text of a megabyte request would double what the
// request already weighs. It keeps what a Meter can read, and no count may differ
// from the one a prompt that kept everything makes, for any family, wherever the
// cut falls in the text.
func TestAPromptKeepsOnlyWhatACounterReads(t *testing.T) {
	prose := corpus(t, "document-52k")
	code := corpus(t, "code-go", "code-python", "code-javascript")
	cjk := corpus(t, "multilingual-cjk-long", "multilingual-japanese")
	long := func(unit string, n int) string {
		return prefixOf(strings.Repeat(unit+"\n", n/(len(unit)+1)+1), n)
	}
	message := func(role, text string) map[string]any { return map[string]any{"role": role, "content": text} }
	scenarios := map[string][]any{
		"one long message":                       {message("user", long(prose, 2<<20))},
		"a document after a short system prompt": {message("system", "You are a careful assistant."), message("user", long(code, 700<<10)), message("assistant", "Understood."), message("user", "Summarize it.")},
		"a long script":                          {message("user", long(cjk, 400<<10))},
		"after a line break, spaces":             {message("user", prefixOf(prose, ExactBytes-40)+"\n"+strings.Repeat(" ", 1<<20)+"end")},
		"an unbroken run":                        {message("user", prefixOf(prose, ExactBytes-40)+strings.Repeat("z", 1<<20))},
	}
	var many []any
	for range 3000 {
		many = append(many, message("user", prefixOf(prose[7:], 1<<10)))
	}
	scenarios["three thousand messages"] = many
	// A segment may begin anywhere relative to the point where the prompt stops
	// keeping text, and a cut may fall inside a character.
	for _, lead := range []int{ExactBytes - 5, ExactBytes - 2, ExactBytes - 1, ExactBytes, ExactBytes + 1, ExactBytes + scanWindow, retainBytes - 3, retainBytes - 1, retainBytes, retainBytes + 1} {
		scenarios["a second message "+itoa(lead)+" bytes in"] = []any{message("user", prefixOf(prose, lead)), message("user", long(cjk, 200<<10)), message("user", "and last")}
	}
	for name, messages := range scenarios {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"model": "m", "messages": messages})
			if err != nil {
				t.Fatal(err)
			}
			request := mustParse(t, openai.FamilyChat, string(body))
			kept, whole := walkKeeping(request, 0), walkKeeping(request, 1<<40)

			var held int
			for _, s := range kept.text {
				held += len(s.text)
			}
			// A cut that falls inside a character backs up over it, so what is held may
			// be a few bytes under what is accounted for, and never over.
			if held > kept.kept || kept.kept > retainBytes {
				t.Fatalf("the prompt holds %d bytes of text and accounts for %d, of the %d it may keep", held, kept.kept, retainBytes)
			}
			if len(kept.text) > len(messages)+1 {
				t.Errorf("%d segments for %d messages", len(kept.text), len(messages))
			}
			if whole.kept != int(whole.bytes) {
				t.Fatalf("the unbounded walk kept %d of %d bytes", whole.kept, whole.bytes)
			}
			if kept.bytes != whole.bytes || kept.heuristic != whole.heuristic || kept.messages != whole.messages || len(kept.roles) != len(whole.roles) {
				t.Fatalf("what the walk measured depends on what it kept: %+v against %+v", kept.bytes, whole.bytes)
			}
			var total int64
			for _, s := range kept.text {
				total += int64(len(s.text)) + s.rest
			}
			if total != whole.bytes {
				t.Errorf("the segments account for %d bytes of %d", total, whole.bytes)
			}
			for _, model := range []string{"gpt-4o", "gpt-4", "claude-sonnet-4-5", "mistral-large"} {
				c := ForModel(model)
				if got, want := c.count(kept, false), c.count(whole, false); got != want {
					t.Errorf("%s: counted %+v, a prompt that kept everything counts %+v", model, got, want)
				}
			}
		})
	}
}

// TestAPromptDoesNotHoldTheTextOfItsRequest measures what the others assert
// from the structure: a prompt walked from a four-megabyte request keeps a few
// tens of kilobytes alive, not a copy of the text, which a prefix sliced out of
// the decoded string would be.
func TestAPromptDoesNotHoldTheTextOfItsRequest(t *testing.T) {
	text := corpus(t, "document-52k")
	text = prefixOf(strings.Repeat(text+"\n", 4<<20/(len(text)+1)+1), 4<<20)
	body, err := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": text}}})
	if err != nil {
		t.Fatal(err)
	}
	const requests = 4
	var parsed [requests]*openai.Request
	for i := range parsed {
		parsed[i] = mustParse(t, openai.FamilyChat, string(body))
	}
	heap := func() uint64 {
		var stats runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}
	before := heap()
	var prompts [requests]*Prompt
	for i := range prompts {
		prompts[i] = Walk(parsed[i])
	}
	after := heap()
	runtime.KeepAlive(prompts)
	runtime.KeepAlive(parsed)
	// The text of one request is about four megabytes, so four held whole would
	// be sixteen. What the prompts keep is 36 KiB of text each, and a few
	// kilobytes of segments.
	if after > before && after-before > requests*(128<<10) {
		t.Fatalf("%d prompts of %d-byte requests hold %d bytes", requests, len(body), after-before)
	}
}

func TestALongPromptIsCalibrated(t *testing.T) {
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 4*ExactBytes/45)
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": text}}})
	prompt := Walk(mustParse(t, openai.FamilyChat, string(body)))
	exact := ForModel("gpt-4o")
	e := prompt.Estimate(exact, nil)
	if e.Provenance != ProvenanceCalibrated {
		t.Fatalf("provenance %s for %d bytes, want calibrated", e.Provenance, len(text))
	}
	whole, err := O200kBase.Count(text)
	if err != nil {
		t.Fatal(err)
	}
	// The tail is charged at the ratio of the exact part, and framing is the
	// same whichever way the text was counted.
	if want := int64(whole) + 1 + 3 + 3; e.Input < want || e.Input > want+want/50 {
		t.Fatalf("estimate %d for %d exact tokens and framing, want within two percent above %d", e.Input, whole, want)
	}
	// A family without a tokenizer is never exact, whatever the length.
	if h := prompt.Estimate(ForModel("claude-sonnet-4-5"), nil); h.Provenance != ProvenanceHeuristic {
		t.Fatalf("provenance %s", h.Provenance)
	}
}

func TestWalkerReadsEveryDialectPrompt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family openai.Family
		body   string
		want   int64 // heuristic tokens
	}{
		{"gemini contents and system instruction", openai.FamilyGemini,
			`{"contents":[{"role":"user","parts":[{"text":"abcd"},{"inlineData":{"mimeType":"image/png","data":"AAAA"}}]}],"systemInstruction":{"parts":[{"text":"abcdefgh"}]}}`,
			1 + ImageTokens + 2},
		{"gemini count wraps a request", openai.FamilyGeminiCount,
			`{"generateContentRequest":{"contents":[{"role":"user","parts":[{"text":"abcd"}]}]}}`, 1},
		{"anthropic tools are one schema", openai.FamilyAnthropic,
			`{"messages":[],"tools":[{"name":"a","input_schema":{"type":"object"}}]}`,
			// The array compacts to 47 characters.
			12},
		{"bedrock converse", openai.FamilyBedrock,
			`{"messages":[{"role":"user","content":[{"text":"abcd"}]}],"system":[{"text":"abcd"}]}`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Walk(native(t, tc.family, tc.body)).Estimate(Counter{}, nil)
			if e.Input != max(tc.want, 1) {
				t.Fatalf("input %d, want %d", e.Input, tc.want)
			}
		})
	}
}

// TestWalkerReadsTheStructuredOutputsReasoningAndDocumentsOfEveryDialect holds
// what the walker charges for the members of a prompt a model reads that are not
// messages: the schema a reply must follow, a model's own reasoning, and a
// document. A request that carried one used to be charged for its messages alone,
// as if the member were not there.
func TestWalkerReadsTheStructuredOutputsReasoningAndDocumentsOfEveryDialect(t *testing.T) {
	const schema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`
	schemaTokens := HeuristicTokens(schema)
	for _, tc := range []struct {
		name   string
		family openai.Family
		body   string
		want   int64 // heuristic tokens
	}{
		{"chat response format", openai.FamilyChat,
			`{"model":"m","messages":[{"role":"user","content":"abcd"}],"response_format":{"type":"json_schema","json_schema":{"name":"o","schema":` + schema + `}}}`,
			1 + schemaTokens},
		{"chat json mode", openai.FamilyChat,
			`{"model":"m","messages":[{"role":"user","content":"abcd"}],"response_format":{"type":"json_object"}}`, 1},
		{"responses text format", openai.FamilyResponses,
			`{"model":"m","input":"abcd","text":{"format":{"type":"json_schema","name":"o","schema":` + schema + `}}}`,
			1 + schemaTokens},
		{"responses reasoning summary", openai.FamilyResponses,
			`{"model":"m","input":[{"type":"reasoning","id":"r","summary":[{"type":"summary_text","text":"abcdefgh"},{"type":"summary_text","text":"abcd"}],"encrypted_content":"` + strings.Repeat("A", 4000) + `"}]}`,
			2 + 1},
		{"responses reasoning text", openai.FamilyResponses,
			`{"model":"m","input":[{"type":"reasoning","id":"r","summary":[],"content":[{"type":"reasoning_text","text":"abcdefgh"}]}]}`, 2},
		{"anthropic output format", openai.FamilyAnthropic,
			`{"messages":[{"role":"user","content":"abcd"}],"output_config":{"effort":"high","format":{"type":"json_schema","schema":` + schema + `}}}`,
			1 + schemaTokens},
		{"anthropic legacy output format", openai.FamilyAnthropic,
			`{"messages":[{"role":"user","content":"abcd"}],"output_format":{"type":"json_schema","schema":` + schema + `}}`,
			1 + schemaTokens},
		{"anthropic thinking", openai.FamilyAnthropic,
			`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"abcdefgh","signature":"` + strings.Repeat("s", 400) + `"},{"type":"redacted_thinking","data":"` + strings.Repeat("A", 400) + `"}]}]}`, 2},
		{"anthropic pdf document", openai.FamilyAnthropic,
			`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + strings.Repeat("A", 20_000) + `"}}]}]}`, MediaTokens},
		{"anthropic document by url", openai.FamilyAnthropic,
			`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://example.com/a.pdf"}}]}]}`, MediaTokens},
		{"anthropic text document", openai.FamilyAnthropic,
			`{"messages":[{"role":"user","content":[{"type":"document","title":"t","source":{"type":"text","media_type":"text/plain","data":"abcdefgh"}}]}]}`, 2},
		{"anthropic document of content blocks", openai.FamilyAnthropic,
			`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"abcd"},{"type":"image","source":{"type":"base64","data":"AAAA"}}]}}]}]}`,
			1 + ImageTokens},
		{"gemini response schema", openai.FamilyGemini,
			`{"contents":[{"role":"user","parts":[{"text":"abcd"}]}],"generationConfig":{"maxOutputTokens":9,"responseSchema":` + schema + `}}`, 1 + schemaTokens},
		{"gemini response json schema", openai.FamilyGemini,
			`{"contents":[{"role":"user","parts":[{"text":"abcd"}]}],"generationConfig":{"responseJsonSchema":` + schema + `}}`, 1 + schemaTokens},
		{"gemini count of a request with a schema and tools", openai.FamilyGeminiCount,
			`{"generateContentRequest":{"contents":[{"role":"user","parts":[{"text":"abcd"}]}],"generationConfig":{"responseSchema":` + schema + `},"tools":[{"functionDeclarations":[{"name":"f","parameters":` + schema + `}]}]}}`,
			1 + schemaTokens + HeuristicTokens(`[{"functionDeclarations":[{"name":"f","parameters":`+schema+`}]}]`)},
		{"bedrock tool catalogue", openai.FamilyBedrock,
			`{"messages":[{"role":"user","content":[{"text":"abcd"}]}],"toolConfig":{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":` + schema + `}}}]}}`,
			1 + HeuristicTokens(`{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":`+schema+`}}}]}`)},
		{"bedrock structured output", openai.FamilyBedrock,
			`{"messages":[{"role":"user","content":[{"text":"abcd"}]}],"outputConfig":{"textFormat":{"type":"json_schema","structure":{"jsonSchema":{"name":"o","schema":` + strconv.Quote(schema) + `}}}}}`,
			1 + schemaTokens},
		// Gemini's toolConfig only chooses among the tools the request holds.
		{"gemini tool config", openai.FamilyGemini,
			`{"contents":[{"role":"user","parts":[{"text":"abcd"}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["get_weather"]}}}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request *openai.Request
			if tc.family == openai.FamilyChat || tc.family == openai.FamilyResponses {
				request = mustParse(t, tc.family, tc.body)
			} else {
				request = native(t, tc.family, tc.body)
			}
			if e := Walk(request).Estimate(Counter{}, nil); e.Input != max(tc.want, 1) {
				t.Fatalf("input %d, want %d", e.Input, tc.want)
			}
		})
	}
}

// A member a request does not carry costs a walk nothing, whatever the dialect
// looks for: nearly every request has no structured output, no reasoning and no
// document, and a feature a request does not use allocates nothing for it.
func TestWalkingMembersThatAreNotThereAllocatesNothing(t *testing.T) {
	absent := func(string) json.RawMessage { return nil }
	for _, family := range walkerFamilies {
		var in input
		w := walker{&in}
		if got := testing.AllocsPerRun(100, func() {
			w.structured(family, absent)
			w.reasoning(nil)
			w.document(nil)
			w.dialect(nil)
			w.generationConfig(nil)
		}); got != 0 {
			t.Errorf("%s: looking for what is not there allocates %v times", family, got)
		}
		if got := testing.AllocsPerRun(100, func() { walkInput(&in, family, absent) }); got != 0 {
			t.Errorf("%s: walking a request with no members allocates %v times", family, got)
		}
	}
}

// A provider that defaults the schema of a structured output, or a Bedrock
// tool catalogue, supplies prompt text the caller left out, as one that defaults
// the tools does, and the member the caller sent is not added to.
func TestDefaultedStructuredOutputsAndCataloguesAreCounted(t *testing.T) {
	const schema = `{"type":"object","properties":{"city":{"type":"string"}}}`
	for _, tc := range []struct {
		name      string
		family    openai.Family
		body, own string // the request, and the same request with a member of its own
		defaults  map[string]json.RawMessage
	}{
		{"chat", openai.FamilyChat,
			`{"model":"m","messages":[{"role":"user","content":"abcd"}]}`,
			`{"model":"m","messages":[{"role":"user","content":"abcd"}],"response_format":{"type":"text"}}`,
			map[string]json.RawMessage{"response_format": json.RawMessage(`{"type":"json_schema","json_schema":{"name":"o","schema":` + schema + `}}`)}},
		{"responses", openai.FamilyResponses,
			`{"model":"m","input":"abcd"}`,
			`{"model":"m","input":"abcd","text":{"format":{"type":"text"}}}`,
			map[string]json.RawMessage{"text": json.RawMessage(`{"format":{"type":"json_schema","name":"o","schema":` + schema + `}}`)}},
		{"anthropic output config", openai.FamilyAnthropic,
			`{"messages":[{"role":"user","content":"abcd"}]}`,
			`{"messages":[{"role":"user","content":"abcd"}],"output_config":{"effort":"high"}}`,
			map[string]json.RawMessage{"output_config": json.RawMessage(`{"format":{"type":"json_schema","schema":` + schema + `}}`)}},
		{"anthropic output format", openai.FamilyAnthropic,
			`{"messages":[{"role":"user","content":"abcd"}]}`,
			`{"messages":[{"role":"user","content":"abcd"}],"output_format":{"type":"text"}}`,
			map[string]json.RawMessage{"output_format": json.RawMessage(`{"type":"json_schema","schema":` + schema + `}`)}},
		// The encoder merges a default generation config into the caller's, so a
		// caller's own response schema is what shields the provider's.
		{"gemini", openai.FamilyGemini,
			`{"contents":[{"role":"user","parts":[{"text":"abcd"}]}]}`,
			`{"contents":[{"role":"user","parts":[{"text":"abcd"}]}],"generationConfig":{"maxOutputTokens":9,"responseSchema":{"type":"OBJECT"}}}`,
			map[string]json.RawMessage{"generationConfig": json.RawMessage(`{"responseSchema":` + schema + `}`)}},
		{"bedrock tool catalogue", openai.FamilyBedrock,
			`{"messages":[{"role":"user","content":[{"text":"abcd"}]}]}`,
			`{"messages":[{"role":"user","content":[{"text":"abcd"}]}],"toolConfig":{}}`,
			map[string]json.RawMessage{"toolConfig": json.RawMessage(`{"tools":[{"toolSpec":{"name":"f","inputSchema":{"json":` + schema + `}}}]}`)}},
		{"bedrock structured output", openai.FamilyBedrock,
			`{"messages":[{"role":"user","content":[{"text":"abcd"}]}]}`,
			`{"messages":[{"role":"user","content":[{"text":"abcd"}]}],"outputConfig":{}}`,
			map[string]json.RawMessage{"outputConfig": json.RawMessage(`{"textFormat":{"type":"json_schema","structure":{"jsonSchema":{"name":"o","schema":` + strconv.Quote(schema) + `}}}}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			walk := func(body string) *Prompt {
				if tc.family == openai.FamilyChat || tc.family == openai.FamilyResponses {
					return Walk(mustParse(t, tc.family, body))
				}
				return Walk(native(t, tc.family, body))
			}
			request := walk(tc.body)
			plain, with := request.Estimate(Counter{}, nil), request.Estimate(Counter{}, tc.defaults)
			if added := with.Input - plain.Input; added < HeuristicTokens(schema) {
				t.Fatalf("a default that supplies a schema adds %d tokens to the %d of the request, which is less than the schema", added, plain.Input)
			}
			own := walk(tc.own)
			if got, want := own.Estimate(Counter{}, tc.defaults).Input, own.Estimate(Counter{}, nil).Input; got != want {
				t.Fatalf("a default was counted beside the member the caller sent: %d, want %d", got, want)
			}
		})
	}
}

// A default generation config is merged into the one the caller sent member by
// member, as the encoder does, so a caller that sent only a bound to its output
// still has the provider's response schema sent, and counted. The count is that
// of the request the provider is sent.
func TestDefaultedGenerationConfigIsMergedIntoTheCallers(t *testing.T) {
	const (
		schema = `{"type":"object","properties":{"city":{"type":"string"}}}`
		other  = `{"type":"object","properties":{"country":{"type":"string"},"population":{"type":"integer"}}}`
		prefix = `{"contents":[{"role":"user","parts":[{"text":"abcd"}]}],"generationConfig":`
	)
	for _, tc := range []struct {
		name, config, defaults string
		sent                   string // the generation config the provider is sent
	}{
		{"a caller that sent a bound", `{"maxOutputTokens":9}`, `{"responseMimeType":"application/json","responseSchema":` + schema + `}`,
			`{"maxOutputTokens":9,"responseMimeType":"application/json","responseSchema":` + schema + `}`},
		{"a caller that sent no config", ``, `{"responseSchema":` + schema + `}`, `{"responseSchema":` + schema + `}`},
		{"a caller that sent an empty config", `{}`, `{"responseJsonSchema":` + schema + `}`, `{"responseJsonSchema":` + schema + `}`},
		{"a caller's own schema", `{"responseSchema":{"type":"OBJECT"}}`, `{"responseSchema":` + other + `}`, `{"responseSchema":{"type":"OBJECT"}}`},
		{"a schema in the other field", `{"responseJsonSchema":` + schema + `}`, `{"responseSchema":` + other + `}`,
			`{"responseJsonSchema":` + schema + `,"responseSchema":` + other + `}`},
		{"a schema named with an escape", `{"maxOutputTokens":9}`, `{"responseSch\u0065ma":` + schema + `}`, `{"maxOutputTokens":9,"responseSchema":` + schema + `}`},
		{"a caller that opted out of the config", `null`, `{"responseSchema":` + schema + `}`, `null`},
		{"a config that is not an object", `"none"`, `{"responseSchema":` + schema + `}`, `"none"`},
		{"a default that holds no schema", `{"maxOutputTokens":9}`, `{"temperature":0.2,"topK":4}`, `{"maxOutputTokens":9,"temperature":0.2,"topK":4}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, sent := `{"contents":[{"role":"user","parts":[{"text":"abcd"}]}]`, prefix+tc.sent+`}`
			if tc.config != "" {
				body += `,"generationConfig":` + tc.config
			}
			request := Walk(native(t, openai.FamilyGemini, body+`}`))
			defaults := map[string]json.RawMessage{"generationConfig": json.RawMessage(tc.defaults)}
			got := request.Estimate(ForModel("gpt-4o"), defaults)
			want := Walk(native(t, openai.FamilyGemini, sent)).Estimate(ForModel("gpt-4o"), nil)
			if got.Input != want.Input || got.Provenance != want.Provenance {
				t.Fatalf("the estimate is %d tokens, %s; the request the provider is sent is %d, %s", got.Input, got.Provenance, want.Input, want.Provenance)
			}
		})
	}
}

// A provider that defaults only what is not prompt text, which is nearly every
// default of a field the walker reads a part of, costs a request no walk.
func TestDefaultsThatHoldNoPromptTextAreNotWalked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		family   openai.Family
		body     string
		defaults string
	}{
		{"gemini bounds", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"abcd"}]}],"generationConfig":{"maxOutputTokens":9}}`,
			`{"generationConfig":{"maxOutputTokens":512,"temperature":0.2,"responseMimeType":"application/json"}}`},
		{"gemini sampling of a request with no config", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"abcd"}]}]}`,
			`{"generationConfig":{"temperature":0.2}}`},
		{"gemini function calling mode", openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"abcd"}]}]}`,
			`{"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"generationConfig":{"temperature":0.2}}`},
		{"anthropic effort", openai.FamilyAnthropic, `{"messages":[{"role":"user","content":"abcd"}]}`,
			`{"output_config":{"effort":"high"},"max_tokens":512}`},
		{"chat json mode", openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"abcd"}]}`,
			`{"response_format":{"type":"json_object"}}`},
		{"responses plain text", openai.FamilyResponses, `{"model":"m","input":"abcd"}`,
			`{"text":{"verbosity":"low"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request *openai.Request
			if tc.family == openai.FamilyChat || tc.family == openai.FamilyResponses {
				request = mustParse(t, tc.family, tc.body)
			} else {
				request = native(t, tc.family, tc.body)
			}
			var defaults map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.defaults), &defaults); err != nil {
				t.Fatal(err)
			}
			prompt := Walk(request)
			if got := testing.AllocsPerRun(100, func() {
				if in, ok := prompt.defaulted(defaults); ok {
					t.Fatalf("defaults that hold no prompt text supplied %+v", in)
				}
			}); got != 0 {
				t.Errorf("looking at defaults that hold no prompt text allocates %v times", got)
			}
		})
	}
}

// TestAHeuristicTailKeepsTheFramingOfTheExactPart is the other side of the test
// below: a prompt whose first piece fits the bound is framed whatever its tail
// costs, because what the framing is charged on was counted exactly, even when
// too little of it was exact for the tail to be anything but the heuristic.
func TestAHeuristicTailKeepsTheFramingOfTheExactPart(t *testing.T) {
	text := strings.Repeat("a", 40_000)
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": "Be brief."},
		map[string]any{"role": "user", "content": text},
	}})
	e := Walk(mustParse(t, openai.FamilyChat, string(body))).Estimate(ForModel("gpt-4o"), nil)
	// "Be brief." is three tokens. The run is too long to count, and what is
	// counted before it is too short to say what it costs, so it and the two
	// roles after it, forty thousand and ten bytes, are four bytes to a token.
	// Then two messages of three and the reply's priming of three.
	if want := int64(3 + 10_003 + 2*3 + 3); e.Input != want || e.Provenance != ProvenanceHeuristic {
		t.Fatalf("input %d (%s), want %d, heuristic", e.Input, e.Provenance, want)
	}
}

func TestMeterDegradationLeavesNoFraming(t *testing.T) {
	// One piece longer than the exact bound has nothing to calibrate from, so
	// the count is the plain heuristic; a heuristic is not framed, because the
	// gateway never framed one.
	text := strings.Repeat("a", 2*ExactBytes)
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": text}}})
	e := Walk(mustParse(t, openai.FamilyChat, string(body))).Estimate(ForModel("gpt-4o"), nil)
	if e.Provenance != ProvenanceHeuristic || e.Input != HeuristicTokens(text) {
		t.Fatalf("input %d (%s), want the heuristic %d", e.Input, e.Provenance, HeuristicTokens(text))
	}
}

func TestEstimateTokensSaturate(t *testing.T) {
	e := Walk(mustParse(t, openai.FamilyChat, `{"model":"m","max_completion_tokens":9007199254740991,"n":1024,"messages":[{"role":"user","content":"hi"}]}`)).Estimate(Counter{}, nil)
	if e.Tokens() != MaxTokens {
		t.Fatalf("an absurd request reserves %d, want %d", e.Tokens(), int64(MaxTokens))
	}
}

// TestPromptFieldsNamesEveryFieldTheWalkerReads keeps the quick test for a
// provider default that supplies prompt text honest: a field the walker reads
// and the list lacks would be ignored when a provider defaults it.
func TestPromptFieldsNamesEveryFieldTheWalkerReads(t *testing.T) {
	read := map[string]bool{}
	for _, family := range walkerFamilies {
		var in input
		walkInput(&in, family, func(name string) json.RawMessage {
			read[name] = true
			return nil
		})
	}
	for name := range read {
		if !slices.Contains(promptFields, name) {
			t.Errorf("the walker reads %q, which promptFields does not list", name)
		}
	}
	for _, name := range promptFields {
		if !read[name] {
			t.Errorf("promptFields lists %q, which no family's walk reads", name)
		}
	}
}

// TestReplyIsTheReservationBeyondTheInput holds Reply to what Tokens reserves
// past the prompt, so that a budget pricing the reply and a window counting its
// tokens read the one bound.
func TestReplyIsTheReservationBeyondTheInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family openai.Family
		body   string
		want   int64
	}{
		{"unbounded output is the default", openai.FamilyChat,
			`{"model":"m","messages":[{"role":"user","content":"hello"}]}`, DefaultOutputTokens},
		{"max_tokens bounds one reply", openai.FamilyChat,
			`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`, 10},
		{"every candidate may be as long", openai.FamilyChat,
			`{"model":"m","max_completion_tokens":7,"n":3,"messages":[{"role":"user","content":"hi"}]}`, 21},
		{"an embedding has no reply", openai.FamilyEmbeddings, `{"model":"m","input":"hello"}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Walk(mustParse(t, tc.family, tc.body)).Estimate(Counter{}, nil)
			if got := e.Reply(); got != tc.want {
				t.Fatalf("Reply = %d, want %d", got, tc.want)
			}
			if got, want := e.Tokens(), max(e.Input+e.Reply(), 1); got != want {
				t.Fatalf("Tokens = %d, want the input and the reply, %d", got, want)
			}
		})
	}
	if got := Walk(nil).Estimate(Counter{}, nil).Reply(); got != DefaultOutputTokens {
		t.Fatalf("a request that named nothing replies %d, want the default", got)
	}
	// A reply bound beyond what the limiter stores saturates instead of wrapping.
	huge := mustParse(t, openai.FamilyChat, `{"model":"m","max_tokens":9007199254740991,"n":8,"messages":[{"role":"user","content":"hi"}]}`)
	if got := Walk(huge).Estimate(Counter{}, nil).Reply(); got != MaxTokens {
		t.Fatalf("Reply = %d, want it saturated at %d", got, MaxTokens)
	}
}
