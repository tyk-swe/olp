package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestInferenceChecksCurrentEndpointPolicy(t *testing.T) {
	h := newHarness(t, Config{})
	// The pinned release was published with a plain-HTTP exception. The
	// current process permits the IP, but no longer permits plain HTTP.
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
	h.gateway.egress = policy
	h.gateway.client = policy.Client(upstreamHeaderTimeout)
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "upstream_unavailable" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatal("dispatched credentials to an endpoint denied by the current policy")
	}
}

func TestInferenceNormalizesEndpointAndForwardsZeroTopLogprobs(t *testing.T) {
	for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses} {
		for _, suffix := range []string{"/", "//", "///"} {
			t.Run(string(family)+suffix, func(t *testing.T) {
				h := newHarness(t, Config{})
				for id, provider := range h.rt.release.Snapshot.Providers {
					provider.Endpoint += suffix
					h.rt.release.Snapshot.Providers[id] = provider
				}
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/a/v1"+endpointPath(family) {
						t.Errorf("upstream path %q", r.URL.Path)
						http.NotFound(w, r)
						return
					}
					var fields map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
						t.Error(err)
					}
					if string(fields["top_logprobs"]) != "0" {
						t.Errorf("top_logprobs not forwarded: %s", fields["top_logprobs"])
					}
					if family == openai.FamilyChat {
						completion(modelA, answerText)(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"id":"resp-1","object":"response","model":"model-a","status":"completed","output":[]}`)
				})
				input := `"messages":[{"role":"user","content":"hi"}],"logprobs":true`
				if family == openai.FamilyResponses {
					input = `"input":"hi"`
				}
				resp := h.do(t.Context(), http.MethodPost, "/v1"+endpointPath(family), fullKey, []byte(`{"model":"`+routeSlug+`",`+input+`,"top_logprobs":0}`), nil)
				defer resp.Body.Close()
				var body map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusOK || body["model"] != routeSlug {
					t.Fatalf("status %d body %v", resp.StatusCode, body)
				}
			})
		}
	}
}
