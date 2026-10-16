//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/tests/fidelity"
	corpus "github.com/tyk-swe/olp/tests/fixtures/fidelity"
)

// nativeToolDocuments is the independent two-turn Anthropic tool workflow: the
// streamed first request and its events, then the next request and its reply.
type nativeToolDocuments struct {
	initial, next, events, final []byte
}

func nativeToolCorpus(t *testing.T) nativeToolDocuments {
	t.Helper()
	next, err := corpus.Files.ReadFile("v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	events, err := corpus.Files.ReadFile("v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(next, &fields) != nil {
		t.Fatal("invalid next request fixture")
	}
	var turns []json.RawMessage
	if json.Unmarshal(fields["messages"], &turns) != nil || len(turns) != 3 {
		t.Fatal("invalid next request turns")
	}
	fields["messages"], _ = json.Marshal(turns[:1])
	fields["stream"] = json.RawMessage("true")
	initial, _ := json.Marshal(fields)
	final := []byte(`{"id":"msg-native-tool-final","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"Weather: sunny. Time: 14:00."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":64,"output_tokens":8}}`)
	return nativeToolDocuments{initial: initial, next: next, events: events, final: final}
}

// nativeToolProvider answers certification probes generically until measured
// is set, then serves only requests that match the independent tool corpus.
type nativeToolProvider struct {
	*httptest.Server
	measured    atomic.Bool
	first, next atomic.Int64
}

func newNativeToolProvider(t *testing.T, d nativeToolDocuments) *nativeToolProvider {
	t.Helper()
	p := &nativeToolProvider{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != vendorSecret || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
			http.Error(w, "native authentication changed", 401)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			return
		}
		if !p.measured.Load() {
			var fields map[string]json.RawMessage
			json.Unmarshal(body, &fields)
			kindGeneration(w, "anthropic", string(fields["stream"]) == "true")
			return
		}
		if fidelity.Compare(d.initial, body) == nil {
			p.first.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			for _, frame := range bytes.Split(d.events, []byte("\n\n")) {
				if len(bytes.TrimSpace(frame)) == 0 {
					continue
				}
				if _, err := w.Write(append(append([]byte{}, frame...), '\n', '\n')); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
			return
		}
		if fidelity.Compare(d.next, body) == nil {
			p.next.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write(d.final)
			return
		}
		t.Error("provider request differs from the independent native tool corpus")
		http.Error(w, "native request changed", 400)
	}))
	t.Cleanup(p.Close)
	return p
}
