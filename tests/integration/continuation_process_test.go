//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/fidelity"
)

func continuationProcessEnvironment(t *testing.T, h *accessHarness) map[string]string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth"), []byte(h.AuthHex), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ring"), []byte(h.Ring), 0600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"OLP_DATABASE_URL": h.DBURL, "OLP_DATABASE_MAX_CONNECTIONS": "6", "OLP_VALKEY_URL": required(t, "OLP_TEST_VALKEY_URL"), "OLP_AUTH_HMAC_KEY_FILE": filepath.Join(dir, "auth"), "OLP_MASTER_KEY_FILE": filepath.Join(dir, "ring"), "OLP_LISTEN_ADDR": "127.0.0.1:0", "OLP_OBSERVABILITY_LISTEN_ADDR": "127.0.0.1:0", "OLP_PROVIDER_EGRESS_ALLOW_CIDRS": "127.0.0.0/8", "OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS": "127.0.0.1", "OLP_SHUTDOWN_TIMEOUT": "4s", "HOSTNAME": "continuation-recovery"}
}
func processContinuationRequest(t *testing.T, origin, key, method, path string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, origin+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
func TestContinuationSurvivesGatewayProcessLossWithoutInferenceReplay(t *testing.T) {
	binary := required(t, "OLP_TEST_BINARY")
	stream, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	nextGolden, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, ready := range []bool{false, true} {
		name := "before ready"
		if ready {
			name = "after ready before handle observed"
		}
		t.Run(name, func(t *testing.T) {
			h := newAccessHarness(t)
			var calls atomic.Int64
			accepted := make(chan struct{}, 1)
			slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, r *http.Request, body []byte) {
				calls.Add(1)
				var request map[string]json.RawMessage
				_ = json.Unmarshal(body, &request)
				var messages []json.RawMessage
				_ = json.Unmarshal(request["messages"], &messages)
				if len(messages) > 1 {
					if err := fidelity.Compare(nextGolden, body); err != nil {
						t.Error(err)
						http.Error(w, "unexpected next request", 400)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg-recovered-final","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"Both tools completed."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":4}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if ready {
					_, _ = w.Write(stream)
					return
				}
				prefix := bytes.SplitN(stream, []byte("event: content_block_start"), 2)[0]
				_, _ = w.Write(prefix)
				_ = http.NewResponseController(w).Flush()
				accepted <- struct{}{}
				<-r.Context().Done()
			})
			env := continuationProcessEnvironment(t, h)
			first := testutil.StartProcess(t, binary, "gateway", env)
			source := []byte(strings.Replace(continuationInput, "ROUTE", slug, 1))
			headers := continuationHeaders()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, first.PublicOrigin+"/v1/chat/completions", bytes.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+key)
			for name, value := range headers {
				req.Header.Set(name, value)
			}
			clientResponse := make(chan *http.Response, 1)
			clientError := make(chan error, 1)
			go func() {
				resp, e := http.DefaultClient.Do(req)
				if e != nil {
					clientError <- e
				} else {
					clientResponse <- resp
				}
			}()
			if !ready {
				select {
				case <-accepted:
				case err := <-clientError:
					t.Fatal(err)
				case <-time.After(8 * time.Second):
					t.Fatal("provider not accepted before process loss")
				}
			} else {
				var resp *http.Response
				select {
				case resp = <-clientResponse:
				case err := <-clientError:
					t.Fatal(err)
				case <-time.After(8 * time.Second):
					t.Fatal("no tool stream")
				}
				scanner := bufio.NewScanner(resp.Body)
				found := false
				for scanner.Scan() {
					if bytes.Contains(scanner.Bytes(), []byte(`"tool_calls"`)) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("no actionable call: status=%d err=%v", resp.StatusCode, scanner.Err())
				}
				// Deliberately discard the connection before consuming the terminal handle.
				_ = resp.Body.Close()
			}
			if err := first.Kill(); err != nil {
				t.Fatal(err)
			}
			second := testutil.StartProcess(t, binary, "gateway", env)
			recovery := processContinuationRequest(t, second.PublicOrigin, key, http.MethodGet, "/v1/continuation-submissions/"+headers["X-OLP-Submission-ID"], nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
			raw, err := io.ReadAll(recovery.Body)
			recovery.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !ready {
				if recovery.StatusCode != 409 || !bytes.Contains(raw, []byte("continuation_outcome_unknown")) {
					t.Fatalf("unknown recovery: %d %s", recovery.StatusCode, raw)
				}
				retry := processContinuationRequest(t, second.PublicOrigin, key, http.MethodPost, "/v1/chat/completions", source, headers)
				retryBody, _ := io.ReadAll(retry.Body)
				retry.Body.Close()
				if retry.StatusCode != 409 || calls.Load() != 1 {
					t.Fatalf("unknown work repeated: %d calls=%d %s", retry.StatusCode, calls.Load(), retryBody)
				}
				return
			}
			if recovery.StatusCode != 200 || bytes.Contains(raw, []byte("opaque-fixture-signature-do-not-log")) {
				t.Fatalf("ready recovery: %d %s", recovery.StatusCode, raw)
			}
			var state struct {
				Handle    string          `json:"handle"`
				Assistant json.RawMessage `json:"assistant"`
			}
			if json.Unmarshal(raw, &state) != nil || state.Handle == "" || len(state.Assistant) == 0 {
				t.Fatalf("invalid recoverable delivery: %s", raw)
			}
			var next map[string]json.RawMessage
			_ = json.Unmarshal(source, &next)
			delete(next, "stream")
			next["messages"], _ = json.Marshal([]json.RawMessage{json.RawMessage(`{"role":"user","content":"Weather and time in Paris?"}`), state.Assistant, json.RawMessage(`{"role":"tool","tool_call_id":"call-weather","content":"sunny"}`), json.RawMessage(`{"role":"tool","tool_call_id":"call-clock","content":"14:00"}`)})
			nextRaw, _ := json.Marshal(next)
			nextHeaders := continuationHeaders()
			nextHeaders["X-OLP-Continuation-Handle"] = state.Handle
			final := processContinuationRequest(t, second.PublicOrigin, key, http.MethodPost, "/v1/chat/completions", nextRaw, nextHeaders)
			finalBody, _ := io.ReadAll(final.Body)
			final.Body.Close()
			if final.StatusCode != 200 || !bytes.Contains(finalBody, []byte("Both tools completed.")) || calls.Load() != 2 {
				t.Fatalf("recovered next: %d calls=%d %s", final.StatusCode, calls.Load(), finalBody)
			}
		})
	}
}
