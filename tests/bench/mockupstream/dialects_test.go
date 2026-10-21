//go:build bench

package mockupstream

import (
	"encoding/json"
	"strconv"
	"testing"
)

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
