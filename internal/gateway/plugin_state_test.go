package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestPluginRewritesCannotIntroduceProviderState(t *testing.T) {
	for name, rewrite := range map[string]abi.Rewrite{
		"enable storage":    {Op: abi.RewriteSet, Path: "/store", Value: json.RawMessage(`true`)},
		"omit storage":      {Op: abi.RewriteDelete, Path: "/store"},
		"null storage":      {Op: abi.RewriteSet, Path: "/store", Value: json.RawMessage(`null`)},
		"background work":   {Op: abi.RewriteSet, Path: "/background", Value: json.RawMessage(`true`)},
		"previous response": {Op: abi.RewriteSet, Path: "/previous_response_id", Value: json.RawMessage(`"resp_other"`)},
		"conversation":      {Op: abi.RewriteSet, Path: "/conversation", Value: json.RawMessage(`"conv_other"`)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{})
			route := h.rt.release.Snapshot.Routes[routeSlug]
			route.Targets = route.Targets[:1]
			h.rt.release.Snapshot.Routes[routeSlug] = route
			h.servePlugin(abi.Profile{ID: "acme-responses", Label: "Acme Responses", Dialect: "openai-responses", Hosting: abi.Hosting{
				Address: h.upstream.URL + "/a/v1", Headers: map[string]string{"Authorization": "Token {credential}"}, Rewrites: []abi.Rewrite{rewrite},
			}})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"model-a","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
			})

			response, body := h.responses(`,"store":false`)
			if response.StatusCode != http.StatusBadRequest || errorCode(t, body) != "unsupported_parameter" || h.mock.count("a") != 0 {
				t.Fatalf("state introduced by %s: status=%d body=%v upstream calls=%d", name, response.StatusCode, body, h.mock.count("a"))
			}
		})
	}
}
