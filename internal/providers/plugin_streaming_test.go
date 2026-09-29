package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Certification reaches an upstream that serves only streams the way the
// gateway does: a non-streaming tuple is certified from the aggregated stream.
func TestForcedStreamingCertifiesNonStreamingTuplesFromTheStream(t *testing.T) {
	var mu sync.Mutex
	streamed := []bool{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		streamed = append(streamed, body.Stream)
		mu.Unlock()
		if !body.Stream || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Token fixture-secret" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		item := `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}`
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"acme-large","output":[]}}`,
			`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
			`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"acme-large","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer upstream.Close()
	cfg := Configuration{ProviderID: "acme", Kind: KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileID: "acme-responses", ProfileRevision: strings.Repeat("ab", 32)}
	pinAcme(t, &cfg, upstream.URL, abi.Profile{
		ID: "acme-responses", Label: "Acme Responses", Dialect: "openai-responses",
		Hosting: abi.Hosting{Address: upstream.URL + "/v1", Headers: map[string]string{"Authorization": "Token {credential}"}, ForceStreaming: true},
	})
	for _, mode := range []string{ModeUnary, ModeStreaming} {
		if err := New(nil, loopbackPolicy(), nil).certifyTuple(t.Context(), &cfg, []byte("fixture-secret"), "acme-large", CapabilityInput{Operation: OperationGeneration, Surface: "openai", Mode: mode}, 4096); err != nil {
			t.Fatalf("%s certification: %v", mode, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(streamed) != 2 || !streamed[0] || !streamed[1] {
		t.Fatalf("the upstream received stream flags %v", streamed)
	}
}
