//go:build integration

package integration_test

import (
	"net/http"
	"testing"
)

func TestAPIKeyCIDRsPersistAndEnforceAcrossPublicScopes(t *testing.T) {
	f := glSeedIn(t, "key-network", glPrice{})
	id, secret := f.key("local", map[string]any{"scopes": []string{"inference", "models_read"}, "allowed_cidrs": []string{"127.0.0.0/8", "::1/128"}})
	path := "/api/v1/api-keys/" + id
	check := func(want int) {
		t.Helper()
		if status, err := endUserChat(t.Context(), f.h, secret, routeSlug, "caller"); err != nil || status != want {
			t.Fatalf("inference status=%d error=%v", status, err)
		}
		r, _ := http.NewRequestWithContext(t.Context(), "GET", f.h.HTTP.URL+"/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+secret)
		// An untrusted forwarded address cannot grant access.
		r.Header.Set("X-Forwarded-For", "192.0.2.42")
		response, err := f.h.HTTP.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("models status=%d", response.StatusCode)
		}
	}
	check(200)
	detail := f.h.want(f.owner, "GET", path, nil, nil, 200)
	if len(detail["allowed_cidrs"].([]any)) != 2 {
		t.Fatal("network policy missing from key detail")
	}
	for _, cidrs := range [][]string{{"localhost"}, {"::/129"}, {"192.0.2.0/24", "192.0.2.42/24"}} {
		f.h.want(f.owner, "PATCH", path, map[string]any{"allowed_cidrs": cidrs}, etagHeader(detail), 422)
	}
	f.h.want(f.owner, "PATCH", path, map[string]any{"allowed_cidrs": []string{"192.0.2.0/24"}}, etagHeader(detail), 200)
	f.h.refresh()
	before := f.vendor.chats.Load()
	check(403)
	if f.vendor.chats.Load() != before {
		t.Fatal("network refusal dispatched")
	}
	detail = f.h.want(f.owner, "GET", path, nil, nil, 200)
	f.h.want(f.owner, "PATCH", path, map[string]any{"allowed_cidrs": nil}, etagHeader(detail), 200)
	f.h.refresh()
	check(200)
	detail = f.h.want(f.owner, "GET", path, nil, nil, 200)
	if len(detail["allowed_cidrs"].([]any)) != 0 {
		t.Fatal("network restrictions were not cleared")
	}
}
