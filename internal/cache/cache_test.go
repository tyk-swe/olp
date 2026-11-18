package cache

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/secrets"
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

type fakeCommander struct {
	values map[string]string
	hashes map[string]map[string]string
	resp3  bool
}

func (f *fakeCommander) Do(_ context.Context, a ...string) (any, error) {
	switch a[0] {
	case "EVAL":
		if a[1] == lookupScript {
			return f.lookup(a)
		}
		if a[1] == putScript {
			hash := map[string]string{"sealed": a[4], "bucket": a[6]}
			if a[7] != "" {
				hash["vector"] = a[7]
			}
			f.hashes[a[3]] = hash
			return int64(1), nil
		}
		return nil, errors.New("unsupported script")
	case "FT.CREATE":
		return "OK", nil
	case "FT.SEARCH":
		start := strings.Index(a[2], "{")
		end := strings.Index(a[2], "}")
		if start < 0 || end < 0 || end < start {
			return nil, errors.New("unsupported query")
		}
		bucket := a[2][start+1 : end]
		keys := []string{}
		for key, hash := range f.hashes {
			if hash["bucket"] == bucket && hash["vector"] != "" {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		if f.resp3 {
			results := make([]any, 0, len(keys))
			for _, key := range keys {
				results = append(results, map[string]any{"id": key, "extra_attributes": map[string]any{"__vector_score": "0"}})
			}
			return map[string]any{"total_results": int64(len(results)), "results": results}, nil
		}
		reply := []any{int64(len(keys))}
		for _, key := range keys {
			reply = append(reply, key, []any{"__vector_score", "0"})
		}
		return reply, nil
	case "HGET":
		if hash, ok := f.hashes[a[1]]; ok {
			if value, ok := hash[a[2]]; ok {
				return value, nil
			}
		}
		return nil, nil
	case "SET":
		f.values[a[1]] = a[2]
		return "OK", nil
	case "GET":
		if value, ok := f.values[a[1]]; ok {
			return value, nil
		}
		return nil, nil
	case "MGET":
		reply := make([]any, len(a)-1)
		for i, key := range a[1:] {
			if value, ok := f.values[key]; ok {
				reply[i] = value
			}
		}
		return reply, nil
	}
	return nil, errors.New("unsupported command")
}

func (f *fakeCommander) lookup(a []string) (any, error) {
	n, err := strconv.Atoi(a[2])
	if err != nil {
		return nil, err
	}
	keys := a[3 : 3+n]
	argv := a[3+n:]
	generations := make([]string, n)
	for i, key := range keys {
		if value, ok := f.values[key]; ok {
			generations[i] = value
		} else {
			f.values[key] = argv[i]
			generations[i] = argv[i]
		}
	}
	suffix := strings.Join(generations, ":")
	key := argv[n] + ":" + suffix
	sum := sha1.Sum(fmt.Appendf(nil, "%s:%s", argv[n+1], suffix))
	bucket := hex.EncodeToString(sum[:])
	entry := ""
	if argv[n+2] == "1" {
		entry = f.hashes[key]["sealed"]
	}
	reply := []any{key, bucket, entry}
	for _, generation := range generations {
		reply = append(reply, generation)
	}
	return reply, nil
}

func TestNearestMissesWhenPurgeFollowsTicketResolution(t *testing.T) {
	ring, err := secrets.ParseRingWithUnwrapper(context.Background(), fmt.Appendf(nil, `{"active_version":1,"keys":[{"version":1,"key":%q}]}`, strings.Repeat("01", 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeCommander{values: map[string]string{}, hashes: map[string]map[string]string{}}
	store := &Store{
		Client:       client,
		Keys:         ring,
		Auth:         secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), "installation"),
		Installation: "installation",
	}
	space := Space{Project: "project", Route: "route", NamedRoute: "named", Scope: "key", Fingerprint: "caller"}
	policy := Policy{Mode: "semantic", EmbeddingRoute: "embeddings", AllowUnsealedVectors: true, TTLSeconds: 60}
	vector := []float32{1, 0}
	stored, err := store.Begin(t.Context(), space, []byte("request"))
	if err != nil {
		t.Fatal(err)
	}
	stored.VectorSpace = "embed"
	if err := store.Put(t.Context(), stored, Entry{Body: []byte("answer")}, policy, Controls{}, vector); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.Begin(t.Context(), space, []byte("similar request"))
	if err != nil {
		t.Fatal(err)
	}
	ticket.VectorSpace = "embed"
	for _, resp3 := range []bool{false, true} {
		client.resp3 = resp3
		entry, _, err := store.Nearest(t.Context(), ticket, vector, policy, Controls{})
		if err != nil || entry == nil || string(entry.Body) != "answer" {
			t.Fatalf("got entry %v and %v before any purge (RESP3=%v), want the stored answer", entry, err, resp3)
		}
	}
	bare := Ticket{Key: ticket.Key, Bucket: ticket.Bucket, VectorSpace: ticket.VectorSpace}
	if entry, _, err := store.Nearest(t.Context(), bare, vector, policy, Controls{}); err != nil || entry != nil {
		t.Errorf("got entry %v and %v for a ticket without generation data, want a miss", entry, err)
	}
	if err := store.Purge(t.Context(), "project", space.Project); err != nil {
		t.Fatal(err)
	}
	entry, _, err := store.Nearest(t.Context(), ticket, vector, policy, Controls{})
	if err != nil || entry != nil {
		t.Fatalf("got entry %v and %v after purging the project, want a miss", entry, err)
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
