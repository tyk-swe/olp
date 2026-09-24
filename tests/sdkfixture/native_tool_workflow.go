package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/tyk-swe/olp/tests/fidelity"
	fixtures "github.com/tyk-swe/olp/tests/fixtures/fidelity"
)

const nativeToolRoute = "sdk-native-tools-route"

// nativeToolWorkflow belongs solely to the scripted provider fixture. Its
// counters and verification listener are not gateway state or a public API.
// Native SDKs must carry the complete assistant content into the next request.
type nativeToolWorkflow struct {
	initial, next, events []byte
	mu                    sync.Mutex
	counts                nativeToolCounts
}

type nativeToolCounts struct {
	Dispatches      int  `json:"dispatches"`
	InitialRequests int  `json:"initial_requests"`
	NextRequests    int  `json:"next_requests"`
	Rejected        int  `json:"rejected_requests"`
	Complete        bool `json:"complete"`
}

func newNativeToolWorkflow() (*nativeToolWorkflow, error) {
	next, err := fixtures.Files.ReadFile("v1/anthropic-tool-next-request.json")
	if err != nil {
		return nil, err
	}
	events, err := fixtures.Files.ReadFile("v1/anthropic-tool-workflow.sse")
	if err != nil {
		return nil, err
	}
	// The initial invocation uses the frozen controls/tools and original user
	// turn; streaming is the only added transport field. No production codec
	// builds either expected request or the scripted response events.
	var initial map[string]json.RawMessage
	if err := json.Unmarshal(next, &initial); err != nil {
		return nil, err
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(initial["messages"], &messages); err != nil || len(messages) != 3 {
		return nil, fmt.Errorf("native tool fixture must contain three turns")
	}
	initial["messages"], err = json.Marshal(messages[:1])
	if err != nil {
		return nil, err
	}
	initial["stream"] = json.RawMessage("true")
	encoded, err := json.Marshal(initial)
	if err != nil {
		return nil, err
	}
	return &nativeToolWorkflow{initial: encoded, next: next, events: events}, nil
}

func (f *nativeToolWorkflow) snapshot() nativeToolCounts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts
}

func (f *nativeToolWorkflow) verification() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /native-tool-workflow", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, f.snapshot())
	})
	return mux
}

func (f *nativeToolWorkflow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts.Dispatches++
	fail := func(message string) {
		f.counts.Rejected++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "message": message}})
	}
	if r.Method != http.MethodPost || r.URL.RequestURI() != "/native-tools/v1/messages" || r.Header.Get("X-Api-Key") != credential || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
		fail("native workflow method, path, authentication or API revision differs")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		fail("native workflow request exceeds the fixture limit")
		return
	}
	expected := f.initial
	if f.counts.Dispatches == 2 && f.counts.InitialRequests == 1 {
		expected = f.next
	} else if f.counts.Dispatches != 1 {
		fail("native workflow dispatched more than the two required requests")
		return
	}
	if err := fidelity.Compare(expected, body); err != nil {
		// The independent oracle reports only a pointer, never opaque content.
		fail("native workflow request differs: " + err.Error())
		return
	}
	if f.counts.Dispatches == 1 {
		f.counts.InitialRequests++
		w.Header().Set("Content-Type", "text/event-stream")
		// Fragment each frozen event at the transport boundary without changing
		// a byte of its native payload, sequence or terminal contract.
		for _, frame := range bytes.SplitAfter(f.events, []byte("\n\n")) {
			if len(frame) == 0 {
				continue
			}
			for _, fragment := range [][]byte{frame[:len(frame)/2], frame[len(frame)/2:]} {
				if _, err := w.Write(fragment); err != nil {
					return
				}
				if err := http.NewResponseController(w).Flush(); err != nil {
					return
				}
			}
		}
		return
	}
	f.counts.NextRequests++
	f.counts.Complete = true
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"msg-native-tool-final","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"Weather: sunny. Time: 14:00."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":64,"output_tokens":8}}`)
}
