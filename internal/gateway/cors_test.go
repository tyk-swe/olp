package gateway

import (
	"net/http/httptest"
	"testing"
)

func TestCORSIsAnExplicitOriginAllowlist(t *testing.T) {
	for _, origin := range []string{"https://console.example", "https://attacker.example", ""} {
		for _, configured := range []bool{true, false} {
			s := &Server{}
			if configured {
				s.cfg.CORSAllowedOrigins = []string{"https://console.example"}
			}
			r := httptest.NewRequest("OPTIONS", "/v1/responses", nil)
			r.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			s.preflight(w, r)
			want := ""
			if configured && origin == "https://console.example" {
				want = origin
			}
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != want {
				t.Errorf("origin %q configured=%v: got %q", origin, configured, got)
			}
		}
	}
}
