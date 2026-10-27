package gateway

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func passthroughHarness(t *testing.T) *harness {
	t.Helper()
	return strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"passthrough"}
		route.MaxAttempts = 1
		route.Targets = route.Targets[:1]
		route.Passthrough = &routing.PassthroughConfig{
			Dialect:                    "bound-native",
			Methods:                    []string{"GET", "POST", "HEAD"},
			Paths:                      []string{"/v1/widgets/*", "/v1/status"},
			Query:                      []string{"limit"},
			Headers:                    []string{"X-Widget-Mode", "If-None-Match"},
			AcknowledgeOpaqueUnmetered: true,
		}
		s.Routes[routeSlug] = route
	})
}

const passthroughURL = "/native/bound-native/routes/" + routeSlug + "/v1/widgets/abc"

func TestPassthroughInstallationPolicyExclusionDispatchesNothing(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"passthrough"}
		route.MaxAttempts = 1
		route.Targets = route.Targets[:1]
		route.Passthrough = &routing.PassthroughConfig{
			Dialect:                    "bound-native",
			Methods:                    []string{"GET", "POST", "HEAD"},
			Paths:                      []string{"/v1/widgets/*", "/v1/status"},
			Query:                      []string{"limit"},
			Headers:                    []string{"X-Widget-Mode", "If-None-Match"},
			AcknowledgeOpaqueUnmetered: true,
		}
		s.Routes[routeSlug] = route
		s.InstallationPolicy = &routing.Policy{Constraints: routing.Preferences{Only: []string{"provider:00000000-0000-0000-0000-000000000000"}}}
	})
	resp := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("policy-excluded bound connection served the request")
	}
	if got := h.mock.count("a"); got != 0 {
		t.Fatalf("upstream dispatches = %d, want policy refusal before dispatch", got)
	}
}

type failingResponseWriter struct {
	http.ResponseWriter
}

func (w failingResponseWriter) Write([]byte) (int, error)   { return 0, io.ErrClosedPipe }
func (w failingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestPassthroughClientWriteFailureKeepsProviderSuccess(t *testing.T) {
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(failingResponseWriter{w}, r)
		})
	}
	h := strictRelease(t, newHarnessWrapped(t, Config{}, wrap), func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"passthrough"}
		route.MaxAttempts = 1
		route.Targets = route.Targets[:1]
		route.Passthrough = &routing.PassthroughConfig{
			Dialect:                    "bound-native",
			Methods:                    []string{"GET"},
			Paths:                      []string{"/v1/widgets/*"},
			AcknowledgeOpaqueUnmetered: true,
		}
		s.Routes[routeSlug] = route
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "widget-result")
	})
	resp := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	env := h.sink.last(t)
	if len(env.Attempts) != 1 {
		t.Fatalf("attempts = %d", len(env.Attempts))
	}
	fact := env.Attempts[0]
	if fact.Class != "success" || !fact.UsageObserved || !fact.UsageComplete || fact.BillingUncertain {
		t.Fatalf("successful provider work discarded on delivery failure: %+v", fact)
	}
	if env.Outcome == "success" {
		t.Fatal("failed downstream delivery recorded as a successful request outcome")
	}
}

func TestPassthroughCallerRoutingExclusionDispatchesNothing(t *testing.T) {
	h := passthroughHarness(t)
	resp := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, map[string]string{
		"X-OLP-Routing": `{"only":["provider:00000000-0000-0000-0000-000000000000"]}`,
	})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("caller-excluded bound connection served the request")
	}
	if got := h.mock.count("a"); got != 0 {
		t.Fatalf("upstream dispatches = %d, want caller routing refusal before dispatch", got)
	}
}

func TestPassthroughRoundTripStripsCallerCredentials(t *testing.T) {
	h := passthroughHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a/v1/v1/widgets/abc" {
			t.Errorf("upstream path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+secretA {
			t.Errorf("bound credential missing: %q", r.Header.Get("Authorization"))
		}
		if strings.Contains(r.Header.Get("Authorization"), fullKey) || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("caller credential forwarded: %v", r.Header)
		}
		if r.Header.Get("X-Widget-Mode") != "fast" {
			t.Errorf("allowed header not forwarded: %v", r.Header)
		}
		if r.Header.Get("X-Secret-Mode") != "" {
			t.Errorf("unlisted header forwarded")
		}
		if r.URL.Query().Get("limit") != "2" {
			t.Errorf("allowed query not forwarded: %s", r.URL.RawQuery)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "opaque-body" {
			t.Errorf("body %q", body)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Widget-Result", "done")
		io.WriteString(w, "widget-result")
	})
	resp := h.do(t.Context(), http.MethodPost, passthroughURL+"?limit=2", fullKey, []byte("opaque-body"), map[string]string{
		"X-Widget-Mode": "fast", "X-Secret-Mode": "no", "X-Api-Key": "caller-secret", "Cookie": "session=x",
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "widget-result" || resp.Header.Get("X-Widget-Result") != "done" {
		t.Fatalf("passthrough: %d %s", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if env.Operation != "passthrough" || env.Route != routeSlug || len(env.Attempts) != 1 || env.Attempts[0].Class != "success" || !env.Attempts[0].UsageObserved || !env.Attempts[0].UsageComplete || env.Attempts[0].BillingUncertain {
		t.Fatalf("envelope: %+v attempts %+v", env, env.Attempts)
	}
	if env.Attempts[0].Price != nil {
		t.Fatalf("unpriced attempt priced: %+v", env.Attempts[0].Price)
	}
}

type feeDenying struct{ *allowing }

func (c *feeDenying) Do(ctx context.Context, args ...string) (any, error) {
	if slices.Contains(args, "0.75") {
		return []any{int64(1), int64(0), "daily_cost_estimate", int64(1000), int64(86_400_000), int64(2_678_400_000)}, nil
	}
	return c.allowing.Do(ctx, args...)
}

func TestPassthroughPricedRouteReservesFeeBeforeDispatch(t *testing.T) {
	h := passthroughHarness(t)
	fee := "0.75"
	h.rt.inputs = &usage.RoutingInputs{Prices: []usage.RoutingPrice{
		{Price: usage.Price{ProviderKind: "openai_compatible", Model: modelA, Operation: "passthrough", PerRequest: &fee, Currency: "usd"}, EffectiveAt: time.Now()},
	}, RefreshedAt: time.Now()}
	authority := h.rt.keys[fullKey]
	authority.LookupID = "lookup_full"
	authority.Policy.DailyCostLimit = stringptr("0.5")
	h.rt.keys[fullKey] = authority
	h.gateway.Admission = newAdmission(t, &feeDenying{allowing: newAllowing(0, 0, 0, false)})
	resp := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || h.mock.count("a") != 0 {
		t.Fatalf("priced passthrough dispatched past its budget: %d %s calls=%d", resp.StatusCode, body, h.mock.count("a"))
	}
	h.rt.inputs.Discounts = []usage.RoutingDiscount{{ID: "discount-1", ProviderKind: "openai_compatible", Multiplier: "0.5", EffectiveAt: time.Now().Add(-time.Hour)}}
	resp = h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 {
		t.Fatalf("discounted passthrough not admitted at its discounted fee: %d calls=%d", resp.StatusCode, h.mock.count("a"))
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 || env.Attempts[0].Discount == nil {
		t.Fatalf("discount not pinned on the attempt fact: %+v", env.Attempts)
	}
}

func TestPassthroughOversizedChunkedResponseRefusedBeforeCommit(t *testing.T) {
	h := passthroughHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 32*1024)
		flusher := w.(http.Flusher)
		for range 40 {
			w.Write(chunk)
			flusher.Flush()
		}
	})
	resp := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("oversized chunked response delivered as %d with %d bytes", resp.StatusCode, len(body))
	}
}

func TestPassthroughTruncatedResponseRefusedBeforeCommit(t *testing.T) {
	h := passthroughHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("partial"))
	})
	resp := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("truncated response delivered as %d with %q", resp.StatusCode, body)
	}
}

func TestPassthroughHeadAndConditionalResponsesPassThrough(t *testing.T) {
	h := passthroughHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.Header().Set("ETag", `"v1"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Length", "2097152")
		w.WriteHeader(http.StatusOK)
	})
	head := h.do(t.Context(), http.MethodHead, passthroughURL, fullKey, nil, nil)
	io.Copy(io.Discard, head.Body)
	head.Body.Close()
	if head.StatusCode != http.StatusOK || head.Header.Get("Content-Length") != "2097152" {
		t.Fatalf("HEAD metadata refused or lost: %d content-length=%q", head.StatusCode, head.Header.Get("Content-Length"))
	}
	conditional := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, map[string]string{"If-None-Match": `"v1"`})
	io.Copy(io.Discard, conditional.Body)
	conditional.Body.Close()
	if conditional.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional response rewritten: %d", conditional.StatusCode)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 || env.Attempts[0].Class != "success" || !attemptSuccessful(&env.Attempts[0]) {
		t.Fatalf("conditional 304 not priced as a successful attempt: %+v", env.Attempts)
	}
}

func TestPassthroughUpstreamErrorRecordsFailureOutcome(t *testing.T) {
	h := passthroughHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"missing"}`)
	})
	resp := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("upstream status not forwarded: %d", resp.StatusCode)
	}
	env := h.sink.last(t)
	if env.Outcome != "failure" {
		t.Fatalf("forwarded upstream error recorded as %q outcome", env.Outcome)
	}
}

func TestPassthroughCoolingSlotDispatchesNothing(t *testing.T) {
	h := passthroughHarness(t)
	for _, provider := range h.rt.release.Snapshot.Providers {
		if provider.Name == "a" {
			h.gateway.health.cooldown(provider.ID, h.slotA, time.Minute)
		}
	}
	resp := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || h.mock.count("a") != 0 {
		t.Fatalf("cooling slot dispatched: %d %s calls=%d", resp.StatusCode, body, h.mock.count("a"))
	}
}

func TestPassthroughRefusalsDispatchNothing(t *testing.T) {
	for _, tc := range []struct {
		name, method, url, key string
		headers                map[string]string
	}{
		{"encoded dot", "GET", "/native/bound-native/routes/" + routeSlug + "/v1/widgets/%2e%2e", fullKey, nil},
		{"encoded slash", "GET", "/native/bound-native/routes/" + routeSlug + "/v1/widgets/a%2fb", fullKey, nil},
		{"dot segment", "GET", "/native/bound-native/routes/" + routeSlug + "/v1/widgets/..", fullKey, nil},
		{"two segments", "GET", "/native/bound-native/routes/" + routeSlug + "/v1/widgets/a/b", fullKey, nil},
		{"zero segments", "GET", "/native/bound-native/routes/" + routeSlug + "/v1/widgets", fullKey, nil},
		{"unknown query", "GET", passthroughURL + "?evil=1", fullKey, nil},
		{"method", "DELETE", passthroughURL, fullKey, nil},
		{"foreign key", "GET", passthroughURL, otherKey, nil},
		{"wrong dialect", "GET", "/native/other/routes/" + routeSlug + "/v1/widgets/abc", fullKey, nil},
		{"missing route", "GET", "/native/bound-native/routes/nope/v1/widgets/abc", fullKey, nil},
		{"websocket", "GET", passthroughURL, fullKey, map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := passthroughHarness(t)
			resp := h.do(t.Context(), tc.method, tc.url, tc.key, nil, tc.headers)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 400 || h.mock.count("a")+h.mock.count("b") != 0 {
				t.Fatalf("%s dispatched: %d %s", tc.name, resp.StatusCode, body)
			}
		})
	}
}

func TestPassthroughRedirectRefused(t *testing.T) {
	h := passthroughHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/b/v1/widgets/abc")
		w.WriteHeader(http.StatusFound)
	})
	resp := h.do(t.Context(), http.MethodGet, passthroughURL, fullKey, nil, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "passthrough_redirect_refused") || h.mock.count("b") != 0 {
		t.Fatalf("redirect followed: %d %s b=%d", resp.StatusCode, body, h.mock.count("b"))
	}
}

func TestPassthroughCannotShadowModeledEndpoint(t *testing.T) {
	h := passthroughHarness(t)
	resp := h.do(t.Context(), http.MethodPost, "/native/bound-native/models/"+routeSlug, fullKey, []byte(`{"model":"x"}`), nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 || h.mock.count("a") != 0 {
		t.Fatalf("modeled endpoint shadowed: %d %s", resp.StatusCode, body)
	}
}

func TestPassthroughBodyLimit(t *testing.T) {
	h := strictHarness(t, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations = []string{"passthrough"}
		route.MaxAttempts = 1
		route.Targets = route.Targets[:1]
		limit := int64(4)
		route.MaxBodyBytes = &limit
		route.Passthrough = &routing.PassthroughConfig{Dialect: "bound-native", Methods: []string{"POST"}, Paths: []string{"/v1/widgets/*"}, AcknowledgeOpaqueUnmetered: true}
		s.Routes[routeSlug] = route
	})
	resp := h.do(t.Context(), http.MethodPost, passthroughURL, fullKey, []byte("12345"), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || h.mock.count("a") != 0 {
		t.Fatalf("body limit: %d calls=%d", resp.StatusCode, h.mock.count("a"))
	}
}
