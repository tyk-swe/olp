package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// servePluginProfile moves the harness's first provider onto a plugin
// profile of the Chat Completions dialect at its upstream.
func (h *harness) servePluginProfile(hosting abi.Hosting) {
	h.t.Helper()
	h.servePlugin(abi.Profile{ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", Hosting: hosting})
}

// servePlugin moves the harness's first provider onto a plugin profile.
func (h *harness) servePlugin(profile abi.Profile) {
	h.t.Helper()
	digest := strings.Repeat("cd", 32)
	plugin, err := connectors.NewPluginProfile(digest, abi.Manifest{Name: "acme", Version: "1.0.0", Profiles: []abi.Profile{profile}}, profile.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	secrets := map[string][]byte{}
	for id, provider := range h.rt.release.Snapshot.Providers {
		for _, slot := range provider.Slots {
			secrets[*slot.CredentialID], _ = h.rt.release.Credential(*slot.CredentialID)
		}
		if provider.Slots[0].ID == h.slotA {
			provider.Kind, provider.AuthMode, provider.Plugin = connectors.KindPlugin, connectors.AuthStaticCredential, plugin
			provider.ProfileID, provider.ProfileRevision, provider.Endpoint = profile.ID, digest, plugin.Address(nil)
			h.rt.release.Snapshot.Providers[id] = provider
		}
	}
	if h.rt.release, err = runtime.NewRelease(uuid.NewString(), 7, h.rt.release.Snapshot, secrets); err != nil {
		h.t.Fatal(err)
	}
}

// A plugin profile's envelope wraps what the gateway sends, after its
// rewrites, and its unwrapped responses and stream events are what the codecs,
// the caller and usage accounting see.
func TestPluginEnvelopeWrapsRequestsAndUnwrapsResponses(t *testing.T) {
	h := newHarness(t, Config{})
	h.servePluginProfile(abi.Hosting{
		Address:  h.upstream.URL + "/a/v1",
		Headers:  map[string]string{"Authorization": "Token {credential}"},
		Envelope: &abi.Envelope{Request: "request", Fields: map[string]string{"model": "{model}", "project": "olp"}, Response: "response"},
		Rewrites: []abi.Rewrite{
			{Op: abi.RewriteSet, Path: "/store", Value: json.RawMessage(`false`)},
			{Op: abi.RewriteDefault, Path: "/temperature", Value: json.RawMessage(`0.5`)},
			{Op: abi.RewriteDelete, Path: "/user"},
		},
	})
	type enveloped struct {
		Model   string                     `json:"model"`
		Project string                     `json:"project"`
		Request map[string]json.RawMessage `json:"request"`
	}
	sent := make(chan enveloped, 2)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var body enveloped
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil || r.Header.Get("Authorization") != "Token "+secretA {
			t.Errorf("the upstream received %s with %v", data, r.Header)
		}
		sent <- body
		if string(body.Request["stream"]) != "true" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"traceId":"t-1","response":{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}}`, modelA, answerText)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"choices":[{"index":0,"delta":{"role":"assistant","content":"hel"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":4,"total_tokens":6}}`,
		} {
			fmt.Fprintf(w, "data: {\"response\":%s,\"traceId\":\"t-2\"}\n\n", strings.Replace(chunk, "{", `{"id":"chatcmpl-2","object":"chat.completion.chunk","created":1,"model":"`+modelA+`",`, 1))
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})

	resp, body := h.chat(fullKey, nil, `,"user":"caller-1","temperature":0.9`)
	if resp.StatusCode != http.StatusOK || body["model"] != routeSlug || body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != answerText {
		t.Fatalf("unary %d %v", resp.StatusCode, body)
	}
	if env := h.sink.last(t); env.Usage == nil || env.Usage.TotalTokens != 5 || len(env.Attempts) != 1 || env.Attempts[0].Class != classSuccess {
		t.Fatalf("accounted %+v", env)
	}
	unary := <-sent
	if unary.Model != modelA || unary.Project != "olp" || string(unary.Request["model"]) != `"`+modelA+`"` || unary.Request["messages"] == nil ||
		string(unary.Request["store"]) != "false" || string(unary.Request["temperature"]) != "0.9" || unary.Request["user"] != nil {
		t.Fatalf("the upstream received %+v", unary)
	}

	stream := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer stream.Body.Close()
	var text string
	done := false
	scanner := bufio.NewScanner(stream.Body)
	for scanner.Scan() {
		payload, found := strings.CutPrefix(scanner.Text(), "data: ")
		if !found {
			continue
		}
		if payload == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil || chunk.Model != routeSlug {
			t.Fatalf("the caller received %s: %v", payload, err)
		}
		for _, choice := range chunk.Choices {
			text += choice.Delta.Content
		}
	}
	if stream.StatusCode != http.StatusOK || text != "hello" || !done {
		t.Fatalf("stream %d %q done=%v", stream.StatusCode, text, done)
	}
	if env := h.sink.last(t); env.Mode != "streaming" || env.Usage == nil || env.Usage.TotalTokens != 6 || len(env.Attempts) != 1 {
		t.Fatalf("accounted %+v", env)
	}
	streamed := <-sent
	if string(streamed.Request["stream"]) != "true" || !strings.Contains(string(streamed.Request["stream_options"]), `"include_usage":true`) ||
		string(streamed.Request["store"]) != "false" || string(streamed.Request["temperature"]) != "0.5" {
		t.Fatalf("the upstream received %+v", streamed)
	}
}
