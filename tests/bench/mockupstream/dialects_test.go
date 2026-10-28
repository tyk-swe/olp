//go:build bench

package mockupstream

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestErrorBodiesFollowTheDialect(t *testing.T) {
	s := defaultMock(t)
	dialects := []struct {
		name     string
		path     string
		body     string
		stream   string
		statuses []int
	}{
		{"openai", "/v1/chat/completions", openAIBody("m", false, "x"), openAIBody("m", true, "x"), []int{503, 429, 401}},
		{"anthropic", "/v1/messages", anthropicBody("m", false, "x"), anthropicBody("m", true, "x"), []int{503, 429, 500}},
		{"gemini", "/v1beta/models/m:generateContent", geminiBody, geminiBody, []int{503, 400}},
	}
	for _, dialect := range dialects {
		seen := map[string]int{}
		for _, status := range dialect.statuses {
			resp, got := s.post(t, request{path: dialect.path, body: dialect.body, headers: map[string]string{"x-mock-status": strconv.Itoa(status)}})
			if resp.StatusCode != status || resp.Header.Get("Content-Type") != "application/json" || !json.Valid(got) {
				t.Fatalf("%s %d: %d %q %s", dialect.name, status, resp.StatusCode, resp.Header.Get("Content-Type"), got)
			}
			if prior, dup := seen[string(got)]; dup {
				t.Errorf("%s error body for %d is identical to %d, so the mock does not discriminate statuses", dialect.name, status, prior)
			}
			seen[string(got)] = status
		}
		resp, streamed := s.post(t, request{path: dialect.path, body: dialect.stream, headers: map[string]string{"x-mock-status": "429"}})
		if resp.StatusCode != 429 || !json.Valid(streamed) {
			t.Fatalf("%s streamed 429: %d %s", dialect.name, resp.StatusCode, streamed)
		}
	}
}

func TestGeminiErrorCodeEchoesInjectedStatus(t *testing.T) {
	s := defaultMock(t)
	for _, status := range []int{503, 429, 400} {
		path := "/v1beta/models/m:generateContent"
		if status == 429 {
			path = "/v1beta/models/m:streamGenerateContent?alt=sse"
		}
		_, got := s.post(t, request{path: path, body: geminiBody, headers: map[string]string{"x-mock-status": strconv.Itoa(status)}})
		var body struct {
			Error struct {
				Code   int    `json:"code"`
				Status string `json:"status"`
			} `json:"error"`
		}
		if err := json.Unmarshal(got, &body); err != nil {
			t.Fatalf("%d: %v (%s)", status, err, got)
		}
		if body.Error.Code != status {
			t.Errorf("injected %d came back with error code %d", status, body.Error.Code)
		}
		if body.Error.Status == "" {
			t.Errorf("injected %d came back with no error status (%s)", status, got)
		}
	}
}
