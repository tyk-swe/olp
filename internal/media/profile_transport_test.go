package media

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func TestConfiguredMediaTransportUsesTLSRootsAndSemanticHeaders(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/images/generations" || r.Header.Get("Api-Key") != "media-key" || r.Header.Get("OpenAI-Beta") != "fixture-v1" {
			t.Error("wrong native media request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"url":"https://cdn.example/image.png"}]}`))
	}))
	defer upstream.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	transport := &Transport{Client: policy.Client(time.Second), Auth: connectors.NewAuth(policy), Egress: policy, Spool: testSpool(t, MinCapacityBytes), MaxResponseBytes: 1 << 20}
	target := Target{ConnectionScope: "provider/revision/slot", Model: "native-image", Secret: []byte("media-key"), Config: connectors.Config{ProfileID: "azure-v1-chat", ProfileRevision: "1", Kind: "azure_openai", AuthMode: "api_key", Endpoint: upstream.URL, SemanticHeaders: map[string]string{"OpenAI-Beta": "fixture-v1"}, Network: &egress.ConnectionOptions{TrustRootsPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}))}}}
	result, failure := transport.Do(t.Context(), target, &UpstreamCall{Method: "POST", Path: "images/generations", JSON: []byte(`{"model":"native-image","prompt":"original"}`), Kind: ResponseImages}, nil)
	if failure != nil || result.Images == nil || len(result.Images.Images) != 1 {
		t.Fatalf("configured media TLS failed: %+v", failure)
	}
	transport.connections.CloseIdleConnections()
}
