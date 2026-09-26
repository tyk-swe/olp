//go:build integration

package integration_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
)

type policyCapture struct {
	mu     sync.Mutex
	events []gateway.Envelope
}

func (c *policyCapture) Terminal(e gateway.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}
func (c *policyCapture) last() gateway.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events[len(c.events)-1]
}

func TestRoutingPolicyPublicationIntersectionAndProvenance(t *testing.T) {
	fixture := glSeed(t, nil, limits.FailClosed)
	h, owner := fixture.h, fixture.owner
	var draftID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.route_drafts WHERE slug=$1", routeSlug).Scan(&draftID); err != nil {
		t.Fatal(err)
	}
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "policy client", "scopes": []string{"inference"}, "allowed_routes": []string{routeSlug}}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.refresh()
	secret := key["secret"].(string)
	capture := &policyCapture{}
	h.Gateway.Sink = capture
	installationPath := "/api/v1/routing-policies/installation/" + uuid.Nil.String()
	routePath := "/api/v1/routing-policies/route-draft/" + draftID
	keyPath := "/api/v1/routing-policies/api-key/" + key["id"].(string)
	update := func(path string, body any) map[string]any {
		t.Helper()
		current := h.want(owner, "GET", path, nil, nil, 200)
		headers := withMatch(current, map[string]string{"Idempotency-Key": uuid.NewString()})
		result := h.want(owner, "PUT", path, body, headers, 200)
		replayed := h.want(owner, "PUT", path, body, headers, 200)
		if replayed["etag"] != result["etag"] {
			t.Fatal("policy mutation did not replay")
		}
		return result
	}
	before := h.Runtime.Release().Sequence
	beforePolicy, _ := json.Marshal(h.Runtime.Release().Snapshot.Routes[routeSlug].Policy)
	update(routePath, map[string]any{"allowed_strategies": []string{"weighted", "price"}, "constraints": map[string]any{"only": []string{"provider:" + fixture.provider}}, "defaults": map[string]any{"strategy": "price"}})
	h.refresh()
	afterPolicy, _ := json.Marshal(h.Runtime.Release().Snapshot.Routes[routeSlug].Policy)
	if h.Runtime.Release().Sequence != before || string(afterPolicy) != string(beforePolicy) {
		t.Fatal("staged route policy changed live serving")
	}
	draft := h.want(owner, "GET", "/api/v1/route-drafts/"+draftID, nil, nil, 200)
	preview := h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/simulate", map[string]any{"operation": "generation", "surface": "openai", "mode": "unary", "seed": "policy"}, nil, 200)
	target := preview["targets"].([]any)[0].(map[string]any)
	if target["decision"].(map[string]any)["strategy"] != "price" {
		t.Fatalf("draft did not use staged policy: %v", preview)
	}
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	h.refresh()
	if p := h.Runtime.Release().Snapshot.Routes[routeSlug].Policy; p == nil || p.Defaults.Strategy == nil || *p.Defaults.Strategy != "price" {
		t.Fatal("revision omitted its policy")
	}
	body := map[string]any{"model": routeSlug, "messages": []any{map[string]string{"role": "user", "content": "private prompt never retained"}}, "max_tokens": 16}
	if status, _, _ := h.gateway("POST", "/v1/chat/completions", secret, body); status != 200 {
		t.Fatalf("published defaults: %d", status)
	}
	if e := capture.last(); len(e.Attempts) != 1 || e.Attempts[0].Strategy != "price" || len(e.Attempts[0].PolicyDigest) != 64 {
		t.Fatalf("execution ignored default policy: %+v", e.Attempts)
	}
	update(keyPath, map[string]any{"constraints": map[string]any{"ignore": []string{"provider:" + fixture.provider}}})
	h.refresh()
	status, data, _ := parityCall(t, h, secret, "/v1/chat/completions", body, "openai")
	if status != 503 {
		t.Fatalf("request expanded key restriction: %d %s", status, data)
	}
	update(keyPath, map[string]any{"defaults": map[string]any{"strategy": "weighted"}})
	h.refresh()
	update(installationPath, map[string]any{"allowed_strategies": []string{"weighted"}})
	h.refresh()
	status, data, _ = parityCall(t, h, secret, "/v1/chat/completions", body, "openai")
	if status != 400 {
		t.Fatalf("request expanded allowed strategies: %d %s", status, data)
	}
	if status, _, _ := h.gateway("POST", "/v1/chat/completions", secret, body); status != 200 {
		t.Fatalf("key default precedence: %d", status)
	}
	event := capture.last()
	if event.Attempts[0].Strategy != "weighted" {
		t.Fatal("key default did not override route default")
	}
	serialized, _ := json.Marshal(event)
	if strings.Contains(string(serialized), "private prompt") || strings.Contains(string(serialized), "allow_fallbacks") || strings.Contains(string(serialized), "X-OLP-Routing") {
		t.Fatal("content or raw request preferences entered durable envelope")
	}
}
