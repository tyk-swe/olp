//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
)

func TestEndUserBlockClosesEstablishedRealtimeSessions(t *testing.T) {
	testRealtimeAuthorityRestriction(t, "end_user")
}

func TestAPIKeyNetworkChangeClosesEstablishedRealtimeSessions(t *testing.T) {
	testRealtimeAuthorityRestriction(t, "network")
}

func TestRequiredAttributionClosesEstablishedRealtimeSessions(t *testing.T) {
	testRealtimeAuthorityRestriction(t, "attribution")
}

func testRealtimeAuthorityRestriction(t *testing.T, restriction string) {
	for _, protocol := range []string{"openai", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			h := newAccessHarness(t)
			sink := &captureSink{}
			h.Gateway.Sink = sink
			var slug, key, path string
			if protocol == "openai" {
				fixture := newStrictRealtimeFixture(t, "openai")
				slug, key = provisionStrictRealtime(t, h, "openai", fixture.URL+"/v1")
				path = "/v1/realtime?model=" + slug
			} else {
				fixture := newGeminiLifecycleProvider(t)
				slug, key, _ = provisionGeminiLifecycle(t, h, h.owner(), "gemini-live", fixture)
				path = "/gemini/ws/" + connectors.GeminiLiveMethod
			}
			owner := &browser{}
			h.want(owner, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, 201)
			authority, err := h.Runtime.Authenticate(key)
			if err != nil {
				t.Fatal(err)
			}
			keyPath := "/api/v1/api-keys/" + authority.ID
			detail := h.want(owner, "GET", keyPath, nil, nil, 200)
			h.want(owner, "PATCH", keyPath, map[string]any{"attribution_defaults": map[string]string{"team": "core"}, "end_user_source": "header", "end_user_policy": map[string]any{}, "allowed_cidrs": []string{"127.0.0.0/8", "::1/128"}}, etagHeader(detail), 200)
			digest := h.want(owner, "POST", keyPath+"/end-user", map[string]any{"identifier": "live-customer"}, nil, 200)["end_user_digest"].(string)
			h.refresh()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.HTTP.URL, "http")+path, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}, "X-OLP-End-User": {"live-customer"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			if protocol == "gemini" {
				if err = conn.Write(ctx, websocket.MessageText, fmt.Appendf(nil, `{"setup":{"model":"models/%s"}}`, slug)); err != nil {
					t.Fatal(err)
				}
				if _, _, err = conn.Read(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				for i, frame := range strictRealtimeClientFrames {
					if err = conn.Write(ctx, websocket.MessageText, frame); err != nil {
						t.Fatal(err)
					}
					if _, _, err = conn.Read(ctx); err != nil {
						t.Fatal(err)
					}
					if i == 1 {
						if _, _, err = conn.Read(ctx); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			detail = h.want(owner, "GET", keyPath, nil, nil, 200)
			policy := map[string]any{"end_user_policy": map[string]any{"blocked": []string{digest}}}
			if restriction == "network" {
				policy = map[string]any{"allowed_cidrs": []string{"192.0.2.0/24"}}
			}
			if restriction == "attribution" {
				policy = map[string]any{"required_attribution_keys": []string{"task"}}
			}
			if restriction == "overlap" {
				headers := etagHeader(detail)
				headers["Idempotency-Key"] = "overlap-live"
				h.want(owner, "POST", keyPath+"/rotate", map[string]any{"overlap_seconds": 2}, headers, 200)
			} else {
				h.want(owner, "PATCH", keyPath, policy, etagHeader(detail), 200)
			}
			h.refresh()
			_, _, err = conn.Read(ctx)
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("restricted caller retained live access: %v", err)
			}
			glEventually(t, "identified realtime terminal accounting", func() bool { return sink.count() == 1 })
			if terminal := sink.last(); terminal.EndUserDigest != digest || terminal.Attribution["team"] != "core" || len(terminal.Attempts) != 1 {
				t.Fatalf("lost identity or retried revoked stream: %+v", terminal)
			}
		})
	}
}

func TestAPIKeyOverlapExpiryClosesEstablishedRealtimeSessions(t *testing.T) {
	testRealtimeAuthorityRestriction(t, "overlap")
}
