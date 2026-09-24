package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestGeminiLifecycleAuthenticationRejectsAmbiguousSDKForms(t *testing.T) {
	for _, test := range []struct {
		query, header, want string
		invalid             bool
	}{
		{query: "key=one", want: "one"},
		{header: "two", want: "two"},
		{query: "key=one&key=two", invalid: true},
		{query: "key=one", header: "two", invalid: true},
		{query: "endpoint=https%3A%2F%2Fevil.example&key=one", invalid: true},
	} {
		r := &http.Request{URL: &url.URL{RawQuery: test.query}, Header: http.Header{}}
		if test.header != "" {
			r.Header.Set("X-Goog-Api-Key", test.header)
		}
		key, err := geminiClientKey(r)
		if test.invalid {
			if err == nil {
				t.Fatalf("ambiguous key accepted: %+v", test)
			}
		} else if err != nil || key != test.want {
			t.Fatalf("native key form %+v returned %q, %v", test, key, err)
		}
	}
}

func TestGeminiProjectedIdentityScanDecodesEscapedExtensions(t *testing.T) {
	for _, test := range []struct {
		name, body string
		leaks      bool
	}{
		{"plain", `{"id":"local","native":"int_foo"}`, true},
		{"escaped string", `{"id":"local","native":{"ref":"int\u005ffoo"}}`, true},
		{"escaped member", `{"id":"local","native":{"int\u005ffoo":true}}`, true},
		{"array", `{"id":"local","native":["other","int\u005ffoo"]}`, true},
		{"safe", `{"id":"local","native":{"ref":"other"}}`, false},
		{"invalid", `{"id":"local","native":`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := containsNativeResourceID([]byte(test.body), "int_foo"); got != test.leaks {
				t.Fatalf("decoded identity leak=%t, want %t", got, test.leaks)
			}
		})
	}
}

type stalledGeminiWriter struct{}

func (stalledGeminiWriter) Write(ctx context.Context, _ websocket.MessageType, _ []byte) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestGeminiLiveFrameWriteHasIndependentDeadline(t *testing.T) {
	start := time.Now()
	err := writeGeminiLiveFrameWithin(t.Context(), stalledGeminiWriter{}, websocket.MessageText, []byte(`{"serverContent":{}}`), 25*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("stalled Live frame did not stop at its own deadline: %v, %s", err, time.Since(start))
	}
}

func TestGeminiLiveUsageRequiresDocumentedResponseCategoryAndConservativeSettlement(t *testing.T) {
	const valid = `{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":9,"responseTokenCount":5,"cachedContentTokenCount":3}}`
	u := geminiLiveUsage([]byte(valid), 1024)
	if u == nil || u.InputTokens != 9 || u.OutputTokens != 5 || u.CachedInputTokens == nil || *u.CachedInputTokens != 3 || !geminiLiveTurnComplete([]byte(valid), 1024) {
		t.Fatalf("documented Live usage/turn was not observed: %+v", u)
	}
	for _, raw := range []string{
		`{"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":5}}`,
		`{"usageMetadata":{"promptTokenCount":9,"responseTokenCount":5,"cachedContentTokenCount":10}}`,
		`{"usageMetadata":{"responseTokenCount":5}}`,
	} {
		if got := geminiLiveUsage([]byte(raw), 1024); got != nil {
			t.Fatalf("uncertain usage counted as complete: %s %+v", raw, got)
		}
	}
	x := &execution{estimate: 100, dispatched: true, facts: []AttemptFact{{Usage: &openai.Usage{InputTokens: 12, OutputTokens: 3}, BillingUncertain: true}}}
	if amount := geminiLiveSettlement(x); amount == nil || *amount != 100 {
		t.Fatalf("uncertain session under-reserved: %v", amount)
	}
	x.facts[0].BillingUncertain = false
	if amount := geminiLiveSettlement(x); amount == nil || *amount != 15 {
		t.Fatalf("complete session did not settle observed usage: %v", amount)
	}
}
