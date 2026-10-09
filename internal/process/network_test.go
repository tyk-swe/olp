package process

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestManagementNetworkProtectsEveryManagementSurface(t *testing.T) {
	allowed := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/32")}
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(204) })
	handler := Perimeter("https://olp.example", managementNetwork(allowed, trusted, next))
	for _, path := range []string{"/", "/index.html", "/_app/immutable/a.js", "/settings", "/api/v1/auth/capabilities", "/api/v1/sessions", "/api/v1/setup", "/api/v1/oidc/callback", "/api/v1/openapi.json", "/api/v1/users", "/api/v1/missing", "/health", "/metrics"} {
		for _, test := range []struct {
			peer, forwarded string
			want            int
		}{
			{"192.0.2.9:1234", "", 204},
			{"[2001:db8::1]:1234", "", 204},
			{"[::ffff:192.0.2.9]:1234", "", 204},
			{"203.0.113.1:1234", "192.0.2.9", 403},
			{"10.0.0.1:1234", "192.0.2.9", 204},
			{"10.0.0.1:1234", "192.0.2.9, 203.0.113.1", 403},
			{"10.0.0.1:1234", "malformed", 403},
			{"malformed", "192.0.2.9", 403},
		} {
			t.Run(path+"/"+test.peer+"/"+test.forwarded, func(t *testing.T) {
				called = false
				r := httptest.NewRequest("GET", path, nil)
				r.RemoteAddr = test.peer
				r.Header.Set("X-Forwarded-For", test.forwarded)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != test.want || called != (test.want == 204) {
					t.Fatalf("status=%d called=%v", w.Code, called)
				}
				if test.want == 403 && (!strings.Contains(w.Body.String(), "management_ip_not_allowed") || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff") {
					t.Fatalf("unsafe refusal: %s", w.Body.String())
				}
			})
		}
	}
	for _, path := range []string{"/v1/models", "/v1/chat/completions", "/anthropic/v1/messages", "/gemini/v1beta/models", "/ws/live", "/bedrock/model/x/invoke", "/code/route/responses", "/native/mistral-fim/models/route"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "203.0.113.1:1234"
		handler.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatalf("inference surface restricted: %s", path)
		}
	}
}

func TestUnconfiguredManagementNetworkReturnsOriginalHandler(t *testing.T) {
	next := http.NewServeMux()
	if managementNetwork(nil, nil, next) != next {
		t.Fatal("unconfigured policy added middleware")
	}
}
