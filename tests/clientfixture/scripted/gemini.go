package scripted

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

var gemini = vendor{
	name: "gemini",
	authorized: func(r *http.Request, credential string) bool {
		return r.Header.Get("X-Goog-Api-Key") == credential
	},
	fail: func(w http.ResponseWriter, status int, kind, message string) {
		writeJSON(w, status, map[string]any{"error": map[string]any{"code": status, "message": message, "status": geminiStatus(status)}})
	},
}

func geminiStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	}
	if status >= 500 {
		return "INTERNAL"
	}
	return "INVALID_ARGUMENT"
}

func (f *Fixture) registerGemini() {
	type endpoint struct {
		dialect string
		serve   func(*request, bool)
	}
	endpoints := map[string]endpoint{
		"generateContent":       {"gemini.generate", f.generateContent},
		"streamGenerateContent": {"gemini.stream", f.generateContent},
		"countTokens":           {"gemini.count_tokens", f.countGeminiTokens},
		"embedContent":          {"gemini.embed", f.embedContent},
		"batchEmbedContents":    {"gemini.batch_embed", f.batchEmbedContents},
	}
	// One wildcard serves every action, as `model:action` is a single path
	// segment.
	f.mux.HandleFunc("POST "+GeminiPrefix+"/models/{action}", func(w http.ResponseWriter, r *http.Request) {
		model, action, _ := strings.Cut(r.PathValue("action"), ":")
		e, known := endpoints[action]
		if !known {
			e.dialect = "gemini.unknown"
		}
		f.handle(gemini, e.dialect, func(x *request) {
			switch {
			case !known:
				x.fail(http.StatusNotFound, "not_found", fmt.Sprintf("Method %q is not supported.", action))
			case model != GeminiModel:
				x.fail(http.StatusNotFound, "not_found", fmt.Sprintf("models/%s is not found for API version v1beta, or is not supported for %s.", model, action))
			default:
				e.serve(x, action == "streamGenerateContent")
			}
		})(w, r)
	})
}

type geminiContent struct {
	Role  string           `json:"role"`
	Parts []map[string]any `json:"parts"`
}

type geminiRequest struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction json.RawMessage `json:"systemInstruction"`
	Tools             []struct {
		FunctionDeclarations []struct {
			Name                 string          `json:"name"`
			Parameters           json.RawMessage `json:"parameters"`
			ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
	ToolConfig struct {
		FunctionCallingConfig struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	} `json:"toolConfig"`
	GenerationConfig struct {
		ResponseMimeType   string          `json:"responseMimeType"`
		ResponseSchema     json.RawMessage `json:"responseSchema"`
		ResponseJSONSchema json.RawMessage `json:"responseJsonSchema"`
		ThinkingConfig     struct {
			IncludeThoughts bool `json:"includeThoughts"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
	// GenerateContentRequest wraps the same members in a countTokens request.
	GenerateContentRequest *geminiRequest `json:"generateContentRequest"`
}

// turn reduces a request to the neutral turn. A content that carries function
// responses is a tool-result message.
func (r *geminiRequest) turn() *turn {
	t := newTurn()
	for _, c := range r.Contents {
		if c.Role == "model" {
			continue
		}
		var text, results []string
		for _, p := range c.Parts {
			if s, ok := p["text"].(string); ok && p["thought"] != true {
				text = append(text, s)
			}
			if fr, ok := p["functionResponse"].(map[string]any); ok {
				results = append(results, functionResponseText(fr["response"]))
			}
		}
		if len(results) > 0 {
			t.results(results...)
		} else if len(text) > 0 {
			t.user(strings.Join(text, "\n"))
		}
	}
	for _, tool := range r.Tools {
		for _, d := range tool.FunctionDeclarations {
			schema := d.ParametersJSONSchema
			if len(schema) == 0 {
				schema = d.Parameters
			}
			t.declare(d.Name, schema)
		}
	}
	switch cfg := r.ToolConfig.FunctionCallingConfig; cfg.Mode {
	case "NONE":
		t.hideTools()
	case "ANY":
		t.forced = "*"
		if len(cfg.AllowedFunctionNames) > 0 {
			t.forced = cfg.AllowedFunctionNames[0]
		}
	}
	if gc := r.GenerationConfig; gc.ResponseMimeType == "application/json" {
		switch {
		case len(gc.ResponseJSONSchema) > 0:
			t.schema = gc.ResponseJSONSchema
		case len(gc.ResponseSchema) > 0:
			t.schema = gc.ResponseSchema
		default:
			t.jsonMode = true
		}
	}
	t.thinking = r.GenerationConfig.ThinkingConfig.IncludeThoughts
	return t
}

// promptTokens is the fixture's count of the prompt: the contents and the system
// instruction, whatever else the request configures. generateContent reports it
// as the prompt tokens of a reply and countTokens as its total, so the two agree
// for the same prompt, as they do on Gemini.
func (r *geminiRequest) promptTokens() int {
	encoded, _ := json.Marshal(struct {
		Contents []geminiContent `json:"contents"`
		System   json.RawMessage `json:"systemInstruction,omitempty"`
	}{r.Contents, r.SystemInstruction})
	return tokens(string(encoded))
}

// functionResponseText reads a tool result: the conventional single string
// member, or the response compacted as JSON.
func functionResponseText(response any) string {
	if m, ok := response.(map[string]any); ok {
		for _, key := range []string{"output", "result", "content", "error"} {
			if s, ok := m[key].(string); ok {
				return s
			}
		}
	}
	return compact(rawOf(response))
}

func (f *Fixture) generateContent(x *request, stream bool) {
	var req geminiRequest
	if !x.decode(&req) {
		return
	}
	if len(req.Contents) == 0 {
		x.fail(http.StatusBadRequest, "invalid_argument", "contents is not specified")
		return
	}
	rp := decide(req.turn())
	if x.failScripted(rp, stream) {
		return
	}
	x.note(rp.script, stream)
	u := usageOf(x.body, rp)
	u.input = req.promptTokens()
	usageMetadata := map[string]any{
		"promptTokenCount": u.input, "candidatesTokenCount": u.output - u.reasoning, "totalTokenCount": u.input + u.output,
		"promptTokensDetails": []any{map[string]any{"modality": "TEXT", "tokenCount": u.input}},
	}
	if u.reasoning > 0 {
		usageMetadata["thoughtsTokenCount"] = u.reasoning
	}
	id := f.nextID("gemini")
	chunk := func(parts []any, last bool) map[string]any {
		candidate := map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "index": 0}
		out := map[string]any{"candidates": []any{candidate}, "modelVersion": GeminiModel, "responseId": id}
		if last {
			candidate["finishReason"] = "STOP"
			out["usageMetadata"] = usageMetadata
		}
		return out
	}
	// Chunks of the reply: reasoning, then text deltas or the function calls.
	var chunks [][]any
	if rp.reasoning != "" {
		chunks = append(chunks, []any{map[string]any{"text": rp.reasoning, "thought": true}})
	}
	if len(rp.calls) > 0 {
		parts := make([]any, len(rp.calls))
		for i, c := range rp.calls {
			parts[i] = map[string]any{"functionCall": map[string]any{"name": c.name, "args": json.RawMessage(compact(c.args))}}
		}
		// A thinking model signs the first call of a step, and the client
		// must return the signature with the call.
		if rp.reasoning != "" {
			parts[0].(map[string]any)["thoughtSignature"] = GeminiThoughtSignature
		}
		chunks = append(chunks, parts)
	} else {
		for _, w := range words(rp.text) {
			chunks = append(chunks, []any{map[string]any{"text": w}})
		}
		if rp.text == "" {
			chunks = append(chunks, []any{map[string]any{"text": ""}})
		}
	}
	if !stream {
		var parts []any
		for _, c := range chunks {
			parts = append(parts, c...)
		}
		parts = mergeText(parts)
		writeJSON(x.w, http.StatusOK, chunk(parts, true))
		return
	}
	if x.r.URL.Query().Get("alt") == "sse" {
		s := newSSE(x.w, "\r\n\r\n")
		for i, c := range chunks {
			s.event("", chunk(c, i == len(chunks)-1))
		}
		return
	}
	// Without alt=sse the stream is one JSON array, written incrementally.
	x.w.Header().Set("Content-Type", "application/json")
	x.w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(x.w)
	for i, c := range chunks {
		encoded, _ := json.Marshal(chunk(c, i == len(chunks)-1))
		sep := ","
		if i == 0 {
			sep = "["
		}
		fmt.Fprintf(x.w, "%s\r\n%s", sep, encoded)
		rc.Flush()
	}
	fmt.Fprint(x.w, "\r\n]")
}

// mergeText joins adjacent plain text parts so a unary reply carries one.
func mergeText(parts []any) []any {
	var out []any
	for _, p := range parts {
		m := p.(map[string]any)
		text, isText := m["text"].(string)
		if last := len(out) - 1; isText && m["thought"] == nil && last >= 0 {
			if prev := out[last].(map[string]any); prev["thought"] == nil {
				if prevText, ok := prev["text"].(string); ok {
					prev["text"] = prevText + text
					continue
				}
			}
		}
		out = append(out, p)
	}
	return out
}

func (f *Fixture) countGeminiTokens(x *request, _ bool) {
	var req geminiRequest
	if !x.decode(&req) {
		return
	}
	if req.GenerateContentRequest != nil {
		req = *req.GenerateContentRequest
	}
	if len(req.Contents) == 0 {
		x.fail(http.StatusBadRequest, "invalid_argument", "contents is not specified")
		return
	}
	n := req.promptTokens()
	x.note("count_tokens", false)
	writeJSON(x.w, http.StatusOK, map[string]any{
		"totalTokens":         n,
		"promptTokensDetails": []any{map[string]any{"modality": "TEXT", "tokenCount": n}},
	})
}

type embedRequest struct {
	Model                string        `json:"model"`
	Content              geminiContent `json:"content"`
	OutputDimensionality int           `json:"outputDimensionality"`
}

func (r embedRequest) text() string {
	var texts []string
	for _, p := range r.Content.Parts {
		if s, ok := p["text"].(string); ok {
			texts = append(texts, s)
		}
	}
	return strings.Join(texts, "\n")
}

func (f *Fixture) embedContent(x *request, _ bool) {
	var req embedRequest
	if !x.decode(&req) {
		return
	}
	if len(req.Content.Parts) == 0 {
		x.fail(http.StatusBadRequest, "invalid_argument", "content is not specified")
		return
	}
	x.note("embeddings", false)
	n := tokens(req.text())
	writeJSON(x.w, http.StatusOK, map[string]any{
		"embedding":     map[string]any{"values": embedding(req.text(), req.OutputDimensionality)},
		"usageMetadata": map[string]any{"promptTokenCount": n, "totalTokenCount": n},
	})
}

func (f *Fixture) batchEmbedContents(x *request, _ bool) {
	var req struct {
		Requests []embedRequest `json:"requests"`
	}
	if !x.decode(&req) {
		return
	}
	if len(req.Requests) == 0 {
		x.fail(http.StatusBadRequest, "invalid_argument", "requests is not specified")
		return
	}
	embeddings, total := make([]any, len(req.Requests)), 0
	for i, r := range req.Requests {
		if r.Model != "" && strings.TrimPrefix(r.Model, "models/") != GeminiModel {
			x.fail(http.StatusBadRequest, "invalid_argument", fmt.Sprintf("requests[%d].model: unexpected model %q", i, r.Model))
			return
		}
		embeddings[i] = map[string]any{"values": embedding(r.text(), r.OutputDimensionality)}
		total += tokens(r.text())
	}
	x.note("embeddings", false)
	writeJSON(x.w, http.StatusOK, map[string]any{
		"embeddings":    embeddings,
		"usageMetadata": map[string]any{"promptTokenCount": total, "totalTokenCount": total},
	})
}
