package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func strictEmbeddings(snapshot *runtime.Snapshot) {
	for id, provider := range snapshot.Providers {
		provider.Capabilities = []runtime.Capability{{Model: provider.Capabilities[0].Model, Operation: "embeddings", Surface: "openai", Mode: "unary"}}
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes[routeSlug]
	route.Operations = []string{"embeddings"}
	snapshot.Routes[routeSlug] = route
}

func TestStrictUnaryReplanningPreservesCallerRoute(t *testing.T) {
	for _, tc := range []struct {
		name     string
		behavior runtime.Behavior
		via      string
		cooldown bool
	}{
		{"delegation", runtime.Behavior{Selectors: []runtime.Selector{{ID: "delegate", Route: backupSlug}}}, usage.ViaSelector, false},
		{"fallback", runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: []string{runtime.FallbackExhausted}}}}, usage.ViaFallback, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := strictHarness(t, strictEmbeddings)
			h.republish(func(snapshot *runtime.Snapshot, _, b runtime.Provider) {
				splitRoutes(snapshot, b, tc.behavior)
			})
			if tc.cooldown {
				// No serving baseline is selected when the primary has no usable slot.
				h.gateway.health.cooldown(h.provider("a"), h.slotA, time.Minute)
			}
			h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != modelB {
					t.Errorf("upstream model=%q error=%v", request.Model, err)
				}
				status(http.StatusOK, `{"object":"list","model":"`+modelB+`","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`)(w, r)
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hi"}`), nil)
			defer resp.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || body["model"] != routeSlug || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
				t.Fatalf("status=%d body=%v dispatches=%d/%d envelope=%+v", resp.StatusCode, body, h.mock.count("a"), h.mock.count("b"), h.sink.last(t))
			}
			env := h.sink.last(t)
			if env.Route != routeSlug || len(env.Attempts) != 1 {
				t.Fatalf("envelope %+v", env)
			}
			last := env.Attempts[len(env.Attempts)-1]
			if last.Class != classSuccess || last.Leg == nil || last.Leg.Route != backupSlug || last.Leg.Via != tc.via {
				t.Fatalf("replanned attempt %+v", last)
			}
		})
	}
}

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestStrictUnaryClientCancellationIsNotAFailure(t *testing.T) {
	h := strictHarness(t, strictEmbeddings)
	logs := &lockedLog{}
	h.server.Config.ErrorLog = log.New(logs, "", 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		cancel()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/embeddings", strings.NewReader(`{"model":"`+routeSlug+`","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); !errors.Is(err, context.Canceled) {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatalf("client error %v", err)
	}
	env := h.sink.last(t)
	if env.Outcome != "cancelled" || env.Status != 0 || len(env.Attempts) != 1 || env.Attempts[0].Class != classCancelled {
		t.Fatalf("envelope %+v", env)
	}
	if h.mock.count("b") != 0 || h.gateway.admission.Admitted() != 0 {
		t.Fatalf("dispatches b=%d admission=%d", h.mock.count("b"), h.gateway.admission.Admitted())
	}
	if strings.Contains(logs.String(), "panic serving") {
		t.Fatalf("handler panicked: %s", logs.String())
	}
}

func TestGeminiEmbeddingErrorsUseGeminiEnvelope(t *testing.T) {
	for _, action := range []string{"embedContent", "batchEmbedContents"} {
		t.Run(action, func(t *testing.T) {
			h := strictHarness(t, strictEmbeddings)
			resp := h.do(t.Context(), http.MethodPost, "/gemini/v1beta/models/"+routeSlug+":"+action+"?key=invalid-key", "", []byte(`{}`), nil)
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var body struct {
				Error struct {
					Code   int    `json:"code"`
					Status string `json:"status"`
				} `json:"error"`
			}
			if err := json.Unmarshal(data, &body); err != nil || resp.StatusCode != http.StatusUnauthorized ||
				body.Error.Code != http.StatusUnauthorized || body.Error.Status != "UNAUTHENTICATED" {
				t.Fatalf("status=%d body=%s", resp.StatusCode, data)
			}
		})
	}
}

func TestStrictGeminiEmbeddingsAcceptQueryKey(t *testing.T) {
	h := strictHarness(t, func(snapshot *runtime.Snapshot) {
		strictEmbeddings(snapshot)
		for id, provider := range snapshot.Providers {
			provider.Kind, provider.ProfileID = "gemini", "gemini-generation"
			snapshot.Providers[id] = provider
		}
	})
	query := make(chan string, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		query <- r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"embedding":{"values":[0.1,0.2]},"usageMetadata":{"promptTokenCount":1}}`)
	})
	resp := h.do(t.Context(), http.MethodPost, "/gemini/v1beta/models/"+routeSlug+":embedContent?key="+url.QueryEscape(fullKey), "",
		[]byte(`{"content":{"parts":[{"text":"hi"}]}}`), nil)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	if got := <-query; strings.Contains(got, "key=") {
		t.Fatalf("upstream query carried the gateway key: %q", got)
	}
}
