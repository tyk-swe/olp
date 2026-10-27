package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

const azureSubmitPath = "/a/v1/documentintelligence/documentModels/model-a:analyze"
const azurePollPath = "/a/v1/documentintelligence/documentModels/model-a/analyzeResults/result-1"
const azureTwoPages = `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"model-a","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# One\n\n# Two","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":5}]},{"pageNumber":2,"spans":[{"offset":7,"length":5}]}]}}`

type azureMock struct {
	submits, polls, deletes atomic.Int32
	poll                    func(n int32, w http.ResponseWriter, r *http.Request)
	foreign                 bool
}

func (m *azureMock) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Ocp-Apim-Subscription-Key") != secretA || r.Header.Get("Authorization") != "" {
			t.Errorf("credential headers on %s %s: %v", r.Method, r.URL.Path, r.Header)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == azureSubmitPath:
			m.submits.Add(1)
			q := r.URL.Query()
			if q.Get("api-version") != "2024-11-30" || q.Get("outputContentFormat") != "markdown" {
				t.Errorf("submit query: %s", r.URL.RawQuery)
			}
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			if json.Unmarshal(body, &req) != nil || len(req) != 1 {
				t.Errorf("submit body: %s", body)
			}
			loc := "http://" + r.Host + azurePollPath + "?api-version=2024-11-30"
			if m.foreign {
				loc = "https://evil.example.net" + azurePollPath + "?api-version=2024-11-30"
			}
			w.Header().Set("Operation-Location", loc)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == azurePollPath:
			m.poll(m.polls.Add(1), w, r)
		case r.Method == http.MethodDelete && r.URL.Path == azurePollPath:
			m.deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func azureHarness(t *testing.T, transformed bool, mock *azureMock) *harness {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"ocr"}
		if transformed {
			route.Fidelity.Mode = routing.FidelityTransformed
		}
		s.Routes[routeSlug] = route
		ocrSearchProviders(s, "azure-document-intelligence", "azure-document-intelligence-ocr", "ocr")
		if !transformed {
			for id, p := range s.Providers {
				p.Capabilities = []routing.Capability{{Model: p.Capabilities[0].Model, Operation: "ocr", Surface: "native", Mode: "unary"}}
				s.Providers[id] = p
			}
		}
	})
	h.mock.set("a", mock.handler(t))
	return h
}

func TestAzureOCRTransformedPolling(t *testing.T) {
	mock := &azureMock{poll: func(n int32, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		if n == 1 {
			io.WriteString(w, `{"status":"running"}`)
			return
		}
		io.WriteString(w, azureTwoPages)
	}}
	h := azureHarness(t, true, mock)
	resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(ocrBody), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Model string `json:"model"`
		Pages []struct {
			Index    int    `json:"index"`
			Markdown string `json:"markdown"`
		} `json:"pages"`
		UsageInfo struct {
			PagesProcessed int `json:"pages_processed"`
		} `json:"usage_info"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil {
		t.Fatalf("azure ocr: %d %s", resp.StatusCode, body)
	}
	if out.Model != routeSlug || len(out.Pages) != 2 || out.Pages[0].Markdown != "# One" || out.Pages[1].Markdown != "# Two" || out.UsageInfo.PagesProcessed != 2 {
		t.Fatalf("projected: %+v", out)
	}
	if mock.submits.Load() != 1 || mock.polls.Load() != 2 {
		t.Fatalf("submits=%d polls=%d", mock.submits.Load(), mock.polls.Load())
	}
	fact := h.sink.last(t).Attempts[0]
	if !fact.UsageObserved || fact.BillingUncertain || fact.Usage == nil || fact.Usage.MediaUnits == nil || *fact.Usage.MediaUnits != "2" {
		t.Fatalf("fact: observed=%v uncertain=%v usage=%+v", fact.UsageObserved, fact.BillingUncertain, fact.Usage)
	}
}

func TestAzureOCRPollNeverResubmits(t *testing.T) {
	mock := &azureMock{poll: func(n int32, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "0")
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, azureTwoPages)
	}}
	h := azureHarness(t, true, mock)
	resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(ocrBody), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || mock.submits.Load() != 1 || mock.polls.Load() != 2 {
		t.Fatalf("ambiguous poll: %d %s submits=%d polls=%d", resp.StatusCode, body, mock.submits.Load(), mock.polls.Load())
	}
}

func TestAzureOCRMappingRefusalsDispatchNothing(t *testing.T) {
	for _, body := range []string{
		`{"model":"` + routeSlug + `","document":{"type":"document_url","document_url":"https://x"},"table_format":"html"}`,
		`{"model":"` + routeSlug + `","document":{"type":"document_url","document_url":"https://x"},"include_image_base64":true}`,
	} {
		mock := &azureMock{poll: func(n int32, w http.ResponseWriter, r *http.Request) {}}
		h := azureHarness(t, true, mock)
		resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(body), nil)
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 || mock.submits.Load() != 0 {
			t.Fatalf("refusal dispatched: %d %s submits=%d", resp.StatusCode, out, mock.submits.Load())
		}
	}
}

func TestAzureOCRMissingUnitsBillingUncertain(t *testing.T) {
	mock := &azureMock{poll: func(n int32, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"model-a","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# Only"}}`)
	}}
	h := azureHarness(t, true, mock)
	resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(ocrBody), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "usage_info") {
		t.Fatalf("missing units: %d %s", resp.StatusCode, body)
	}
	fact := h.sink.last(t).Attempts[0]
	if fact.UsageObserved || !fact.BillingUncertain || fact.UsageComplete {
		t.Fatalf("fact: observed=%v uncertain=%v complete=%v", fact.UsageObserved, fact.BillingUncertain, fact.UsageComplete)
	}
}

func TestAzureOCRPollForeignLocationRefused(t *testing.T) {
	mock := &azureMock{foreign: true, poll: func(n int32, w http.ResponseWriter, r *http.Request) {}}
	h := azureHarness(t, true, mock)
	resp := h.do(t.Context(), http.MethodPost, "/v1/ocr", fullKey, []byte(ocrBody), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 || mock.submits.Load() != 1 || mock.polls.Load() != 0 {
		t.Fatalf("foreign location: %d %s submits=%d polls=%d", resp.StatusCode, body, mock.submits.Load(), mock.polls.Load())
	}
}

func TestAzureOCRCancellationDeletesResult(t *testing.T) {
	polled := make(chan struct{}, 1)
	mock := &azureMock{poll: func(n int32, w http.ResponseWriter, r *http.Request) {
		select {
		case polled <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		io.WriteString(w, `{"status":"running"}`)
	}}
	h := azureHarness(t, true, mock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/ocr", bytes.NewReader([]byte(ocrBody)))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+fullKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-polled:
	case <-time.After(10 * time.Second):
		t.Fatal("poll never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("request never ended after cancel")
	}
	deadline := time.Now().Add(5 * time.Second)
	for mock.deletes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if mock.submits.Load() != 1 || mock.deletes.Load() != 1 {
		t.Fatalf("submits=%d deletes=%d", mock.submits.Load(), mock.deletes.Load())
	}
	fact := h.sink.last(t).Attempts[0]
	if !fact.BillingUncertain {
		t.Fatalf("cancelled fact not billing uncertain: %+v", fact)
	}
}

func TestAzureOCRNativeRoundTrip(t *testing.T) {
	mock := &azureMock{poll: func(n int32, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, azureTwoPages)
	}}
	h := azureHarness(t, false, mock)
	resp := h.do(t.Context(), http.MethodPost, "/native/azure-document-intelligence-ocr/models/"+routeSlug, fullKey, []byte(`{"model":"`+routeSlug+`","base64Source":"QUJD"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Status        string `json:"status"`
		AnalyzeResult struct {
			ModelID string `json:"modelId"`
		} `json:"analyzeResult"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil || out.Status != "succeeded" || out.AnalyzeResult.ModelID != routeSlug {
		t.Fatalf("native azure: %d %s", resp.StatusCode, body)
	}
	if mock.submits.Load() != 1 || mock.polls.Load() != 1 {
		t.Fatalf("submits=%d polls=%d", mock.submits.Load(), mock.polls.Load())
	}
}
