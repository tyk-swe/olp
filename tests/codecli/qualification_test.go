//go:build codecli

package codecli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/codecli"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

func TestOfficialCodexOLPKeyNativeModelsToolAndResume(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(fmt.Sprintf("websocket=%t", ws), func(t *testing.T) {
			peer := codexfixture.New()
			peer.SetResponder(codexfixture.NewJourney("official-tool").Reply)
			server := httptest.NewServer(peer)
			defer server.Close()
			client := codecli.New(t, server.URL+"/code/fixture", "olp-controlled-key", ws)
			out := client.Exec(t, "Run the fixture's harmless printf command and report its output.")
			if !bytes.Contains(out, []byte("controlled fixture complete")) {
				t.Fatalf("missing final output: %s", out)
			}
			out = client.Exec(t, "resume", "--last", "Continue the same conversation.")
			if !bytes.Contains(out, []byte("controlled fixture complete")) {
				t.Fatalf("missing resumed output: %s", out)
			}
			requests := peer.Requests()
			if len(requests) < 3 {
				t.Fatalf("expected tool continuation and resume, got %d", len(requests))
			}
			thread := requests[0].Header.Get("Thread-Id")
			if thread == "" {
				t.Fatal("official CLI omitted thread-id")
			}
			toolResult := false
			for _, request := range requests {
				if request.Header.Get("Authorization") != "Bearer olp-controlled-key" || request.Header.Get("Thread-Id") != thread || request.WebSocket != ws {
					t.Fatal("auth, resumed identity or requested transport changed")
				}
				var body struct {
					Model string          `json:"model"`
					Input json.RawMessage `json:"input"`
				}
				if err := json.Unmarshal(request.Body, &body); err != nil || body.Model != codecli.Model {
					t.Fatalf("native model: %q (%v)", body.Model, err)
				}
				if bytes.Contains(body.Input, []byte(`"function_call_output"`)) && bytes.Contains(body.Input, []byte("OLP_CONTROLLED_TOOL")) {
					toolResult = true
				}
			}
			if !toolResult {
				t.Fatal("official CLI did not send the local tool result back")
			}
			t.Logf("official Codex %s: %d captured requests; native model %s; stable thread-id; no login", codecli.Version, len(requests), codecli.Model)
		})
	}
}

func TestOfficialCodexCancellationClosesHTTPUpstream(t *testing.T) {
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-cancel\"}}\n\n")
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	defer server.Close()
	client := codecli.New(t, server.URL+"/code/fixture", "olp-controlled-key", false)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := client.Command(ctx, "exec", "--skip-git-repo-check", "Wait for the fixture stream.")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("CLI exited: %v %s", err, output.Bytes())
	case <-ctx.Done():
		t.Fatal("no inference dispatch")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("CLI cancellation did not close upstream")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("CLI did not exit after cancellation")
	}
}

func TestOfficialCodexChildCarriesParentIdentity(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(fmt.Sprintf("websocket=%t", ws), func(t *testing.T) {
			peer := codexfixture.New()
			peer.SetResponder(codexfixture.NewJourney("official-child").Reply)
			server := httptest.NewServer(peer)
			defer server.Close()
			client := codecli.New(t, server.URL+"/code/fixture", "olp-controlled-key", ws)
			output := client.Exec(t, "-c", "features.multi_agent_v2=true", "Delegate the controlled fixture inspection to one child agent, then report.")
			requests := peer.Requests()
			if len(requests) < 3 {
				t.Fatal("missing child/continuation requests")
			}
			root := requests[0].Header.Get("Thread-Id")
			child := ""
			for _, req := range requests {
				if req.Header.Get("X-Codex-Parent-Thread-Id") == root {
					child = req.Header.Get("Thread-Id")
					if req.Header.Get("X-Openai-Subagent") != "collab_spawn" || req.WebSocket != ws {
						t.Fatalf("child transport=%t (want %t), subagent=%q\n%s", req.WebSocket, ws, req.Header.Get("X-Openai-Subagent"), output)
					}
				}
			}
			if child == "" || child == root {
				t.Fatal("official child lacks distinct identity and parent lineage")
			}
		})
	}
}

func TestOfficialCodexLocalCompactionKeepsIdentity(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(fmt.Sprintf("websocket=%t", ws), func(t *testing.T) {
			peer := codexfixture.New()
			peer.SetResponder(codexfixture.NewJourney("official-compact").Reply)
			server := httptest.NewServer(peer)
			defer server.Close()
			client := codecli.New(t, server.URL+"/code/fixture", "olp-controlled-key", ws)
			flags := append(codecli.ControlledFlags(ws), "-c", "model_auto_compact_token_limit=10000", "-c", `compact_prompt="OLP_CONTROLLED_COMPACT"`)
			client.Exec(t, append(flags, "Complete the controlled turn before compaction.")...)
			client.Exec(t, append(flags, "resume", "--last", "Continue after compacting the prior turn.")...)
			requests := peer.Requests()
			if len(requests) < 3 {
				t.Fatal("missing compaction and continuation")
			}
			root := requests[0].Header.Get("Thread-Id")
			compacted := false
			for _, req := range requests {
				if req.Header.Get("Thread-Id") != root || req.Header.Get("Authorization") != "Bearer olp-controlled-key" || req.WebSocket != ws {
					t.Fatal("compaction changed identity, auth or transport")
				}
				var body struct {
					Model string `json:"model"`
				}
				if err := json.Unmarshal(req.Body, &body); err != nil || body.Model != codecli.Model {
					t.Fatal("compaction changed native model")
				}
				if bytes.Contains(req.Body, []byte("OLP_CONTROLLED_COMPACT")) {
					compacted = true
				}
			}
			if !compacted {
				t.Fatal("official CLI did not dispatch the compaction prompt")
			}
		})
	}
}
