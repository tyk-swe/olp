package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func ocrSearchProviders(s *routing.Snapshot, vendor, profile, operation string) {
	for id, p := range s.Providers {
		p.Kind, p.VendorID, p.ProfileID = "openai_compatible", vendor, profile
		p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: operation, Surface: "openai", Mode: "unary"}}
		s.Providers[id] = p
	}
}

const ocrBody = `{"model":"` + routeSlug + `","document":{"type":"document_url","document_url":"https://example.com/report.pdf"}}`

func TestOCRNativeRoundTrip(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"ocr"}
		s.Routes[routeSlug] = route
		ocrSearchProviders(s, "mistral", "mistral-ocr", "ocr")
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a/v1/ocr" {
			t.Errorf("ocr addressed at %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+secretA {
			t.Errorf("credential: %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if json.Unmarshal(body, &req) != nil || req["model"] != modelA {
			t.Errorf("upstream body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"mistral-ocr-latest","pages":[{"index":0,"markdown":"# Report"}],"usage_info":{"pages_processed":1,"doc_size_bytes":9}}`)
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(ocrBody), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 || out["model"] != routeSlug {
		t.Fatalf("ocr: %d %s calls=%d", resp.StatusCode, body, h.mock.count("a"))
	}
}

func TestSearchRequiresRouteHeader(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"search"}
		s.Routes[routeSlug] = route
		ocrSearchProviders(s, "perplexity", "perplexity-search", "search")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(`{"query":"q"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "missing_required_parameter") || h.mock.count("a")+h.mock.count("b") != 0 {
		t.Fatalf("missing route header: %d %s", resp.StatusCode, body)
	}
}

func TestSearchTransformedTargets(t *testing.T) {
	for _, tc := range []struct {
		vendor, profile, response, wantSnippet, wantHeader string
	}{
		{"tavily", "tavily-search", `{"query":"q","results":[{"title":"t","url":"https://a","content":"c"}],"response_time":1.0,"request_id":"r"}`, "c", "Bearer " + secretA},
		{"exa", "exa-search", `{"requestId":"r","results":[{"title":"t","url":"https://a","publishedDate":"2026-01-02T03:04:05Z","highlights":["h1","h2"]}],"costDollars":0.01}`, "h1\n...\nh2", "Bearer " + secretA},
		{"parallel", "parallel-search", `{"search_id":"upstream","results":[{"url":"https://a","title":"t","publish_date":"2026-01-02","excerpts":["e1","e2"]}],"session_id":"s"}`, "e1\n...\ne2", ""},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			h := strictHarness(t, func(s *routing.Snapshot) {
				route := s.Routes[routeSlug]
				route.Operations = []string{"search"}
				route.Fidelity.Mode = routing.FidelityTransformed
				s.Routes[routeSlug] = route
				ocrSearchProviders(s, tc.vendor, tc.profile, "search")
			})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/a/v1/search" {
					t.Errorf("addressed at %s", r.URL.Path)
				}
				body, _ := io.ReadAll(r.Body)
				var req map[string]any
				_ = json.Unmarshal(body, &req)
				if req["model"] != nil || req["query"] != "q" && req["search_queries"] == nil {
					t.Errorf("upstream body: %s", body)
				}
				if tc.vendor == "parallel" {
					if r.Header.Get("X-Api-Key") != secretA || r.Header.Get("Authorization") != "" {
						t.Errorf("credential headers: %v", r.Header)
					}
				} else if r.Header.Get("Authorization") != tc.wantHeader {
					t.Errorf("credential: %q", r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.response)
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(`{"query":"q"}`), map[string]string{"X-OLP-Route": routeSlug})
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var out struct {
				ID      string `json:"id"`
				Results []struct {
					Title, URL, Snippet string
					Date                *string `json:"date"`
				} `json:"results"`
			}
			if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil {
				t.Fatalf("%s: %d %s", tc.vendor, resp.StatusCode, body)
			}
			if !strings.HasPrefix(out.ID, "search_") || out.ID == "upstream" || out.ID == "r" {
				t.Fatalf("%s upstream id leaked: %q", tc.vendor, out.ID)
			}
			if len(out.Results) != 1 && tc.vendor != "parallel" || out.Results[0].Snippet != tc.wantSnippet {
				t.Fatalf("%s results: %+v", tc.vendor, out.Results)
			}
			if tc.vendor == "parallel" && len(out.Results) != 1 {
				t.Fatalf("parallel results: %+v", out.Results)
			}
		})
	}
}

func TestSearchBraveTransformedTarget(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"search"}
		route.Fidelity.Mode = routing.FidelityTransformed
		s.Routes[routeSlug] = route
		ocrSearchProviders(s, "brave", "brave-search", "search")
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method: %s", r.Method)
		}
		if r.URL.Path != "/a/v1/res/v1/web/search" {
			t.Errorf("addressed at %s", r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("q") != "q" || query.Get("count") != "2" || query.Get("result_filter") != "web" || query.Get("freshness") != "2026-01-02to2026-03-04" {
			t.Errorf("query: %v", query)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) != 0 {
			t.Errorf("get body: %s", body)
		}
		if r.Header.Get("X-Subscription-Token") != secretA || r.Header.Get("Authorization") != "" {
			t.Errorf("credential headers: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"search","query":{"original":"q"},"web":{"type":"search","results":[{"title":"t","url":"https://a","description":"d","page_age":"2026-01-02T03:04:05Z"}],"family_friendly":true}}`)
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(`{"query":"q","max_results":2,"search_after_date_filter":"01/02/2026","search_before_date_filter":"03/04/2026"}`), map[string]string{"X-OLP-Route": routeSlug})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		ID      string `json:"id"`
		Results []struct {
			Title, URL, Snippet string
			Date                *string `json:"date"`
		} `json:"results"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil {
		t.Fatalf("brave: %d %s", resp.StatusCode, body)
	}
	if !strings.HasPrefix(out.ID, "search_") || len(out.Results) != 1 || out.Results[0].Snippet != "d" || out.Results[0].Date == nil || *out.Results[0].Date != "2026-01-02" {
		t.Fatalf("brave projection: %+v", out)
	}
	if h.mock.count("a") != 1 {
		t.Fatalf("upstream calls: %d", h.mock.count("a"))
	}
}

func TestSearchBraveNativeStrict(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"search"}
		s.Routes[routeSlug] = route
		for id, p := range s.Providers {
			p.Kind, p.VendorID, p.ProfileID = "openai_compatible", "brave", "brave-search"
			p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: "search", Surface: "native", Mode: "unary"}}
			s.Providers[id] = p
		}
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Query().Get("offset") != "1" || r.URL.Query().Get("count") != "3" {
			t.Errorf("native wire: %s %v", r.Method, r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"type":"search","query":{"original":"q"},"web":{"type":"search","results":[{"title":"t","url":"https://a","description":"d"}],"family_friendly":true}}`)
	})
	resp := h.do(t.Context(), http.MethodPost, "/native/brave-search/models/"+routeSlug, fullKey, []byte(`{"q":"q","count":3,"offset":1}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"description":"d"`) {
		t.Fatalf("brave native: %d %s", resp.StatusCode, body)
	}
}

func TestSearchBraveRefusalsDispatchNothing(t *testing.T) {
	for _, body := range []string{
		`{"query":"q","search_domain_filter":["a.com"]}`,
		`{"query":"q","search_after_date_filter":"01/02/2026"}`,
		`{"query":"q","search_recency_filter":"week"}`,
		`{"query":"q","country":"US"}`,
	} {
		h := strictHarness(t, func(s *routing.Snapshot) {
			route := s.Routes[routeSlug]
			route.Operations = []string{"search"}
			route.Fidelity.Mode = routing.FidelityTransformed
			s.Routes[routeSlug] = route
			ocrSearchProviders(s, "brave", "brave-search", "search")
		})
		resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(body), map[string]string{"X-OLP-Route": routeSlug})
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 || h.mock.count("a")+h.mock.count("b") != 0 {
			t.Fatalf("refusal dispatched: %d %s", resp.StatusCode, out)
		}
	}
}

func TestSearchMappingRefusalsDispatchNothing(t *testing.T) {
	for _, body := range []string{
		`{"query":"q","country":"US"}`,
		`{"query":"q","search_type":"fast"}`,
		`{"query":"q","max_results":21}`,
		`{"query":"q","model":"` + routeSlug + `"}`,
	} {
		h := strictHarness(t, func(s *routing.Snapshot) {
			route := s.Routes[routeSlug]
			route.Operations = []string{"search"}
			route.Fidelity.Mode = routing.FidelityTransformed
			s.Routes[routeSlug] = route
			ocrSearchProviders(s, "tavily", "tavily-search", "search")
		})
		resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(body), map[string]string{"X-OLP-Route": routeSlug})
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 || h.mock.count("a")+h.mock.count("b") != 0 {
			t.Fatalf("refusal dispatched: %d %s", resp.StatusCode, out)
		}
	}
}

func TestOCRRefusesUnboundedWorkBudgets(t *testing.T) {
	for _, scope := range []string{"key", "slot", "provider"} {
		t.Run(scope, func(t *testing.T) {
			limit := int64(10000)
			h := strictHarness(t, func(s *routing.Snapshot) {
				route := s.Routes[routeSlug]
				route.Operations = []string{"ocr"}
				s.Routes[routeSlug] = route
				for id, p := range s.Providers {
					p.Kind, p.VendorID, p.ProfileID = "openai_compatible", "mistral", "mistral-ocr"
					p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: "ocr", Surface: "openai", Mode: "unary"}}
					if scope == "slot" {
						p.Slots[0].TokensPerMinute = &limit
					}
					if scope == "provider" {
						p.Limits = &routing.Limits{TokensPerMinute: &limit}
					}
					s.Providers[id] = p
				}
			})
			if scope == "key" {
				key := h.rt.keys[fullKey]
				key.Policy.TokensPerMinute = &limit
				h.rt.keys[fullKey] = key
			}
			resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(ocrBody), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 400 || !strings.Contains(string(body), "unbounded_work_budget") || h.mock.count("a")+h.mock.count("b") != 0 {
				t.Fatalf("unbounded ocr: %d %s", resp.StatusCode, body)
			}
		})
	}
}

func TestCallerPaidOCRExemptsCostCaps(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		r := s.Routes[routeSlug]
		r.Operations = []string{"ocr"}
		r.CallerCostExempt = true
		r.Budget = &routing.CostLimits{DailyCostLimit: new("1")}
		s.Routes[routeSlug] = r
		for id, p := range s.Providers {
			p.Kind, p.VendorID, p.ProfileID, p.CredentialSource = "openai_compatible", "mistral", "mistral-ocr", "caller"
			p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: "ocr", Surface: "openai", Mode: "unary"}}
			p.Limits = &routing.Limits{Supply: routing.Supply{DailyCostLimit: new("1")}}
			p.Slots[0].Supply.DailyCostLimit = new("1")
			s.Providers[id] = p
		}
	})
	key := h.rt.keys[fullKey]
	key.Policy.DailyCostLimit = new("1")
	h.rt.keys[fullKey] = key
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"mistral-ocr-latest","pages":[{"index":0,"markdown":"# Report"}],"usage_info":{"pages_processed":1}}`)
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(ocrBody), map[string]string{callerCredentialHeader: "caller-test-secret"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("caller-paid ocr refused: %d %s", resp.StatusCode, body)
	}
}

func TestSearchPerRequestBound(t *testing.T) {
	codec, ok := operationregistry.Lookup("perplexity-search")
	if !ok {
		t.Fatal("perplexity-search not registered")
	}
	x := &call{unary: &unaryCall{dialect: codec}}
	price := &usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai_compatible", Model: modelA, Operation: "search", PerRequest: new("0.01")}}
	bound, ok := x.targetCostBound(routing.Provider{}, routing.Target{}, price, nil)
	if !ok || bound.String() != "0.01" {
		t.Fatalf("search bound: %s ok=%v", bound.String(), ok)
	}
	ocr, _ := operationregistry.Lookup("mistral-ocr")
	x = &call{unary: &unaryCall{dialect: ocr}}
	if _, ok := x.targetCostBound(routing.Provider{}, routing.Target{}, price, nil); ok {
		t.Fatal("unbounded ocr admitted a bound")
	}
}

func TestOCRInputPolicyRefusalAndNoCache(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"ocr"}
		route.ContentPolicy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "secret", Phase: "input", Action: contentpolicy.ActionBlock, Pattern: "secret"}}}
		route.Cache = &cache.Policy{Mode: "exact"}
		s.Routes[routeSlug] = route
		ocrSearchProviders(s, "mistral", "mistral-ocr", "ocr")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(ocrBody), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 || !strings.Contains(string(body), "policy_conflict") || h.mock.count("a") != 0 {
		t.Fatalf("input policy: %d %s calls=%d", resp.StatusCode, body, h.mock.count("a"))
	}
}

func vertexOCRProviders(s *routing.Snapshot) {
	for id, p := range s.Providers {
		p.Kind, p.VendorID, p.ProfileID, p.ProfileRevision = "vertex_ai", "google-vertex", "vertex-document-processing-ocr", "1"
		p.AuthMode, p.CloudRegion, p.CloudProject = "service_account", "us", "example-project"
		model := p.Capabilities[0].Model
		p.Capabilities = []routing.Capability{
			{Model: model, Operation: "ocr", Surface: "openai", Mode: "unary"},
			{Model: model, Operation: "ocr", Surface: "native", Mode: "unary"},
		}
		s.Providers[id] = p
	}
}

func TestVertexOCRMappingRefusalsDispatchNothing(t *testing.T) {
	for _, body := range []string{
		`{"model":"` + routeSlug + `","document":{"type":"document_url","document_url":"https://example.com/report.pdf"}}`,
		`{"model":"` + routeSlug + `","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA"},"table_format":"markdown"}`,
		`{"model":"` + routeSlug + `","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA"},"extract_header":true}`,
	} {
		h := strictHarness(t, func(s *routing.Snapshot) {
			route := s.Routes[routeSlug]
			route.Operations = []string{"ocr"}
			route.Fidelity.Mode = routing.FidelityTransformed
			s.Routes[routeSlug] = route
			vertexOCRProviders(s)
		})
		resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(body), nil)
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 || h.mock.count("a")+h.mock.count("b") != 0 {
			t.Fatalf("unmappable vertex ocr dispatched: %d %s", resp.StatusCode, out)
		}
	}
}

func TestVertexOCRDocumentLocationRefusedWithoutPrefixGrant(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"ocr"}
		s.Routes[routeSlug] = route
		vertexOCRProviders(s)
	})
	body := `{"gcsDocument":{"gcsUri":"gs://docs-bucket/reports/a.pdf","mimeType":"application/pdf"}}`
	resp := h.do(t.Context(), http.MethodPost, "/native/vertex-document-processing-ocr/models/"+routeSlug, fullKey, []byte(body), nil)
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 || !strings.Contains(string(out), "storage_scope") || h.mock.count("a")+h.mock.count("b") != 0 {
		t.Fatalf("ungranted document location: %d %s calls=%d", resp.StatusCode, out, h.mock.count("a")+h.mock.count("b"))
	}
}

func TestSearchMutatingGuardrailRefused(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"search"}
		route.Fidelity.Mode = routing.FidelityTransformed
		s.Routes[routeSlug] = route
		ocrSearchProviders(s, "perplexity", "perplexity-search", "search")
	})
	installTestGuardrail(t, h, guardrails.Input, guardrails.Redact, "builtin.regex", `{"rules":[{"id":"secret","pattern":"secret"}]}`)
	resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(`{"query":"secret"}`), map[string]string{"X-OLP-Route": routeSlug})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "guardrail_surface_unavailable") || h.mock.count("a")+h.mock.count("b") != 0 {
		t.Fatalf("mutating search guardrail: %d %s", resp.StatusCode, body)
	}
}
