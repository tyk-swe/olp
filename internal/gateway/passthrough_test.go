package gateway

import (
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

const (
	passthroughGolden = "testdata/passthrough.golden.json"
	passthroughRoutes = "/native/bound-native/routes/"
	passthroughURL    = passthroughRoutes + routeSlug + "/v1/widgets/abc"
)

type passthroughCase struct {
	name     string
	method   string
	path     string
	key      string
	body     string
	headers  map[string]string
	limited  bool
	upstream http.HandlerFunc
}

type passthroughRecord struct {
	surfaceRecord
	Forwarded []recordedRequest `json:"forwarded"`
	Relayed   map[string]string `json:"relayed,omitempty"`
}

func passthroughCases() []passthroughCase {
	served := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Widget-Result", "done")
		w.Header().Set("Set-Cookie", "upstream=1")
		io.WriteString(w, "widget-result")
	}
	upgrade := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}
	return []passthroughCase{
		{name: "post round trip", method: "POST", path: passthroughURL + "?limit=2", key: fullKey, body: "opaque-body", upstream: served,
			headers: map[string]string{"X-Widget-Mode": "fast", "X-Secret-Mode": "no", "X-Api-Key": "caller-secret", "Cookie": "session=x"}},
		{name: "get round trip", method: "GET", path: passthroughURL, key: fullKey, upstream: served},
		{name: "head round trip", method: "HEAD", path: passthroughURL, key: fullKey, upstream: served},
		{name: "conditional get", method: "GET", path: passthroughURL, key: fullKey, headers: map[string]string{"If-None-Match": `"v1"`},
			upstream: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotModified) }},
		{name: "upstream error", method: "GET", path: passthroughURL, key: fullKey, upstream: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"missing"}`)
		}},
		{name: "upstream redirect", method: "GET", path: passthroughURL, key: fullKey, upstream: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/a/v1/v1/status", http.StatusFound)
		}},
		{name: "limits unavailable", method: "GET", path: passthroughURL, key: fullKey, limited: true, upstream: served},
		{name: "no credential", method: "GET", path: passthroughURL, upstream: served},
		{name: "scope denied", method: "GET", path: passthroughURL, key: readKey, upstream: served},
		{name: "foreign key", method: "GET", path: passthroughURL, key: otherKey, upstream: served},
		{name: "encoded dot", method: "GET", path: passthroughRoutes + routeSlug + "/v1/widgets/%2e%2e", key: fullKey, upstream: served},
		{name: "encoded slash", method: "GET", path: passthroughRoutes + routeSlug + "/v1/widgets/a%2fb", key: fullKey, upstream: served},
		{name: "two segments", method: "GET", path: passthroughRoutes + routeSlug + "/v1/widgets/a/b", key: fullKey, upstream: served},
		{name: "zero segments", method: "GET", path: passthroughRoutes + routeSlug + "/v1/widgets", key: fullKey, upstream: served},
		{name: "unknown query", method: "GET", path: passthroughURL + "?evil=1", key: fullKey, upstream: served},
		{name: "unpublished method", method: "DELETE", path: passthroughURL, key: fullKey, upstream: served},
		{name: "wrong dialect", method: "GET", path: "/native/other/routes/" + routeSlug + "/v1/widgets/abc", key: fullKey, upstream: served},
		{name: "missing route", method: "GET", path: passthroughRoutes + "nope/v1/widgets/abc", key: fullKey, upstream: served},
		{name: "websocket upgrade", method: "GET", path: passthroughURL, key: fullKey, headers: upgrade, upstream: served},
	}
}

func runPassthroughCase(t *testing.T, c passthroughCase) passthroughRecord {
	t.Helper()
	h := newSurfaceHarness(t, c.limited)
	strictRelease(t, h.harness, func(s *routing.Snapshot) {
		route := s.Routes[routeSlug]
		route.Operations, route.MaxAttempts, route.Targets = []string{routing.PassthroughOperation}, 1, route.Targets[:1]
		route.Passthrough = &routing.PassthroughConfig{
			Dialect: "bound-native", Methods: []string{"GET", "POST", "HEAD"},
			Paths: []string{"/v1/widgets/*", "/v1/status"}, Query: []string{"limit"},
			Headers: []string{"X-Widget-Mode", "If-None-Match"}, AcknowledgeOpaqueUnmetered: true,
		}
		s.Routes[routeSlug] = route
	})
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowedRoutes = []string{routeSlug}
	h.rt.keys[fullKey] = authority
	var mu sync.Mutex
	forwarded := []recordedRequest{}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		request := recordRequest(t, r, h.upstream.URL)
		mu.Lock()
		forwarded = append(forwarded, request)
		mu.Unlock()
		c.upstream(w, r)
	})
	var payload []byte
	if c.body != "" {
		payload = []byte(c.body)
	}
	response := h.do(t.Context(), c.method, c.path, c.key, payload, c.headers)
	relayed := map[string]string{}
	for _, name := range []string{"X-Widget-Result", "Set-Cookie", "Location"} {
		if value := response.Header.Get(name); value != "" {
			relayed[name] = value
		}
	}
	record := passthroughRecord{surfaceRecord: h.record(t, response), Relayed: relayed}
	mu.Lock()
	defer mu.Unlock()
	record.Forwarded = forwarded
	return record
}

func TestPassthroughGolden(t *testing.T) {
	cases := []goldenCase{}
	for _, c := range passthroughCases() {
		cases = append(cases, goldenCase{c.name, func(t *testing.T) any { return runPassthroughCase(t, c) }})
	}
	checkGolden(t, passthroughGolden, cases)
}
