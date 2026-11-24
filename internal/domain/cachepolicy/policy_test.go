package cachepolicy

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestParseControlsReadsEachDirectiveOnce(t *testing.T) {
	sixty, zero := 60, 0
	for raw, want := range map[string]Controls{
		"":                                {},
		" No-Cache , max-age = 60, ttl=0": {NoCache: true, MaxAge: &sixty, TTL: &zero},
		"no-store,,":                      {NoStore: true},
	} {
		if got, err := ParseControls(raw); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %+v, %v", raw, got, err)
		}
	}
	for _, raw := range []string{
		"no-cache, NO-CACHE",
		"max-age=1, max-age=2",
		"no-store=true",
		"max-age",
		"max-age=",
		"ttl=-1",
		"ttl=604801",
		"ttl=1.5",
		"private",
		"no-cache" + strings.Repeat(" ", 512),
	} {
		if got, err := ParseControls(raw); err == nil {
			t.Errorf("%.40q was accepted as %+v", raw, got)
		}
	}
}

func TestPolicyValidateRefusesUnsafeOrContradictoryPolicies(t *testing.T) {
	semantic := Policy{Mode: "semantic", EmbeddingRoute: "embeddings", AllowUnsealedVectors: true}
	for name, policy := range map[string]Policy{
		"exact defaults":    {Mode: "exact"},
		"exact, widest":     {Mode: "exact", TTLSeconds: MaxTTLSeconds, MaxEntryBytes: maxEntryBytes, Scope: "route", Streaming: "bypass", Operations: []string{"embeddings", "rerank"}, CountHitsAgainst: []string{"tokens", "requests", "budgets"}},
		"semantic defaults": semantic,
	} {
		if err := policy.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := semantic.Effective().Operations; !reflect.DeepEqual(got, []string{"generation"}) {
		t.Errorf("a semantic policy caches %v by default, want generation only", got)
	}
	with := func(change func(*Policy)) Policy {
		policy := semantic
		change(&policy)
		return policy
	}
	for name, policy := range map[string]Policy{
		"no mode":                             {},
		"an unknown mode":                     {Mode: "fuzzy"},
		"an unknown scope":                    {Mode: "exact", Scope: "installation"},
		"a negative TTL":                      {Mode: "exact", TTLSeconds: -1},
		"a TTL over a week":                   {Mode: "exact", TTLSeconds: MaxTTLSeconds + 1},
		"an entry over the ceiling":           {Mode: "exact", MaxEntryBytes: maxEntryBytes + 1},
		"an unknown streaming mode":           {Mode: "exact", Streaming: "drop"},
		"an unknown operation":                {Mode: "exact", Operations: []string{"images"}},
		"a repeated operation":                {Mode: "exact", Operations: []string{"rerank", "rerank"}},
		"hits that skip request limits":       {Mode: "exact", CountHitsAgainst: []string{"tokens"}},
		"semantic options on an exact policy": {Mode: "exact", EmbeddingRoute: "embeddings"},
		"multiturn on an exact policy":        {Mode: "exact", AllowMultiturn: true},
		"unsealed vectors on an exact policy": {Mode: "exact", AllowUnsealedVectors: true},
		"semantic without acknowledgement":    with(func(p *Policy) { p.AllowUnsealedVectors = false }),
		"semantic without an embedding route": with(func(p *Policy) { p.EmbeddingRoute = "" }),
		"semantic for embeddings":             with(func(p *Policy) { p.Operations = []string{"generation", "embeddings"} }),
		"a similarity above one":              with(func(p *Policy) { p.SimilarityThreshold = 1.01 }),
		"a negative similarity":               with(func(p *Policy) { p.SimilarityThreshold = -0.5 }),
		"a NaN similarity":                    with(func(p *Policy) { p.SimilarityThreshold = math.NaN() }),
		"too many candidates":                 with(func(p *Policy) { p.MaxCandidates = maxCandidateCount + 1 }),
	} {
		if err := policy.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPromptPolicyValidateRequiresASessionLabelForSessionKeys(t *testing.T) {
	for name, c := range map[string]struct {
		policy   PromptPolicy
		accepted bool
	}{
		"defaults":                {PromptPolicy{}, true},
		"every position":          {PromptPolicy{Positions: []string{"tools", "system", "last_user"}, TTL: "1h", KeySource: "key"}, true},
		"a labelled session":      {PromptPolicy{KeySource: "session", SessionLabel: "conversation"}, true},
		"an unlabelled session":   {PromptPolicy{KeySource: "session"}, false},
		"an oversized label":      {PromptPolicy{KeySource: "session", SessionLabel: strings.Repeat("a", 33)}, false},
		"an unknown key source":   {PromptPolicy{KeySource: "user"}, false},
		"an unsupported lifetime": {PromptPolicy{TTL: "24h"}, false},
		"a repeated position":     {PromptPolicy{Positions: []string{"system", "system"}}, false},
		"an unknown position":     {PromptPolicy{Positions: []string{"assistant"}}, false},
	} {
		if err := c.policy.Validate(); (err == nil) != c.accepted {
			t.Errorf("%s: got %v, accepted=%v", name, err, c.accepted)
		}
	}
}
