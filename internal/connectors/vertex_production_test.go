//go:build !oidctest

package connectors

import (
	"net/http"
	"testing"
)

func TestProductionVertexADCRejectsLoopbackFixtures(t *testing.T) {
	cfg := Config{Kind: "vertex_ai", AuthMode: "adc", CloudRegion: "global"}
	for _, endpoint := range []string{"http://127.0.0.1/token", "https://127.0.0.1/token", "https://[::1]/token"} {
		req, err := http.NewRequest("POST", endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.vertexDestination(req.URL) {
			t.Fatalf("production accepted %s", endpoint)
		}
		if _, err := NewAuth(localPolicy()).Apply(t.Context(), req, cfg, nil, nil); err != ErrAuthentication {
			t.Fatalf("production auth accepted %s: %v", endpoint, err)
		}
	}
}
