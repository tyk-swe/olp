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
	"github.com/tyk-swe/olp/internal/operations/ocr"
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

func TestBraveProfileProbesOverGetQuery(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/res/v1/web/search" {
			t.Errorf("unexpected probe %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("count") != "1" || r.URL.Query().Get("q") == "" {
			t.Errorf("probe query: %v", r.URL.Query())
		}
		if r.Header.Get("X-Subscription-Token") != "secret" {
			t.Errorf("probe credential: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"search","query":{"original":"probe"},"web":{"type":"search","results":[{"title":"t","url":"https://a","description":"d"}],"family_friendly":true}}`)
	}))
	defer server.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	cfg := Configuration{ProviderID: "provider", Kind: KindOpenAICompatible, ProfileID: "brave-search", ProfileRevision: "1", AuthMode: AuthAPIKey, Endpoint: new(server.URL), Options: Options{VendorID: new("brave")}, ProbeModels: []string{"brave-model"}}
	cfg.Normalize()
	if tuple := defaultProbeTuple(&cfg); tuple != (CapabilityInput{Operation: "search", Surface: "openai", Mode: ModeUnary}) {
		t.Fatalf("probe tuple = %+v", tuple)
	}
	models, err := New(nil, policy, nil).listModelFacts(context.Background(), &cfg, []byte("secret"))
	if err != nil || len(models) != 1 || models[0].Name != "brave-model" || calls != 1 {
		t.Fatalf("probe: %+v %v calls=%d", models, err, calls)
	}
}

// A route left with less than two probe budgets still reserves about half
// of what remains, so a late probe cannot run up to the route deadline,
// and an unbounded caller adds no fixed cap over its per-call timeouts.

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

func TestAzureOperationProbePolls(t *testing.T) {
	submits, polls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Ocp-Apim-Subscription-Key") != "secret" {
			t.Errorf("credential header on %s %s: %v", r.Method, r.URL.Path, r.Header)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/documentintelligence/documentModels/azure-model:analyze":
			submits++
			if r.URL.Query().Get("api-version") != ocr.AzureAPIVersion || r.URL.Query().Get("outputContentFormat") != "markdown" {
				t.Errorf("submit query: %s", r.URL.RawQuery)
			}
			w.Header().Set("Operation-Location", "http://"+r.Host+"/documentintelligence/documentModels/azure-model/analyzeResults/result-1?api-version=2024-11-30")
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/documentintelligence/documentModels/azure-model/analyzeResults/result-1":
			polls++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"azure-model","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"probe","pages":[{"pageNumber":1}]}}`)
		default:
			t.Errorf("unexpected probe %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	cfg := Configuration{ProviderID: "provider", Kind: KindOpenAICompatible, ProfileID: "azure-document-intelligence-ocr", ProfileRevision: "1", AuthMode: AuthAPIKey, Endpoint: new(server.URL), Options: Options{VendorID: new("azure-document-intelligence")}, ProbeModels: []string{"azure-model"}}
	cfg.Normalize()
	models, err := New(nil, policy, nil).listModelFacts(context.Background(), &cfg, []byte("secret"))
	if err != nil || len(models) != 1 || models[0].Name != "azure-model" || submits != 1 || polls != 1 {
		t.Fatalf("probe: %+v %v submits=%d polls=%d", models, err, submits, polls)
	}
}
