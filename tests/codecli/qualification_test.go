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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/tests/codecli"
)

type capture struct {
	Header    http.Header
	Body      []byte
	WebSocket bool
}

type upstream struct {
	mu                sync.Mutex
	requests          []capture
	turn              int
	childJourney      bool
	childDone         chan struct{}
	childOnce         sync.Once
	compactionJourney bool
}

func (u *upstream) record(header http.Header, body []byte, ws bool) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = append(u.requests, capture{header.Clone(), bytes.Clone(body), ws})
	var request struct {
		Generate *bool `json:"generate"`
	}
	_ = json.Unmarshal(body, &request)
	if request.Generate != nil && !*request.Generate {
		return 0
	}
	u.turn++
	return u.turn
}

func events(turn int) [][]byte {
	id := fmt.Sprintf("resp-controlled-%d", turn)
	if turn == 0 {
		return [][]byte{[]byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, id)), []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q}}`, id))}
	}
	item := `{"type":"message","role":"assistant","id":"msg-controlled","content":[{"type":"output_text","text":"controlled fixture complete"}]}`
	if turn == 1 {
		item = `{"type":"function_call","call_id":"call-controlled","name":"exec_command","arguments":"{\"cmd\":\"printf OLP_CONTROLLED_TOOL\",\"max_output_tokens\":100}"}`
	}
	return [][]byte{
		[]byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q}}`, id)),
		[]byte(`{"type":"response.output_item.done","item":` + item + `}`),
		[]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":1}}}}`, id)),
	}
}

func (u *upstream) reply(ctx context.Context, header http.Header, turn int) [][]byte {
	if u.compactionJourney {
		if turn == 0 {
			return events(0)
		}
		reply := events(2)
		if turn == 1 {
			reply[2] = []byte(`{"type":"response.completed","response":{"id":"resp-controlled-2","usage":{"input_tokens":20000,"output_tokens":4,"total_tokens":20004}}}`)
		}
		return reply
	}
	if !u.childJourney || turn == 0 {
		return events(turn)
	}
	if header.Get("X-Codex-Parent-Thread-Id") != "" {
		u.childOnce.Do(func() { close(u.childDone) })
		return events(2)
	}
	if turn == 1 {
		reply := events(1)
		reply[1] = []byte(`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"spawn-controlled","namespace":"collaboration","name":"spawn_agent","arguments":"{\"message\":\"Inspect the controlled fixture only.\",\"task_name\":\"controlled_child\"}"}}`)
		return reply
	}
	select {
	case <-u.childDone:
	case <-ctx.Done():
	}
	return events(2)
}

func (u *upstream) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/code/fixture/responses" {
		http.Error(w, "unqualified fixture path", 404)
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(4 << 20)
		for {
			_, body, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			turn := u.record(r.Header, body, true)
			for _, event := range u.reply(r.Context(), r.Header, turn) {
				if err := conn.Write(r.Context(), websocket.MessageText, event); err != nil {
					return
				}
			}
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return
	}
	turn := u.record(r.Header, body, false)
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range u.reply(r.Context(), r.Header, turn) {
		fmt.Fprintf(w, "data: %s\n\n", event)
		w.(http.Flusher).Flush()
	}
}

func TestOfficialCodexOLPKeyNativeModelsToolAndResume(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(fmt.Sprintf("websocket=%t", ws), func(t *testing.T) {
			u := &upstream{}
			server := httptest.NewServer(http.HandlerFunc(u.serve))
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
			u.mu.Lock()
			defer u.mu.Unlock()
			if len(u.requests) < 3 {
				t.Fatalf("expected tool continuation and resume, got %d", len(u.requests))
			}
			thread := u.requests[0].Header.Get("Thread-Id")
			if thread == "" {
				t.Fatal("official CLI omitted thread-id")
			}
			toolResult := false
			for _, request := range u.requests {
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
			t.Logf("official Codex %s: %d captured requests; native model %s; stable thread-id; no login", codecli.Version, len(u.requests), codecli.Model)
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
			u := &upstream{childJourney: true, childDone: make(chan struct{})}
			server := httptest.NewServer(http.HandlerFunc(u.serve))
			defer server.Close()
			client := codecli.New(t, server.URL+"/code/fixture", "olp-controlled-key", ws)
			output := client.Exec(t, "-c", "features.multi_agent_v2=true", "Delegate the controlled fixture inspection to one child agent, then report.")
			u.mu.Lock()
			defer u.mu.Unlock()
			if len(u.requests) < 3 {
				t.Fatal("missing child/continuation requests")
			}
			root := u.requests[0].Header.Get("Thread-Id")
			child := ""
			for _, req := range u.requests {
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
			u := &upstream{compactionJourney: true}
			server := httptest.NewServer(http.HandlerFunc(u.serve))
			defer server.Close()
			client := codecli.New(t, server.URL+"/code/fixture", "olp-controlled-key", ws)
			flags := []string{"-c", "model_auto_compact_token_limit=10000", "-c", `compact_prompt="OLP_CONTROLLED_COMPACT"`}
			client.Exec(t, append(flags, "Complete the controlled turn before compaction.")...)
			client.Exec(t, append(flags, "resume", "--last", "Continue after compacting the prior turn.")...)
			u.mu.Lock()
			defer u.mu.Unlock()
			if len(u.requests) < 3 {
				t.Fatal("missing compaction and continuation")
			}
			root := u.requests[0].Header.Get("Thread-Id")
			compacted := false
			for _, req := range u.requests {
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
