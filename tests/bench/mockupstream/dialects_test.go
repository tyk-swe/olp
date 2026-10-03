//go:build bench

package mockupstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func promptFor(body string) int64 { return int64((len(body) + 3) / 4) }

// chatUsage is the usage object of Chat Completions.
type chatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func TestOpenAIUnary(t *testing.T) {
	s := defaultMock(t)
	body := openAIBody("gpt-mock", false, "hello")
	resp, got := s.post(t, request{path: "/v1/chat/completions", body: body})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || resp.ContentLength != int64(len(got)) {
		t.Fatalf("response %d %v length %d, body %d bytes", resp.StatusCode, resp.Header, resp.ContentLength, len(got))
	}
	var doc struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role, Content string
			}
			FinishReason string `json:"finish_reason"`
		}
		Usage chatUsage
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	if doc.Object != "chat.completion" || doc.Model != "gpt-mock" || len(doc.Choices) != 1 {
		t.Fatalf("document %s", got)
	}
	if c := doc.Choices[0]; c.Message.Role != "assistant" || c.Message.Content != wantText(16) || c.FinishReason != "stop" {
		t.Fatalf("choice %+v", c)
	}
	if want := promptFor(body); doc.Usage.PromptTokens != want || doc.Usage.CompletionTokens != 16 || doc.Usage.TotalTokens != want+16 {
		t.Fatalf("usage %+v, want prompt %d", doc.Usage, want)
	}
}

func TestOpenAIStreamFrames(t *testing.T) {
	s := defaultMock(t)
	body := openAIBody("gpt-mock", true, "hello")
	resp, got := s.post(t, request{path: "/v1/chat/completions", body: body})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response %d %v", resp.StatusCode, resp.Header)
	}
	frames, err := readFrames(strings.NewReader(string(got)), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A role chunk, sixteen tokens, the finish chunk, the usage chunk and the terminator.
	if len(frames) != 1+16+1+1+1 {
		t.Fatalf("%d frames:\n%s", len(frames), got)
	}
	type chunk struct {
		Model   string
		Choices []struct {
			Delta        struct{ Role, Content string }
			FinishReason *string `json:"finish_reason"`
		}
		Usage *chatUsage
	}
	decode := func(f frame) chunk {
		var c chunk
		if err := json.Unmarshal([]byte(f.data), &c); err != nil {
			t.Fatalf("%v in %q", err, f.data)
		}
		if c.Model != "gpt-mock" {
			t.Fatalf("model %q in %q", c.Model, f.data)
		}
		return c
	}
	if c := decode(frames[0]); c.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first frame %q", frames[0].data)
	}
	var content strings.Builder
	for _, f := range frames[1:17] {
		c := decode(f)
		if c.Choices[0].FinishReason != nil {
			t.Fatalf("finish before the last token: %q", f.data)
		}
		content.WriteString(c.Choices[0].Delta.Content)
	}
	if content.String() != wantText(16) {
		t.Fatalf("content %q", content.String())
	}
	if c := decode(frames[17]); c.Choices[0].FinishReason == nil || *c.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish frame %q", frames[17].data)
	}
	usage := decode(frames[18])
	if want := promptFor(body); len(usage.Choices) != 0 || usage.Usage == nil || usage.Usage.PromptTokens != want || usage.Usage.CompletionTokens != 16 || usage.Usage.TotalTokens != want+16 {
		t.Fatalf("usage frame %q, want prompt %d", frames[18].data, want)
	}
	if frames[19].data != "[DONE]" {
		t.Fatalf("terminator %q", frames[19].data)
	}
}

func TestResponsesUnary(t *testing.T) {
	s := defaultMock(t)
	body := responsesBody("gpt-mock", false, "hello")
	resp, got := s.post(t, request{path: "/v1/responses", body: body})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("response %d %v", resp.StatusCode, resp.Header)
	}
	var doc struct {
		Object, Model, Status string
		Output                []struct {
			Type, Role string
			Content    []struct{ Type, Text string }
		}
	}
	var usage struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		}
	}
	if err := json.Unmarshal(got, &doc); err != nil || json.Unmarshal(got, &usage) != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	if doc.Object != "response" || doc.Model != "gpt-mock" || doc.Status != "completed" || len(doc.Output) != 1 || len(doc.Output[0].Content) != 1 {
		t.Fatalf("document %s", got)
	}
	if c := doc.Output[0].Content[0]; c.Type != "output_text" || c.Text != wantText(16) {
		t.Fatalf("content %+v", c)
	}
	if want := promptFor(body); usage.Usage.InputTokens != want || usage.Usage.OutputTokens != 16 || usage.Usage.TotalTokens != want+16 {
		t.Fatalf("usage %+v, want input %d", usage.Usage, want)
	}
}

func TestResponsesStreamFrames(t *testing.T) {
	s := defaultMock(t)
	body := responsesBody("gpt-mock", true, "hello")
	resp, got := s.post(t, request{path: "/v1/responses", body: body})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response %d %v", resp.StatusCode, resp.Header)
	}
	frames, err := readFrames(strings.NewReader(string(got)), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Sixteen token deltas, then response.completed.
	if len(frames) != 16+1 {
		t.Fatalf("%d frames:\n%s", len(frames), got)
	}
	var content strings.Builder
	for _, f := range frames[:16] {
		var delta struct{ Type, Delta string }
		if f.event != "response.output_text.delta" || json.Unmarshal([]byte(f.data), &delta) != nil || delta.Type != f.event {
			t.Fatalf("frame %q %q", f.event, f.data)
		}
		content.WriteString(delta.Delta)
	}
	if content.String() != wantText(16) {
		t.Fatalf("content %q", content.String())
	}
	var done struct {
		Type     string
		Response struct {
			Status string
			Usage  struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			}
		}
	}
	last := frames[16]
	if last.event != "response.completed" || json.Unmarshal([]byte(last.data), &done) != nil || done.Type != last.event || done.Response.Status != "completed" ||
		done.Response.Usage.InputTokens != promptFor(body) || done.Response.Usage.OutputTokens != 16 {
		t.Fatalf("final frame %q %q", last.event, last.data)
	}
}

func TestAnthropicUnary(t *testing.T) {
	s := defaultMock(t)
	body := anthropicBody("claude-mock", false, "hello")
	resp, got := s.post(t, request{path: "/v1/messages", body: body, headers: map[string]string{"anthropic-version": "2023-06-01"}})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("response %d %v", resp.StatusCode, resp.Header)
	}
	var doc struct {
		Type, Role, Model string
		Content           []struct{ Type, Text string }
		StopReason        string `json:"stop_reason"`
		Usage             struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		}
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	if doc.Type != "message" || doc.Role != "assistant" || doc.Model != "claude-mock" || doc.StopReason != "end_turn" || len(doc.Content) != 1 || doc.Content[0].Type != "text" || doc.Content[0].Text != wantText(16) {
		t.Fatalf("document %s", got)
	}
	if doc.Usage.InputTokens != promptFor(body) || doc.Usage.OutputTokens != 16 {
		t.Fatalf("usage %+v", doc.Usage)
	}
}

func TestAnthropicStreamEventSequence(t *testing.T) {
	s := defaultMock(t)
	body := anthropicBody("claude-mock", true, "hello")
	resp, got := s.post(t, request{path: "/v1/messages", body: body})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response %d %v", resp.StatusCode, resp.Header)
	}
	frames, err := readFrames(strings.NewReader(string(got)), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, f := range frames {
		events = append(events, f.event)
	}
	want := []string{"message_start", "content_block_start", "ping"}
	for range 16 {
		want = append(want, "content_block_delta")
	}
	want = append(want, "content_block_stop", "message_delta", "message_stop")
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events\n%v\nwant\n%v", events, want)
	}
	// Every frame's data names its own event, and the usage lands where the
	// protocol puts it.
	var text strings.Builder
	for _, f := range frames {
		var doc struct {
			Type    string
			Message struct {
				Model string
				Usage struct {
					InputTokens int64 `json:"input_tokens"`
				}
			}
			Delta struct{ Text string }
			Usage struct {
				OutputTokens int64 `json:"output_tokens"`
			}
		}
		if err := json.Unmarshal([]byte(f.data), &doc); err != nil {
			t.Fatalf("%v in %q", err, f.data)
		}
		if doc.Type != f.event {
			t.Fatalf("event %q carries type %q", f.event, doc.Type)
		}
		switch f.event {
		case "message_start":
			if doc.Message.Model != "claude-mock" || doc.Message.Usage.InputTokens != promptFor(body) {
				t.Fatalf("message_start %q", f.data)
			}
		case "content_block_delta":
			text.WriteString(doc.Delta.Text)
		case "message_delta":
			if doc.Usage.OutputTokens != 16 {
				t.Fatalf("message_delta %q", f.data)
			}
		}
	}
	if text.String() != wantText(16) {
		t.Fatalf("text %q", text.String())
	}
}

func TestGeminiUnaryAndStream(t *testing.T) {
	s := defaultMock(t)
	type response struct {
		Candidates []struct {
			Content struct {
				Role  string
				Parts []struct{ Text string }
			}
			FinishReason string `json:"finishReason"`
		}
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			TotalTokenCount      int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
		ModelVersion string `json:"modelVersion"`
	}
	t.Run("unary", func(t *testing.T) {
		resp, got := s.post(t, request{path: "/v1beta/models/gemini-mock:generateContent", body: geminiBody})
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("response %d %v", resp.StatusCode, resp.Header)
		}
		var doc response
		if err := json.Unmarshal(got, &doc); err != nil {
			t.Fatalf("%v\n%s", err, got)
		}
		c := doc.Candidates[0]
		if c.Content.Role != "model" || c.Content.Parts[0].Text != wantText(16) || c.FinishReason != "STOP" || doc.ModelVersion != "gemini-mock" {
			t.Fatalf("document %s", got)
		}
		if u := doc.UsageMetadata; u.PromptTokenCount != promptFor(geminiBody) || u.CandidatesTokenCount != 16 || u.TotalTokenCount != promptFor(geminiBody)+16 {
			t.Fatalf("usage %+v", u)
		}
	})
	t.Run("stream", func(t *testing.T) {
		resp, got := s.post(t, request{path: "/v1beta/models/gemini-mock:streamGenerateContent?alt=sse", body: geminiBody})
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("response %d %v", resp.StatusCode, resp.Header)
		}
		if !strings.Contains(string(got), "\r\n\r\n") {
			t.Fatalf("Gemini frames end in CRLF CRLF:\n%q", got)
		}
		frames, err := readFrames(strings.NewReader(string(got)), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		// Sixteen token chunks and the closing chunk with the finish reason.
		if len(frames) != 17 {
			t.Fatalf("%d frames:\n%s", len(frames), got)
		}
		var text strings.Builder
		for i, f := range frames {
			var doc response
			if err := json.Unmarshal([]byte(f.data), &doc); err != nil {
				t.Fatalf("%v in %q", err, f.data)
			}
			text.WriteString(doc.Candidates[0].Content.Parts[0].Text)
			if last := i == len(frames)-1; (doc.Candidates[0].FinishReason == "STOP") != last || (doc.UsageMetadata.TotalTokenCount != 0) != last {
				t.Fatalf("frame %d %q: finish reason and usage belong on the last chunk only", i, f.data)
			}
		}
		if text.String() != wantText(16) {
			t.Fatalf("text %q", text.String())
		}
	})
}

// TestPathsAreMatchedBySuffix covers the shapes a provider's endpoint takes:
// OLP appends the operation to whatever base the provider declares.
func TestPathsAreMatchedBySuffix(t *testing.T) {
	s := defaultMock(t)
	for _, tc := range []struct {
		path, body, contentType, want string
	}{
		{"/chat/completions", openAIBody("m", false, "x"), "application/json", `"chat.completion"`},
		{"/openai/deployments/d/chat/completions?api-version=1", openAIBody("m", false, "x"), "application/json", `"chat.completion"`},
		{"/messages", anthropicBody("m", false, "x"), "application/json", `"type":"message"`},
		{"/v1/projects/p/locations/l/publishers/google/models/gemini-x:generateContent", geminiBody, "application/json", `"modelVersion":"gemini-x"`},
		{"/v1beta/models/gemini%2Dx:streamGenerateContent?alt=sse", geminiBody, "text/event-stream", `"modelVersion":"gemini-x"`},
	} {
		resp, got := s.post(t, request{path: tc.path, body: tc.body})
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != tc.contentType || !strings.Contains(string(got), tc.want) {
			t.Errorf("%s: %d %q\n%s", tc.path, resp.StatusCode, resp.Header.Get("Content-Type"), got)
		}
	}
}

func TestUnroutableRequests(t *testing.T) {
	s := defaultMock(t)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/v1/chat/completions", 405},
		{http.MethodPut, "/v1/messages", 405},
		{http.MethodGet, "/v1beta/models/m:generateContent", 405},
		{http.MethodPost, "/v1/embeddings", 404},
		{http.MethodPost, "/v1beta/generateContent", 404},
		{http.MethodPost, "/_mock/nothing", 404},
	} {
		req, _ := http.NewRequest(tc.method, "http://mock"+tc.path, strings.NewReader("{}"))
		resp, err := client(s).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || !json.Valid(body) {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, resp.StatusCode, body)
		}
	}
}

func TestModelListings(t *testing.T) {
	cfg := Config{Default: DefaultBehavior(), Models: map[string]Behavior{"zeta": DefaultBehavior(), "alpha": DefaultBehavior()}}
	s := newMock(t, cfg)
	get := func(path string, headers map[string]string) map[string]json.RawMessage {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://mock"+path, nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client(s).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var doc map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, err)
		}
		return doc
	}
	ids := func(raw json.RawMessage, key string) string {
		var items []map[string]any
		if err := json.Unmarshal(raw, &items); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, item := range items {
			out = append(out, item[key].(string))
		}
		return strings.Join(out, ",")
	}
	openai := get("/v1/models", map[string]string{"Authorization": "Bearer k"})
	if got := ids(openai["data"], "id"); got != "alpha,mock-model,zeta" || string(openai["object"]) != `"list"` {
		t.Errorf("OpenAI listing %s", got)
	}
	anthropic := get("/v1/models", map[string]string{"X-Api-Key": "k", "Anthropic-Version": "2023-06-01"})
	if got := ids(anthropic["data"], "id"); got != "alpha,mock-model,zeta" || string(anthropic["has_more"]) != "false" || string(anthropic["last_id"]) != `"zeta"` {
		t.Errorf("Anthropic listing %s %s", got, anthropic["has_more"])
	}
	google := get("/v1beta/models", map[string]string{"X-Goog-Api-Key": "k"})
	if got := ids(google["models"], "name"); got != "models/alpha,models/mock-model,models/zeta" {
		t.Errorf("Gemini listing %s", got)
	}
}

func TestErrorBodiesFollowTheDialect(t *testing.T) {
	s := defaultMock(t)
	for _, tc := range []struct {
		path, body string
		status     int
		want       []string
	}{
		{"/v1/chat/completions", openAIBody("m", false, "x"), 503, []string{`"error":{`, `"type":"server_error"`, `injected 503 Service Unavailable`}},
		{"/v1/chat/completions", openAIBody("m", true, "x"), 429, []string{`"type":"rate_limit_error"`, `"code":"rate_limit_exceeded"`}},
		{"/v1/chat/completions", openAIBody("m", false, "x"), 401, []string{`"code":"invalid_api_key"`}},
		{"/v1/messages", anthropicBody("m", false, "x"), 503, []string{`"type":"error"`, `"type":"overloaded_error"`}},
		{"/v1/messages", anthropicBody("m", true, "x"), 429, []string{`"type":"rate_limit_error"`}},
		{"/v1/messages", anthropicBody("m", false, "x"), 500, []string{`"type":"api_error"`}},
		{"/v1beta/models/m:generateContent", geminiBody, 503, []string{`"code":503`, `"status":"UNAVAILABLE"`}},
		{"/v1beta/models/m:streamGenerateContent?alt=sse", geminiBody, 429, []string{`"status":"RESOURCE_EXHAUSTED"`}},
		{"/v1beta/models/m:generateContent", geminiBody, 400, []string{`"status":"INVALID_ARGUMENT"`}},
	} {
		resp, got := s.post(t, request{path: tc.path, body: tc.body, headers: map[string]string{"x-mock-status": strconv.Itoa(tc.status)}})
		if resp.StatusCode != tc.status || resp.Header.Get("Content-Type") != "application/json" || !json.Valid(got) {
			t.Errorf("%s: %d %q %s", tc.path, resp.StatusCode, resp.Header.Get("Content-Type"), got)
			continue
		}
		mustContain(t, got, tc.want...)
	}
}

// TestHeadersControlTheResponse covers the controls that shape a successful
// response; the timing, failure and status controls have their own tests.
func TestHeadersControlTheResponse(t *testing.T) {
	s := defaultMock(t)
	for _, tc := range []struct {
		name, path, body string
		count            func(t *testing.T, body []byte) int
	}{
		{"openai unary", "/v1/chat/completions", openAIBody("m", false, "x"), func(t *testing.T, body []byte) int {
			var doc struct{ Usage chatUsage }
			json.Unmarshal(body, &doc)
			return int(doc.Usage.CompletionTokens)
		}},
		{"anthropic stream", "/v1/messages", anthropicBody("m", true, "x"), func(t *testing.T, body []byte) int {
			frames, _ := readFrames(strings.NewReader(string(body)), time.Now())
			n := 0
			for _, f := range frames {
				if f.event == "content_block_delta" {
					n++
				}
			}
			return n
		}},
		{"gemini stream", "/v1beta/models/m:streamGenerateContent?alt=sse", geminiBody, func(t *testing.T, body []byte) int {
			frames, _ := readFrames(strings.NewReader(string(body)), time.Now())
			return len(frames) - 1
		}},
	} {
		for _, n := range []int{1, 2, 64, 65, 300} {
			_, got := s.post(t, request{path: tc.path, body: tc.body, headers: map[string]string{"x-mock-output-tokens": strconv.Itoa(n)}})
			if c := tc.count(t, got); c != n {
				t.Errorf("%s with %d output tokens produced %d", tc.name, n, c)
			}
		}
	}
}

func TestNoModelIsEchoedAsTheDefault(t *testing.T) {
	s := defaultMock(t)
	_, got := s.post(t, request{path: "/v1/chat/completions", body: `{"messages":[]}`})
	mustContain(t, got, `"model":"mock-model"`)
}
