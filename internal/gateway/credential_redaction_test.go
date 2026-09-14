package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestUpstreamClientErrorsRedactAttemptedCredentials(t *testing.T) {
	for _, authMode := range []string{"api_key", "headers"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			for _, stream := range []bool{false, true} {
				for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusTeapot} {
					t.Run(fmt.Sprintf("%s%s/stream=%t/status=%d", authMode, path, stream, status), func(t *testing.T) {
						h := newHarness(t, Config{})
						// Include overlapping values and JSON metacharacters: upstream
						// diagnostics may embed a JSON serialization of the headers.
						values := map[string]string{"x-api-key": " \t" + `secret-"\<&` + "\t ", "X-Tenant": `secret-"\<&-long` + " ", "X-Empty": ""}
						credential := []byte(secretA + " \t")
						if authMode == "headers" {
							credential, _ = json.Marshal(values)
						}
						secrets := map[string][]byte{}
						for id, provider := range h.rt.release.Snapshot.Providers {
							for _, slot := range provider.Slots {
								secrets[*slot.CredentialID], _ = h.rt.release.Credential(*slot.CredentialID)
							}
							if provider.Slots[0].ID == h.slotA {
								provider.AuthMode = authMode
								provider.CredentialHeaders = []string{"X-Api-Key", "x-tenant", "X-Empty"}
								h.rt.release.Snapshot.Providers[id] = provider
							}
						}
						secrets[h.credA] = credential
						var err error
						h.rt.release, err = runtime.NewRelease(uuid.NewString(), 7, h.rt.release.Snapshot, secrets)
						if err != nil {
							t.Fatal(err)
						}
						h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
							echo := r.Header.Get("Authorization") + " token=" + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
							if authMode == "headers" {
								for name, value := range values {
									if r.Header.Get(name) != strings.TrimSpace(value) {
										t.Errorf("incorrect %s credential", name)
									}
									echo += " " + r.Header.Get(name)
								}
								encoded, _ := json.Marshal(r.Header)
								echo += " " + string(encoded) + " " + string(credential)
								var unescaped strings.Builder
								encoder := json.NewEncoder(&unescaped)
								encoder.SetEscapeHTML(false)
								encoder.Encode(r.Header)
								echo += " " + unescaped.String()
							}
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(status)
							json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "Invalid prompt; " + echo, "code": echo, "type": echo}})
						})
						input := map[string]any{"model": routeSlug, "stream": stream}
						if path == "/v1/responses" {
							input["input"] = "hi"
						} else {
							input["messages"] = []any{map[string]string{"role": "user", "content": "hi"}}
						}
						body, _ := json.Marshal(input)
						resp := h.do(t.Context(), http.MethodPost, path, fullKey, body, nil)
						defer resp.Body.Close()
						raw, err := io.ReadAll(resp.Body)
						if err != nil {
							t.Fatal(err)
						}
						var result map[string]any
						if err := json.Unmarshal(raw, &result); err != nil {
							t.Fatal(err)
						}
						wantStatus := status
						if status == http.StatusTeapot {
							wantStatus = http.StatusBadGateway
						}
						if resp.StatusCode != wantStatus || errorCode(t, result) != "upstream_rejected" {
							t.Fatalf("rejection changed: %d %s", resp.StatusCode, raw)
						}
						message := result["error"].(map[string]any)["message"].(string)
						if !strings.HasPrefix(message, "Invalid prompt; ") || !strings.Contains(message, "[REDACTED]") || strings.Contains(message, "secret-") || strings.Contains(message, "-long") {
							t.Fatalf("credential echo was not safely redacted: %s", message)
						}
						if h.mock.count("a") != 1 || h.mock.count("b") != 0 || h.sink.last(t).Attempts[0].Class != classUpstreamClient {
							t.Fatal("redaction changed terminal client-error behavior")
						}
					})
				}
			}
		}
	}
}
