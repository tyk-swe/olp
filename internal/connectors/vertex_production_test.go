//go:build !oidctest

package connectors

import (
	"net/http"
	"testing"
)

func TestProductionVertexRejectsLoopbackFixtures(t *testing.T) {
	for _, auth := range []string{"adc", "service_account"} {
		cfg := Config{Kind: "vertex_ai", AuthMode: auth, CloudRegion: "global"}
		for _, endpoint := range []string{"http://127.0.0.1/token", "https://127.0.0.1/token", "https://[::1]/token"} {
			req, _ := http.NewRequest(http.MethodPost, endpoint, nil)
			if cfg.vertexDestination(req.URL) {
				t.Fatalf("production accepted %s", endpoint)
			}
			if _, err := NewAuth(localPolicy()).Apply(t.Context(), req, cfg, nil, nil); err != ErrAuthentication {
				t.Fatalf("production auth accepted %s: %v", endpoint, err)
			}
		}
	}
}
