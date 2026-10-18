package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func TestLegacyCompletionsGateway(t *testing.T) {
	for _, vendor := range []string{"openai", "vllm", "tgi", "fireworks", "together", "chat_mapping"} {
		native := vendor != "chat_mapping"
		t.Run(vendor, func(t *testing.T) {
			h := strictHarness(t, func(s *routing.Snapshot) {
				for id, p := range s.Providers {
					p.VendorID = "openai"
					p.Kind = "openai"
					p.ProfileID = "openai-chat"
					if native {
						p.VendorID = vendor
						p.ProfileID = vendor + "-completions"
						if vendor != "openai" {
							p.Kind = "openai_compatible"
						}
					}
					s.Providers[id] = p
				}
				if !native {
					r := s.Routes[routeSlug]
					r.Fidelity.Mode = routing.FidelityTransformed
					s.Routes[routeSlug] = r
				}
			})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				want := "/a/v1/completions"
				if !native {
					want = "/a/v1/chat/completions"
				}
				if r.URL.Path != want || !strings.Contains(string(body), modelA) || r.Header.Get("Authorization") != "Bearer "+secretA {
					t.Errorf("dispatch: %s %s", r.URL.Path, body)
				}
				w.Header().Set("Content-Type", "application/json")
				choices := `[{"index":0,"text":"hello","finish_reason":"stop"}]`
				object := "text_completion"
				if !native {
					choices = `[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]`
					object = "chat.completion"
				}
				io.WriteString(w, `{"id":"cmpl-1","object":"`+object+`","model":"`+modelA+`","choices":`+choices+`,"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"hello","max_tokens":8}`), nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(body), `"text":"hello"`) || !strings.Contains(string(body), routeSlug) {
				t.Fatalf("response %d %s", resp.StatusCode, body)
			}
			for _, key := range []string{readKey, otherKey} {
				resp = h.do(t.Context(), http.MethodPost, "/v1/completions", key, []byte(`{"model":"`+routeSlug+`","prompt":"hello"}`), nil)
				resp.Body.Close()
				if resp.StatusCode < 400 {
					t.Fatal("unauthorized completion")
				}
			}
			if h.mock.count("a") != 1 {
				t.Fatalf("unauthorized dispatch: %d", h.mock.count("a"))
			}
			if !native {
				resp = h.do(t.Context(), http.MethodPost, "/v1/completions", fullKey, []byte(`{"model":"`+routeSlug+`","prompt":"hello","echo":true}`), nil)
				body, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode < 400 || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
					t.Fatalf("unmapped request dispatched: %d %s", resp.StatusCode, body)
				}
			}
		})
	}
}
