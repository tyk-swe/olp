package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func strictEmbeddings(snapshot *routing.Snapshot) {
	for id, provider := range snapshot.Providers {
		provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "embeddings", Surface: "openai", Mode: "unary"}}
		snapshot.Providers[id] = provider
	}
	route := snapshot.Routes[routeSlug]
	route.Operations = []string{"embeddings"}
	snapshot.Routes[routeSlug] = route
}

// No serving baseline is selected when the primary has no usable slot.

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

func TestStrictGeminiEmbeddingsAcceptQueryKey(t *testing.T) {
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
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
