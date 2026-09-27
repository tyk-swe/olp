package process

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPerimeterHeadersCoverEveryPublicResponse(t *testing.T) {
	refuse := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Admission refusals, fallbacks and console errors write no headers
		// of their own.
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	for _, tc := range []struct {
		path   string
		want   map[string]string
		absent []string
	}{
		{"/api/v1/users", map[string]string{
			"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY",
			"Cross-Origin-Resource-Policy": "same-origin", "Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'",
		}, []string{"Cross-Origin-Opener-Policy"}},
		{"/providers", map[string]string{
			"Referrer-Policy": "same-origin", "X-Frame-Options": "DENY", "Cross-Origin-Opener-Policy": "same-origin",
			"Cross-Origin-Resource-Policy": "same-origin",
		}, []string{"Cache-Control"}},
		{"/v1/chat/completions", map[string]string{"Cache-Control": "no-store"}, []string{"Cross-Origin-Resource-Policy", "X-Frame-Options"}},
		{"/bedrock/model/m/converse", map[string]string{"Cache-Control": "no-store"}, []string{"Cross-Origin-Resource-Policy"}},
	} {
		for origin, hsts := range map[string]bool{"https://olp.example.com": true, "http://127.0.0.1:8080": false} {
			w := httptest.NewRecorder()
			Perimeter(origin, refuse).ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			h := w.Result().Header
			if h.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%s: missing nosniff", tc.path)
			}
			if (h.Get("Strict-Transport-Security") != "") != hsts {
				t.Errorf("%s from %s: HSTS %q", tc.path, origin, h.Get("Strict-Transport-Security"))
			}
			for name, value := range tc.want {
				if got := h.Get(name); got != value {
					t.Errorf("%s: %s = %q, want %q", tc.path, name, got, value)
				}
			}
			for _, name := range tc.absent {
				if h.Get(name) != "" {
					t.Errorf("%s: unexpected %s", tc.path, name)
				}
			}
		}
	}
	console := httptest.NewRecorder()
	Perimeter("https://olp.example.com", refuse).ServeHTTP(console, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := console.Result().Header.Get("Permissions-Policy"); got == "" || strings.Contains(got, "clipboard") {
		t.Fatalf("the console's permissions policy must deny unused features but keep the clipboard: %q", got)
	}
}
