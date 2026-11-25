package surface

import (
	"net/url"
	"testing"
)

func TestStripNonSemanticQuery(t *testing.T) {
	t.Parallel()
	geminiValid := url.Values{"key": {"secret"}, "alt": {"sse"}}
	if param, _ := StripNonSemanticQuery("gemini", geminiValid); param != "" {
		t.Fatalf("unexpected error for valid gemini key: %s", param)
	}
	if geminiValid.Has("key") || geminiValid.Get("alt") != "sse" {
		t.Fatalf("gemini query = %v", geminiValid)
	}

	geminiDup := url.Values{"key": {"a", "b"}}
	if param, msg := StripNonSemanticQuery("gemini", geminiDup); param != "key" || msg == "" {
		t.Fatalf("duplicate gemini key = (%q, %q)", param, msg)
	}

	anthropicBeta := url.Values{"beta": {"true"}, "other": {"1"}}
	if param, _ := StripNonSemanticQuery("anthropic", anthropicBeta); param != "" {
		t.Fatalf("unexpected error for anthropic beta=true: %s", param)
	}
	if anthropicBeta.Has("beta") || anthropicBeta.Get("other") != "1" {
		t.Fatalf("anthropic query = %v", anthropicBeta)
	}

	anthropicFalse := url.Values{"beta": {"false"}}
	if param, _ := StripNonSemanticQuery("anthropic", anthropicFalse); param != "" || !anthropicFalse.Has("beta") {
		t.Fatalf("anthropic beta=false was altered: %v", anthropicFalse)
	}
}

func TestPathPrefixRoutesBackToTheSurfaceItWasAskedFor(t *testing.T) {
	t.Parallel()
	for _, reserved := range Reserved() {
		name := reserved.Surface.Name
		prefix := PathPrefix(name)
		if !reserved.Surface.Inference || prefix == "" {
			continue
		}
		if got := Of(prefix + "/models"); got.Name != name {
			t.Errorf("PathPrefix(%q) = %q, which routes to the %s surface", name, prefix, got.Name)
		}
	}
	if prefix := PathPrefix("no-such-surface"); prefix != "" {
		t.Errorf("an unknown surface has the path prefix %q", prefix)
	}
}
