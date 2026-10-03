//go:build integration

package gosdk

import (
	"encoding/json"
	"math"
	"testing"

	"google.golang.org/genai"
)

var geminiWeather = &genai.Tool{FunctionDeclarations: []*genai.FunctionDeclaration{{
	Name: "get_weather", Description: "Current weather for a city.", ParametersJsonSchema: weatherParameters,
}}}

// geminiPath is the path of a native Gemini method on the scripted upstream.
func (h harness) geminiPath(method string) string {
	return "/gemini/v1beta/models/" + h.upstreamModels["gemini"] + ":" + method
}

func TestGeminiGenerateContent(t *testing.T) {
	h := connect(t)
	response, err := h.genai(t, h.apiKey).Models.GenerateContent(timeout(t), h.models["gemini"], genai.Text("Say hello."), &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("You are terse.", genai.RoleUser),
		MaxOutputTokens:   64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Text(); got != h.defaultReply || response.Candidates[0].FinishReason != genai.FinishReasonStop {
		t.Fatalf("reply %q (%s)", got, response.Candidates[0].FinishReason)
	}
	if usage := response.UsageMetadata; usage == nil || usage.PromptTokenCount == 0 || usage.CandidatesTokenCount == 0 || usage.TotalTokenCount == 0 {
		t.Fatalf("usage %+v", usage)
	}

	r := h.onlyRequest(t)
	body := r.json(t)
	if r.Path != h.geminiPath("generateContent") || r.Model != h.upstreamModels["gemini"] || r.Stream {
		t.Fatalf("upstream saw %+v", r)
	}
	if r.Headers["x-goog-api-key"] != "[present]" {
		t.Fatalf("upstream headers %v", r.Headers)
	}
	expect(t, body, "Say hello.", "contents", 0, "parts", 0, "text")
	expect(t, body, "You are terse.", "systemInstruction", "parts", 0, "text")
	expect(t, body, 64.0, "generationConfig", "maxOutputTokens")
}

func TestGeminiGenerateContentStreaming(t *testing.T) {
	h := connect(t)
	var text string
	chunks := 0
	var last *genai.GenerateContentResponse
	for response, err := range h.genai(t, h.apiKey).Models.GenerateContentStream(timeout(t), h.models["gemini"], genai.Text("Say hello."), nil) {
		if err != nil {
			t.Fatal(err)
		}
		text += response.Text()
		chunks++
		last = response
	}
	if chunks < 2 || text != h.defaultReply {
		t.Fatalf("%d chunks assembled %q", chunks, text)
	}
	if last.Candidates[0].FinishReason != genai.FinishReasonStop || last.UsageMetadata == nil || last.UsageMetadata.TotalTokenCount == 0 {
		t.Fatalf("last chunk %+v", last)
	}

	r := h.onlyRequest(t)
	if r.Path != h.geminiPath("streamGenerateContent") || !r.Stream || r.Model != h.upstreamModels["gemini"] {
		t.Fatalf("upstream saw %+v", r)
	}
	if r.Query != "alt=sse" {
		t.Fatalf("the stream was not requested as server-sent events: %q", r.Query)
	}
}

func TestGeminiFunctionCallingLoop(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "unary"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			h := connect(t)
			client := h.genai(t, h.apiKey)
			config := &genai.GenerateContentConfig{Tools: []*genai.Tool{geminiWeather}}
			prompt := "Weather in Paris and Rome? " + tool("get_weather", map[string]any{"city": "Paris"}) + also("get_weather", map[string]any{"city": "Rome"})
			history := genai.Text(prompt)

			var turn *genai.Content
			var calls []*genai.FunctionCall
			if streaming {
				// A streamed turn is assembled from its chunks' parts, as a
				// chat loop does.
				turn = &genai.Content{Role: genai.RoleModel}
				for response, err := range client.Models.GenerateContentStream(timeout(t), h.models["gemini"], history, config) {
					if err != nil {
						t.Fatal(err)
					}
					turn.Parts = append(turn.Parts, response.Candidates[0].Content.Parts...)
					calls = append(calls, response.FunctionCalls()...)
				}
			} else {
				response, err := client.Models.GenerateContent(timeout(t), h.models["gemini"], history, config)
				if err != nil {
					t.Fatal(err)
				}
				turn, calls = response.Candidates[0].Content, response.FunctionCalls()
			}
			if len(calls) != 2 {
				t.Fatalf("%d function calls: %s", len(calls), mustJSON(turn))
			}
			cities := []string{"Paris", "Rome"}
			conditions := []string{"sunny", "rainy"}
			var answers []*genai.Part
			for i, call := range calls {
				if call.Name != "get_weather" || call.Args["city"] != cities[i] {
					t.Fatalf("call %d: %+v", i, call)
				}
				answers = append(answers, genai.NewPartFromFunctionResponse(call.Name, map[string]any{"output": conditions[i]}))
			}

			final, err := client.Models.GenerateContent(timeout(t), h.models["gemini"],
				append(history, turn, genai.NewContentFromParts(answers, genai.RoleUser)), config)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := final.Text(), h.afterTools("sunny", "rainy"); got != want {
				t.Fatalf("final text %q, want %q", got, want)
			}

			requests := h.requests(t, 2)
			if requests[0].Stream != streaming || requests[1].Stream || requests[0].Script != "tool_call" || requests[1].Script != "tool_result" {
				t.Fatalf("requests %+v", requests)
			}
			first, follow := requests[0].json(t), requests[1].json(t)
			expect(t, first, "get_weather", "tools", 0, "functionDeclarations", 0, "name")
			expect(t, follow, "model", "contents", 1, "role")
			expect(t, follow, "user", "contents", 2, "role")
			for i, city := range cities {
				expect(t, follow, "get_weather", "contents", 1, "parts", i, "functionCall", "name")
				expect(t, follow, city, "contents", 1, "parts", i, "functionCall", "args", "city")
				expect(t, follow, "get_weather", "contents", 2, "parts", i, "functionResponse", "name")
				expect(t, follow, conditions[i], "contents", 2, "parts", i, "functionResponse", "response", "output")
			}
		})
	}
}

func TestGeminiThoughtsAreReportedSeparately(t *testing.T) {
	h := connect(t)
	response, err := h.genai(t, h.apiKey).Models.GenerateContent(timeout(t), h.models["gemini"],
		genai.Text("Say hello. "+think("Considering a greeting.")), &genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{IncludeThoughts: true, ThinkingBudget: genai.Ptr[int32](128)},
		})
	if err != nil {
		t.Fatal(err)
	}
	var thoughts string
	for _, part := range response.Candidates[0].Content.Parts {
		if part.Thought {
			thoughts += part.Text
		}
	}
	// Text() is the answer; the thought is a part of its own.
	if thoughts != "Considering a greeting." || response.Text() != h.defaultReply {
		t.Fatalf("thoughts %q, answer %q", thoughts, response.Text())
	}
	body := h.onlyRequest(t).json(t)
	expect(t, body, true, "generationConfig", "thinkingConfig", "includeThoughts")
	expect(t, body, 128.0, "generationConfig", "thinkingConfig", "thinkingBudget")
}

func TestGeminiStructuredOutput(t *testing.T) {
	h := connect(t)
	response, err := h.genai(t, h.apiKey).Models.GenerateContent(timeout(t), h.models["gemini"], genai.Text("Forecast for Paris."), &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
		ResponseJsonSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
				"temp": map[string]any{"type": "integer"},
			},
			"required": []string{"city", "temp"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var forecast struct {
		City string `json:"city"`
		Temp int    `json:"temp"`
	}
	if err := json.Unmarshal([]byte(response.Text()), &forecast); err != nil {
		t.Fatalf("the reply %q is not an instance of the schema: %v", response.Text(), err)
	}
	body := h.onlyRequest(t).json(t)
	expect(t, body, "application/json", "generationConfig", "responseMimeType")
	expect(t, body, "integer", "generationConfig", "responseJsonSchema", "properties", "temp", "type")
}

func TestGeminiCountTokens(t *testing.T) {
	h := connect(t)
	client := h.genai(t, h.apiKey)
	prompt := genai.Text("How many tokens is this prompt?")
	count, err := client.Models.CountTokens(timeout(t), h.models["gemini"], prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The count is what the same prompt reports as its prompt tokens, however
	// the generation is configured.
	response, err := client.Models.GenerateContent(timeout(t), h.models["gemini"], prompt, &genai.GenerateContentConfig{Temperature: genai.Ptr[float32](0.5), MaxOutputTokens: 64})
	if err != nil {
		t.Fatal(err)
	}
	if count.TotalTokens == 0 || count.TotalTokens != response.UsageMetadata.PromptTokenCount {
		t.Fatalf("counted %d tokens, the response reported %d", count.TotalTokens, response.UsageMetadata.PromptTokenCount)
	}

	requests := h.requests(t, 2)
	if requests[0].Path != h.geminiPath("countTokens") || requests[0].Model != h.upstreamModels["gemini"] {
		t.Fatalf("upstream saw %+v", requests[0])
	}
	expect(t, requests[0].json(t), "How many tokens is this prompt?", "contents", 0, "parts", 0, "text")
}

func TestGeminiEmbedContent(t *testing.T) {
	h := connect(t)
	contents := []*genai.Content{genai.NewContentFromText("alpha", genai.RoleUser), genai.NewContentFromText("beta", genai.RoleUser)}
	response, err := h.genai(t, h.apiKey).Models.EmbedContent(timeout(t), h.models["gemini_embed_strict"], contents, &genai.EmbedContentConfig{OutputDimensionality: genai.Ptr[int32](8)})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Embeddings) != 2 {
		t.Fatalf("%d embeddings", len(response.Embeddings))
	}
	for i, embedding := range response.Embeddings {
		var norm float64
		for _, v := range embedding.Values {
			norm += float64(v) * float64(v)
		}
		if len(embedding.Values) != 8 || math.Abs(norm-1) > 1e-5 {
			t.Fatalf("embedding %d has %d values and norm %v", i, len(embedding.Values), norm)
		}
	}
	if response.Embeddings[0].Values[0] == response.Embeddings[1].Values[0] && response.Embeddings[0].Values[1] == response.Embeddings[1].Values[1] {
		t.Fatal("different inputs gave the same embedding")
	}

	// The SDK embeds through the batch method, as one request with an entry per
	// content, and the gateway rewrites the model each carries as well as the one
	// in the path.
	r := h.onlyRequest(t)
	body := r.json(t)
	if r.Dialect != "gemini.batch_embed" || r.Path != h.geminiPath("batchEmbedContents") || r.Model != h.upstreamModels["gemini"] {
		t.Fatalf("upstream saw %+v", r)
	}
	for i, text := range []string{"alpha", "beta"} {
		expect(t, body, "models/"+h.upstreamModels["gemini"], "requests", i, "model")
		expect(t, body, text, "requests", i, "content", "parts", 0, "text")
		expect(t, body, 8.0, "requests", i, "outputDimensionality")
	}
}

func TestGeminiModels(t *testing.T) {
	h := connect(t)
	listed := map[string]bool{}
	for model, err := range h.genai(t, h.apiKey).Models.All(timeout(t)) {
		if err != nil {
			t.Fatal(err)
		}
		listed[model.Name] = true
	}
	for name, slug := range h.models {
		if !listed["models/"+slug] {
			t.Errorf("route %s (%s) is not listed: %v", name, slug, listed)
		}
	}
	h.untouched(t)
}
