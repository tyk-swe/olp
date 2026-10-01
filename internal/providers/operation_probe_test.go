package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestOperationProfileWithoutDiscoveryProbesItsOwnOperation(t *testing.T) {
	for _, tc := range []struct {
		profile, operation, path, body string
	}{
		{"cohere-embed-v2", "embeddings", "/v2/embed", `{"id":"p","embeddings":{"float":[[1,-2]]},"texts":["embedding probe"],"meta":{"billed_units":{"input_tokens":2}}}`},
		{"cohere-rerank-v2", "rerank", "/v2/rerank", `{"id":"p","results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}],"meta":{"billed_units":{"search_units":1}}}`},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != tc.path {
					t.Errorf("unexpected probe %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			cfg := Configuration{ProviderID: "provider", Kind: KindOpenAICompatible, ProfileID: tc.profile, ProfileRevision: "1", AuthMode: AuthAPIKey, Endpoint: new(server.URL + "/v2"), Options: Options{VendorID: new("cohere-native-v2")}, ProbeModels: []string{"cohere-model"}}
			cfg.Normalize()
			if tuple := defaultProbeTuple(&cfg); tuple != (CapabilityInput{Operation: tc.operation, Surface: "native", Mode: ModeUnary}) {
				t.Fatalf("probe tuple = %+v", tuple)
			}
			models, err := New(nil, policy, nil).listModelFacts(context.Background(), &cfg, []byte("secret"))
			if err != nil || len(models) != 1 || models[0].Name != "cohere-model" || calls != 1 {
				t.Fatalf("probe: %+v %v calls=%d", models, err, calls)
			}
		})
	}
}

func TestDefaultProbeTupleKeepsGenerationForGenerationProfiles(t *testing.T) {
	cfg := Configuration{Kind: KindOpenAI, AuthMode: AuthAPIKey}
	cfg.Normalize()
	if tuple := defaultProbeTuple(&cfg); tuple != (CapabilityInput{Operation: "generation", Surface: "openai", Mode: ModeUnary}) {
		t.Fatalf("probe tuple = %+v", tuple)
	}
}

func TestProbeBudgetReservesPersistenceTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*probeTimeout)
	defer cancel()
	outer, _ := ctx.Deadline()
	probes, done := probeBudget(ctx)
	defer done()
	inner, ok := probes.Deadline()
	if !ok || !inner.Equal(outer.Add(-probeTimeout)) {
		t.Fatalf("probe deadline %v, route deadline %v", inner, outer)
	}
	// A route without room for a reserve keeps its own deadline, and an
	// unbounded caller adds no fixed cap over its per-call probe timeouts.
	short, cancelShort := context.WithTimeout(context.Background(), probeTimeout)
	defer cancelShort()
	shortDeadline, _ := short.Deadline()
	probes, done = probeBudget(short)
	defer done()
	if inner, _ := probes.Deadline(); !inner.Equal(shortDeadline) {
		t.Fatalf("short route probe deadline %v, want %v", inner, shortDeadline)
	}
	probes, done = probeBudget(context.Background())
	defer done()
	if _, ok := probes.Deadline(); ok {
		t.Fatal("unbounded caller received a fixed probe cap")
	}
}

func TestDiscoveredFactsNeverInvalidateConfiguration(t *testing.T) {
	cfg := Configuration{Kind: KindGemini, AuthMode: AuthAPIKey}
	cfg.Normalize()
	cfg.Options.Models["d"] = json.RawMessage(`{"max_output_tokens":512}`)
	fact := func(v string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"context_length": json.RawMessage(v), "source": json.RawMessage(`"https://example.test"`)}
	}
	mergeDiscoveredFacts(&cfg, "a", fact(`0`))
	mergeDiscoveredFacts(&cfg, "b", fact(`"128k"`))
	mergeDiscoveredFacts(&cfg, "c", map[string]json.RawMessage{"input_modalities": json.RawMessage(`"text"`)})
	mergeDiscoveredFacts(&cfg, "d", fact(`8192`))
	for _, name := range []string{"a", "b", "c"} {
		if _, ok := cfg.Options.Models[name]; ok {
			t.Fatalf("invalid facts stored for %s: %s", name, cfg.Options.Models[name])
		}
	}
	var d map[string]json.RawMessage
	if err := json.Unmarshal(cfg.Options.Models["d"], &d); err != nil || string(d["context_length"]) != "8192" || string(d["max_output_tokens"]) != "512" {
		t.Fatalf("merged facts: %s %v", cfg.Options.Models["d"], err)
	}
	if err := cfg.Validate(&egress.Policy{}); err != nil {
		t.Fatalf("configuration invalid after discovery: %v", err)
	}

	full := Configuration{Kind: KindGemini, AuthMode: AuthAPIKey}
	full.Normalize()
	for i := range 2000 {
		full.Options.Models[fmt.Sprintf("m%d", i)] = json.RawMessage(`{"context_length":1}`)
	}
	mergeDiscoveredFacts(&full, "new", fact(`8192`))
	mergeDiscoveredFacts(&full, "m0", fact(`8192`))
	if len(full.Options.Models) != 2000 {
		t.Fatalf("metadata entries = %d", len(full.Options.Models))
	}
	if err := full.Validate(&egress.Policy{}); err != nil {
		t.Fatalf("configuration invalid after discovery: %v", err)
	}
}
