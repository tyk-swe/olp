package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
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
			Methods:                    []string{"GET", "POST"},
			Paths:                      []string{"/v1/widgets/*", "/v1/status"},
			Query:                      []string{"limit"},
			Headers:                    []string{"X-Widget-Mode"},
			AcknowledgeOpaqueUnmetered: true,
		}
		s.Routes[routeSlug] = route
	})
}

const passthroughURL = "/native/bound-native/routes/" + routeSlug + "/v1/widgets/abc"

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
