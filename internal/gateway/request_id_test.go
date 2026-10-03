package gateway

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The Anthropic SDKs report the request id of an error from the request-id
// header, so the Anthropic surface sends it, beside X-Request-Id and with the
// same value, on every answer. The other surfaces do not send it.
func TestRequestIDIsSentWhereTheSDKsReadIt(t *testing.T) {
	h := anthropicTransformedHarness(t)
	recordAnthropic(h)
	message := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	chat := `{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	anthropicHeaders := map[string]string{"Anthropic-Version": "2023-06-01"}
	for _, tc := range []struct {
		name, method, path, key, body string
		status                        int
		anthropic                     bool
	}{
		{"messages", http.MethodPost, messagesPath, fullKey, message, http.StatusOK, true},
		{"streaming messages", http.MethodPost, messagesPath, fullKey, strings.Replace(message, `"max_tokens":16`, `"max_tokens":16,"stream":true`, 1), http.StatusOK, true},
		{"count_tokens", http.MethodPost, countPath, fullKey, `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]}`, http.StatusOK, true},
		{"an invalid key", http.MethodPost, messagesPath, "olp_unknown", message, http.StatusUnauthorized, true},
		{"an unknown route", http.MethodPost, messagesPath, fullKey, strings.Replace(message, routeSlug, "no-such-route", 1), http.StatusNotFound, true},
		{"a malformed request", http.MethodPost, messagesPath, fullKey, `{"model":`, http.StatusBadRequest, true},
		{"models", http.MethodGet, "/anthropic/v1/models", fullKey, "", http.StatusOK, true},
		{"an unknown endpoint", http.MethodGet, "/anthropic/v1/nothing", fullKey, "", http.StatusNotFound, true},
		{"chat completions", http.MethodPost, "/v1/chat/completions", fullKey, chat, http.StatusOK, false},
		{"chat completions with an invalid key", http.MethodPost, "/v1/chat/completions", "olp_unknown", chat, http.StatusUnauthorized, false},
		{"gemini models", http.MethodGet, "/gemini/v1/models", fullKey, "", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, named := range []string{"", "client-named"} {
				headers := map[string]string{}
				if tc.anthropic {
					headers = anthropicHeaders
				}
				if named != "" {
					headers = map[string]string{"X-Request-Id": named, "Anthropic-Version": "2023-06-01"}
				}
				var body []byte
				if tc.body != "" {
					body = []byte(tc.body)
				}
				resp := h.do(t.Context(), tc.method, tc.path, tc.key, body, headers)
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != tc.status {
					t.Fatalf("status=%d, want %d", resp.StatusCode, tc.status)
				}
				id := resp.Header.Get("X-Request-Id")
				if id == "" || (named != "" && id != named) {
					t.Fatalf("X-Request-Id=%q with the request id %q", id, named)
				}
				if got := resp.Header.Values("Request-Id"); tc.anthropic && !slices.Equal(got, []string{id}) || !tc.anthropic && len(got) != 0 {
					t.Fatalf("Request-Id=%q beside X-Request-Id=%q", got, id)
				}
			}
		})
	}
}

// A browser SDK reads the id from the exposed headers.
func TestRequestIDIsExposedToBrowsersOfTheAnthropicSurface(t *testing.T) {
	const origin = "https://app.example"
	h := newHarness(t, Config{MaxInFlight: 8, CORSAllowedOrigins: []string{origin}})
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/anthropic/v1/models", true},
		{http.MethodOptions, "/anthropic/v1/messages", true},
		{http.MethodGet, "/v1/models", false},
		{http.MethodOptions, "/v1/chat/completions", false},
		{http.MethodGet, "/gemini/v1/models", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := h.do(t.Context(), tc.method, tc.path, fullKey, nil, map[string]string{"Origin": origin})
			resp.Body.Close()
			exposed := strings.Split(strings.ToLower(strings.ReplaceAll(resp.Header.Get("Access-Control-Expose-Headers"), " ", "")), ",")
			if !slices.Contains(exposed, "x-request-id") || slices.Contains(exposed, "request-id") != tc.want {
				t.Fatalf("exposed headers %q, want request-id exposed=%v", exposed, tc.want)
			}
		})
	}
}
