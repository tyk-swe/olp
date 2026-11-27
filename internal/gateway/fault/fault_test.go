package fault

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

func TestRedactNativeFailureDocumentScrubsValuesAndRefusesSecretKeys(t *testing.T) {
	t.Parallel()
	var creds egress.Sensitive
	creds.Add("sk-live-secret<123>")

	raw := `{"error":{"message":"bad key sk-live-secret\u003c123\u003e","items":["ok","sk-live-secret<123>"],"code":401.00}}`
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 4096, MaxDepth: 16, MaxNodes: 256})
	if err != nil {
		t.Fatal(err)
	}
	got, err := RedactNativeFailureDocument(doc, creds)
	if err != nil {
		t.Fatalf("RedactNativeFailureDocument returned error: %v", err)
	}
	if strings.Contains(got.Raw(), "sk-live-secret") {
		t.Fatalf("redacted document leaked credential: %s", got.Raw())
	}
	if !strings.Contains(got.Raw(), `"code":401.00`) {
		t.Errorf("untouched numeric literal was modified: %s", got.Raw())
	}

	clean, err := oif.ParseJSON([]byte(`{"error":{"message":"unrelated"}}`), oif.Limits{MaxBytes: 4096, MaxDepth: 16, MaxNodes: 256})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := RedactNativeFailureDocument(clean, creds)
	if err != nil || unchanged.Raw() != clean.Raw() {
		t.Fatalf("clean document changed: %s err=%v", unchanged.Raw(), err)
	}

	keyLeak, err := oif.ParseJSON([]byte(`{"sk-live-secret<123>":"value"}`), oif.Limits{MaxBytes: 4096, MaxDepth: 16, MaxNodes: 256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RedactNativeFailureDocument(keyLeak, creds); err == nil {
		t.Fatal("credential-bearing member name was not refused")
	}
}

func TestWriteSurfaceErrorFormatsEnvelopesAndHeaders(t *testing.T) {
	t.Parallel()
	param := "messages"
	cases := []struct {
		name           string
		surface        string
		err            *Error
		wantHeader     map[string]string
		wantFieldCheck func(*testing.T, map[string]any)
	}{
		{
			name:    "openai with retry and no-retry",
			surface: "openai",
			err: &Error{
				Status:     http.StatusTooManyRequests,
				Type:       "rate_limit_error",
				Code:       "rate_limited",
				Message:    "slow down",
				Param:      &param,
				RetryAfter: 1500 * time.Millisecond,
				NoRetry:    true,
			},
			wantHeader: map[string]string{
				"Content-Type":   "application/json",
				"Retry-After":    "2",
				"X-Should-Retry": "false",
			},
			wantFieldCheck: func(t *testing.T, body map[string]any) {
				inner := body["error"].(map[string]any)
				if inner["code"] != "rate_limited" || inner["param"] != "messages" {
					t.Fatalf("openai body = %#v", body)
				}
			},
		},
		{
			name:    "anthropic maps 503 to overloaded_error",
			surface: "anthropic",
			err:     ServerError(http.StatusServiceUnavailable, "overloaded", "busy"),
			wantFieldCheck: func(t *testing.T, body map[string]any) {
				inner := body["error"].(map[string]any)
				if body["type"] != "error" || inner["type"] != "overloaded_error" {
					t.Fatalf("anthropic body = %#v", body)
				}
			},
		},
		{
			name:    "gemini maps 429 to RESOURCE_EXHAUSTED",
			surface: "gemini",
			err:     &Error{Status: http.StatusTooManyRequests, Code: "quota_exceeded", Message: "exhausted", Param: &param},
			wantFieldCheck: func(t *testing.T, body map[string]any) {
				inner := body["error"].(map[string]any)
				if inner["status"] != "RESOURCE_EXHAUSTED" {
					t.Fatalf("gemini body = %#v", body)
				}
			},
		},
		{
			name:       "bedrock sets X-Amzn-Errortype",
			surface:    "bedrock",
			err:        NotFoundError("route_not_found", "missing"),
			wantHeader: map[string]string{"X-Amzn-Errortype": "route_not_found"},
			wantFieldCheck: func(t *testing.T, body map[string]any) {
				if body["code"] != "route_not_found" || body["message"] != "missing" {
					t.Fatalf("bedrock body = %#v", body)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			WriteSurfaceError(rec, tc.err, tc.surface)
			if rec.Code != tc.err.Status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.err.Status)
			}
			for k, want := range tc.wantHeader {
				if got := rec.Header().Get(k); got != want {
					t.Errorf("header %s = %q, want %q", k, got, want)
				}
			}
			var decoded map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("invalid JSON response: %v", err)
			}
			tc.wantFieldCheck(t, decoded)
		})
	}
}

func TestRateLimitHeadersAndResetDurationFormatting(t *testing.T) {
	t.Parallel()
	if !ReportsRate("openai") || !ReportsRate("anthropic") || ReportsRate("gemini") || ReportsRate("bedrock") {
		t.Fatal("ReportsRate returned unexpected surface support")
	}

	resetAt := time.Date(2026, 11, 20, 12, 1, 0, 0, time.UTC)
	state := limits.RateState{
		RequestLimit:     100,
		RequestRemaining: 42,
		TokenLimit:       50000,
		TokenRemaining:   12345,
		ResetAfter:       8640 * time.Millisecond,
		ResetAt:          resetAt,
	}

	openaiHeaders := http.Header{}
	var buf HeaderBuffer
	var list HeaderList
	list.Apply(openaiHeaders, list.Rate(buf[:0], "openai", state, state.ResetAfter))
	wantOpenAI := map[string]string{
		headerOpenAILimitRequests:     "100",
		headerOpenAIRemainingRequests: "42",
		headerOpenAIResetRequests:     "8.64s",
		headerOpenAILimitTokens:       "50000",
		headerOpenAIRemainingTokens:   "12345",
		headerOpenAIResetTokens:       "8.64s",
	}
	for k, want := range wantOpenAI {
		if got := openaiHeaders.Get(k); got != want {
			t.Errorf("openai header %s = %q, want %q", k, got, want)
		}
	}

	anthropicHeaders := http.Header{}
	reqOnly := limits.RateState{RequestLimit: 10, RequestRemaining: 0, ResetAt: resetAt}
	var abuf HeaderBuffer
	var alist HeaderList
	alist.Apply(anthropicHeaders, alist.Rate(abuf[:0], "anthropic", reqOnly, time.Minute))
	if got := anthropicHeaders.Get(headerAnthropicResetRequests); got != resetAt.Format(time.RFC3339) {
		t.Errorf("anthropic reset = %q, want %q", got, resetAt.Format(time.RFC3339))
	}
	if got := anthropicHeaders.Get(headerAnthropicLimitTokens); got != "" {
		t.Errorf("unexpected token limit header on request-only state: %q", got)
	}

	geminiHeaders := http.Header{}
	setRateLimitHeaders(geminiHeaders, "gemini", state)
	if len(geminiHeaders) != 0 {
		t.Errorf("gemini should emit no rate headers, got %v", geminiHeaders)
	}

	durationCases := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "0s"},
		{0, "0s"},
		{20 * time.Millisecond, "20ms"},
		{time.Second, "1s"},
		{8640 * time.Millisecond, "8.64s"},
		{time.Minute, "1m0s"},
		{90*time.Second + 500*time.Millisecond, "1m30.5s"},
		{2 * time.Hour, "2h0m0s"},
		{500 * time.Microsecond, "500µs"},
	}
	for _, tc := range durationCases {
		if got := string(appendResetDuration(nil, tc.d)); got != tc.want {
			t.Errorf("appendResetDuration(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestStagingErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind       staging.Kind
		wantStatus int
		wantCode   string
	}{
		{staging.TooLarge, http.StatusRequestEntityTooLarge, "media_too_large"},
		{staging.Timeout, http.StatusRequestTimeout, "multipart_parser_timeout"},
		{staging.Overloaded, http.StatusServiceUnavailable, "media_admission_overloaded"},
		{staging.Unavailable, http.StatusServiceUnavailable, "media_spool_unavailable"},
		{staging.Invalid, http.StatusBadRequest, "invalid_request"},
	}
	for _, tc := range cases {
		got := StagingError(&staging.Error{Kind: tc.kind, Message: "detail"})
		if got.Status != tc.wantStatus || got.Code != tc.wantCode {
			t.Errorf("StagingError(%v) = (%d, %q), want (%d, %q)", tc.kind, got.Status, got.Code, tc.wantStatus, tc.wantCode)
		}
	}
}
