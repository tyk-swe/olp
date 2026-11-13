package cache

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func canonical(t *testing.T, raw string) string {
	t.Helper()
	document, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return string(Canonical(document))
}

func TestCanonicalMatchesOnlyRequestsWithTheSameMeaning(t *testing.T) {
	const request = `{"model":"m","messages":[{"role":"user","content":"a/b é"}],"temperature":1}`
	for _, same := range []string{
		`{ "temperature" : 1, "messages" : [ { "content" : "a/b é", "role" : "user" } ], "model" : "m" }`,
		`{"model":"m","messages":[{"role":"user","content":"a\/b é"}],"temperature":1}`,
	} {
		if canonical(t, same) != canonical(t, request) {
			t.Errorf("%s\n got %s\nwant %s", same, canonical(t, same), canonical(t, request))
		}
	}
	for _, different := range []string{
		`{"model":"m","messages":[{"role":"user","content":"a/b é"}],"temperature":1.0}`,
		`{"model":"m","messages":[{"role":"user","content":"a/b é"}],"temperature":"1"}`,
		`{"model":"m","messages":[{"role":"user","content":"A/b é"}],"temperature":1}`,
		`{"model":"m","messages":[{"role":"user","content":"a/b é"},{"role":"user","content":"a/b é"}],"temperature":1}`,
		`{"model":"m","messages":[{"role":"user","content":"a/b é"}],"temperature":1,"stream":null}`,
		`{"model":"m","messages":[{"content":"user","role":"a/b é"}],"temperature":1}`,
	} {
		if canonical(t, different) == canonical(t, request) {
			t.Errorf("%s shares a cache entry with %s", different, request)
		}
	}
	if first, second := canonical(t, `{"a":[1,2]}`), canonical(t, `{"a":[12]}`); first == second {
		t.Errorf("two elements and their concatenation share the entry %s", first)
	}
}

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
		"exact, widest":     {Mode: "exact", TTLSeconds: maxTTLSeconds, MaxEntryBytes: maxEntryBytes, Scope: "route", Streaming: "bypass", Operations: []string{"embeddings", "rerank"}, CountHitsAgainst: []string{"tokens", "requests", "budgets"}},
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
		"a TTL over a week":                   {Mode: "exact", TTLSeconds: maxTTLSeconds + 1},
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

func TestValidateVectorRefusesVectorsWithoutADirection(t *testing.T) {
	if err := ValidateVector([]float32{0, -0.5, 3}); err != nil {
		t.Error(err)
	}
	for name, vector := range map[string][]float32{
		"empty":     {},
		"zero":      {0, 0, 0},
		"NaN":       {1, float32(math.NaN())},
		"infinite":  {1, float32(math.Inf(-1))},
		"oversized": append(make([]float32, 65536), 1),
	} {
		if err := ValidateVector(vector); err == nil {
			t.Errorf("a %s vector was accepted", name)
		}
	}
}

func TestSearchCandidatesAcceptsRESP3AndLegacyReplies(t *testing.T) {
	t.Parallel()
	store := Store{Installation: "fixture"}
	key := store.prefix() + "entry:match"
	fields := map[string]any{"__vector_score": "0.1"}
	for name, reply := range map[string]any{
		"RESP3":  map[string]any{"total_results": int64(1), "results": []any{map[string]any{"id": key, "extra_attributes": fields}}},
		"flat":   []any{int64(1), key, fields},
		"mapped": []any{int64(1), map[string]any{key: fields}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := store.searchCandidates(reply, Policy{SimilarityThreshold: 0.8})
			if err != nil || len(got) != 1 || got[0].key != key || math.Abs(got[0].score-0.9) > 1e-9 {
				t.Fatalf("matching search result = %v, %v", got, err)
			}
			got, err = store.searchCandidates(reply, Policy{SimilarityThreshold: 0.95})
			if err != nil || len(got) != 0 {
				t.Fatalf("below-threshold search result = %v, %v", got, err)
			}
		})
	}
	for name, reply := range map[string]any{
		"empty":         map[string]any{"total_results": int64(0), "results": []any{}},
		"wrong results": map[string]any{"results": "invalid"},
		"wrong row":     map[string]any{"results": []any{"invalid"}},
		"foreign key":   map[string]any{"results": []any{map[string]any{"id": "another:entry", "extra_attributes": fields}}},
		"missing score": map[string]any{"results": []any{map[string]any{"id": key, "extra_attributes": map[string]any{}}}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := store.searchCandidates(reply, Policy{})
			if name == "empty" {
				if err != nil || len(got) != 0 {
					t.Fatalf("empty result = %v, %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("malformed result accepted: %v", got)
			}
		})
	}
}
