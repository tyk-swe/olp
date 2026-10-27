package routing

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/contentpolicy"
)

func passthroughConfig() *PassthroughConfig {
	return &PassthroughConfig{
		Dialect:                    "bound-native",
		Methods:                    []string{"GET", "POST"},
		Paths:                      []string{"/v1/widgets/*", "/v1/status"},
		Query:                      []string{"limit"},
		Headers:                    []string{"X-Widget-Mode"},
		AcknowledgeOpaqueUnmetered: true,
	}
}

func TestPassthroughMatchSegmentBounds(t *testing.T) {
	cfg := passthroughConfig()
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/v1/widgets/abc", true},
		{"/v1/widgets", false},
		{"/v1/widgets/", false},
		{"/v1/widgets/a/b", false},
		{"/v1/status", true},
		{"/v1/status/", false},
		{"/v1/Status", false},
		{"/v2/widgets/abc", false},
	} {
		got, err := cfg.Match("GET", tc.path)
		if err != nil && tc.want {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if got != tc.want {
			t.Fatalf("%s matched=%v want %v", tc.path, got, tc.want)
		}
	}
	if ok, err := cfg.Match("DELETE", "/v1/status"); err != nil || ok {
		t.Fatalf("method not allowed matched: %v %v", ok, err)
	}
}

func TestPassthroughNormalizeRejectsEscapes(t *testing.T) {
	for _, raw := range []string{
		"/v1/widgets/%2e%2e",
		"/v1/widgets/%2E%2E/secret",
		"/v1/widgets/a%2fb",
		"/v1/widgets/a%5cb",
		"/v1/widgets/%2fsecret",
		"/v1/widgets/..",
		"/v1/widgets/.",
		"/v1/widgets/%252e%252e",
		"/v1//widgets/a",
		"//evil.example/v1/widgets/a",
		"/v1/widgets/a#frag",
		"/v1/widgets/a%23frag",
		"/v1/widgets/a%3fquery",
		"/v1/widgets/a\\b",
		"https://evil.example/v1/widgets/a",
		"v1/widgets/a",
		"/v1/widgets/%zz",
		"/v1/widgets/a%00b",
	} {
		if _, err := NormalizePassthroughPath(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	got, err := NormalizePassthroughPath("/v1/widgets/a%20b")
	if err != nil || got != "/v1/widgets/a b" {
		t.Fatalf("normalize once: %q %v", got, err)
	}
}

func TestPassthroughConfigValidation(t *testing.T) {
	if err := passthroughConfig().Validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	for _, mutate := range []func(*PassthroughConfig){
		func(c *PassthroughConfig) { c.AcknowledgeOpaqueUnmetered = false },
		func(c *PassthroughConfig) { c.Methods = []string{"CONNECT"} },
		func(c *PassthroughConfig) { c.Methods = []string{"GET", "GET"} },
		func(c *PassthroughConfig) { c.Methods = nil },
		func(c *PassthroughConfig) { c.Paths = []string{"v1/widgets"} },
		func(c *PassthroughConfig) { c.Paths = []string{"/v1/**"} },
		func(c *PassthroughConfig) { c.Paths = []string{"/v1/*/.."} },
		func(c *PassthroughConfig) { c.Paths = []string{"/v1/%2e"} },
		func(c *PassthroughConfig) { c.Paths = []string{"/v1/a?b"} },
		func(c *PassthroughConfig) { c.Paths = nil },
		func(c *PassthroughConfig) { c.Headers = []string{"Authorization"} },
		func(c *PassthroughConfig) { c.Headers = []string{"Connection"} },
		func(c *PassthroughConfig) { c.Headers = []string{"Cookie"} },
		func(c *PassthroughConfig) { c.Query = []string{"bad name"} },
		func(c *PassthroughConfig) { c.Dialect = "Bad Dialect" },
	} {
		c := passthroughConfig()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Fatalf("invalid config accepted: %+v", c)
		}
	}
}

func passthroughRoute() Route {
	return Route{
		Slug:           "escape",
		Operations:     []string{"passthrough"},
		OverallTimeout: 1000,
		MaxAttempts:    1,
		Fidelity:       RouteFidelity{Mode: FidelityStrict},
		Targets:        []Target{{ID: "11111111-1111-1111-1111-111111111111", ProviderID: "22222222-2222-2222-2222-222222222222", ProviderModel: "m", Weight: 1, Timeout: 1000, RoutingID: "33333333-3333-3333-3333-333333333333"}},
		Behavior:       Behavior{Passthrough: passthroughConfig()},
	}
}

func TestPassthroughRouteConflicts(t *testing.T) {
	if err := passthroughRoute().ValidatePassthrough(); err != nil {
		t.Fatalf("valid route refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Route)
	}{
		{"fallback", func(r *Route) { r.Fallbacks = []Fallback{{Route: "other", On: []string{"exhausted"}}} }},
		{"cache", func(r *Route) { r.Cache = &cache.Policy{Mode: "exact"} }},
		{"prompt cache", func(r *Route) { r.PromptCache = &cache.PromptPolicy{} }},
		{"shadow", func(r *Route) { r.Targets[0].Shadow = &Shadow{SampleRate: 0.5} }},
		{"two targets", func(r *Route) { r.Targets = append(r.Targets, r.Targets[0]) }},
		{"transformed", func(r *Route) { r.Fidelity.Mode = FidelityTransformed }},
		{"content policy", func(r *Route) { r.ContentPolicy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "x"}}} }},
		{"other operation", func(r *Route) { r.Operations = []string{"passthrough", "generation"} }},
		{"selector", func(r *Route) { r.Selectors = []Selector{{ID: "s", Tags: []string{"a"}}} }},
		{"retry", func(r *Route) { r.Retry = Retry{"timeout": {MaxRetries: 1, BaseBackoffMS: 1, MaxBackoffMS: 1}} }},
		{"attempts", func(r *Route) { r.MaxAttempts = 2 }},
		{"missing ack", func(r *Route) { r.Passthrough.AcknowledgeOpaqueUnmetered = false }},
	} {
		r := passthroughRoute()
		tc.mutate(&r)
		err := r.ValidatePassthrough()
		if err == nil {
			t.Fatalf("%s accepted", tc.name)
		}
		if !strings.Contains(err.Error(), "pass-through") && !strings.Contains(err.Error(), "passthrough") {
			t.Fatalf("%s error lacks context: %v", tc.name, err)
		}
	}
	plain := passthroughRoute()
	plain.Passthrough, plain.Operations = nil, []string{"generation"}
	if err := plain.ValidatePassthrough(); err != nil {
		t.Fatalf("non passthrough route refused: %v", err)
	}
	if !passthroughRoute().IsPassthrough() || plain.IsPassthrough() {
		t.Fatal("IsPassthrough mismatch")
	}
}
