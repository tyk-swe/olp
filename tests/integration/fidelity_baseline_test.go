//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/tests/fidelity"
	fixtures "github.com/tyk-swe/olp/tests/fixtures/fidelity"
)

// The provider expectation is independently authored JSON; neither side of
// this assertion runs a production protocol decoder or encoder. Provisioning
// goes through the same public management API used by operators.
func TestFidelityPublicNativeAndTranslatedConservationControls(t *testing.T) {
	data, err := fixtures.Files.ReadFile("v1/counterexamples.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Scenarios []struct {
			ID              string          `json:"id"`
			Source          json.RawMessage `json:"source"`
			Native          json.RawMessage `json:"native"`
			PreservedTarget json.RawMessage `json:"preserved_target"`
		}
	}
	if err = json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"openai", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			var captured [][]byte
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "openai" && r.Header.Get("Authorization") != "Bearer "+vendorSecret || kind == "gemini" && r.Header.Get("X-Goog-Api-Key") != vendorSecret {
					t.Error("configured provider credential was not injected")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if kind == "openai" {
						io.WriteString(w, `{"data":[{"id":"fixture-model","object":"model"}]}`)
					} else {
						io.WriteString(w, `{"models":[{"name":"models/fixture-model","displayName":"Fixture"}]}`)
					}
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					http.Error(w, "read failure", 400)
					return
				}
				mu.Lock()
				captured = append(captured, body)
				mu.Unlock()
				if kind == "openai" && strings.HasSuffix(r.URL.Path, "/responses") {
					writeResponsesFixture(w, "fixture-model", "independent native control", false)
					return
				}
				if kind == "openai" {
					io.WriteString(w, `{"id":"fixture-chat","object":"chat.completion","created":1,"model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"independent native control"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},"fixture_response_extension":{"preserved":true}}`)
				} else {
					io.WriteString(w, `{"responseId":"fixture-gemini","modelVersion":"fixture-model","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"independent translated control"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`)
				}
			}))
			t.Cleanup(up.Close)
			h := newAccessHarness(t)
			endpoint := up.URL + "/v1"
			if kind == "gemini" {
				endpoint = up.URL + "/v1beta"
			}
			slug, secret := provisionRoute(t, h, map[string]any{"kind": kind, "auth_mode": "api_key", "endpoint": endpoint}, vendorSecret, "fixture-model", []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
			mu.Lock()
			captured = nil
			mu.Unlock()
			id := "native-unknown-presence-values"
			if kind == "gemini" {
				id = "basic-qualified-translation-control"
			}
			var input map[string]json.RawMessage
			var expected json.RawMessage
			for _, scenario := range corpus.Scenarios {
				if scenario.ID != id {
					continue
				}
				if err = json.Unmarshal(scenario.Source, &input); err != nil {
					t.Fatal(err)
				}
				expected = scenario.Native
				if kind == "gemini" {
					expected = scenario.PreservedTarget
				}
			}
			if input == nil {
				t.Fatal("fixture missing", id)
			}
			input["model"], err = json.Marshal(slug)
			if err != nil {
				t.Fatal(err)
			}
			status, reply, _ := h.gateway(http.MethodPost, "/v1/chat/completions", secret, input)
			if status != http.StatusOK {
				t.Fatalf("positive control status %d: %v", status, reply)
			}
			mu.Lock()
			requests := append([][]byte(nil), captured...)
			mu.Unlock()
			if len(requests) != 1 {
				t.Fatalf("positive control dispatched %d times", len(requests))
			}
			if err = fidelity.Compare(expected, requests[0]); err != nil {
				t.Fatalf("effective native invocation: %v", err)
			}
			choices, ok := reply["choices"].([]any)
			if !ok || len(choices) != 1 {
				t.Fatal("missing client result", reply)
			}
			message, ok := choices[0].(map[string]any)["message"].(map[string]any)
			if !ok || message["content"] != "independent "+map[string]string{"openai": "native", "gemini": "translated"}[kind]+" control" {
				t.Fatal("client observation differs", reply)
			}
			if kind == "openai" {
				if err = fidelity.Compare([]byte(`{"preserved":true}`), mustFidelityJSON(t, reply["fixture_response_extension"])); err != nil {
					t.Fatal("native response extension lost", err)
				}
			}
			if kind == "gemini" {
				input["fixture_extension"] = json.RawMessage(`{"must_execute":true}`)
				status, reply, _ = h.gateway(http.MethodPost, "/v1/chat/completions", secret, input)
				if status != http.StatusBadRequest {
					t.Fatalf("incompatibility status %d: %v", status, reply)
				}
				failure, ok := reply["error"].(map[string]any)
				if !ok || failure["param"] != "/fixture_extension" || !strings.Contains(failure["message"].(string), "preserve") {
					t.Fatal("imprecise incompatibility", reply)
				}
				mu.Lock()
				calls := len(captured)
				mu.Unlock()
				if calls != 1 {
					t.Fatalf("incompatible translation dispatched: total %d", calls)
				}
			}
		})
	}
}

func mustFidelityJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
