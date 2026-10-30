//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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

func TestWorkflowModelRecoveryFencesLostReplicaWithoutInterruptingLiveReplica(t *testing.T) {
	binary := required(t, "OLP_TEST_BINARY")
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Workflow restart recovery")
	started := make(chan string, 2)
	release := make(chan struct{})
	delivered := make(chan string, 2)
	canceled := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+vendorModel+`","object":"model"}]}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.25, 0.5}}}, "model": vendorModel, "usage": map[string]int{"prompt_tokens": 4, "total_tokens": 4}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/responses") {
			writeResponsesFixture(w, vendorModel, "ok", false)
			return
		}
		if bytes.Contains(body, []byte("olp_certification_probe")) {
			writeJSON(w, map[string]any{"id": "probe", "object": "chat.completion", "model": vendorModel, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "probe-call", "type": "function", "function": map[string]any{"name": "olp_certification_probe", "arguments": `{}`}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10}})
			return
		}
		label := func() string {
			switch {
			case bytes.Contains(body, []byte("orphaned-call")):
				return "orphaned-call"
			case bytes.Contains(body, []byte("live-call")):
				return "live-call"
			case bytes.Contains(body, []byte("heartbeat-loss")):
				return "heartbeat-loss"
			default:
				return ""
			}
		}()
		if label == "" {
			writeJSON(w, map[string]any{"id": "probe", "object": "chat.completion", "model": vendorModel, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10}})
			return
		}
		started <- label
		if label == "heartbeat-loss" {
			<-r.Context().Done()
			canceled <- label
			return
		}
		select {
		case <-release:
			writeJSON(w, map[string]any{"id": "completed", "object": "chat.completion", "model": vendorModel, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "completed"}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10}})
			delivered <- label
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(upstream.Close)
	provider := activeAzureProviderInProject(t, h, owner, "Workflow restart provider", upstream.URL, &project, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	providerID := mcpExecString(t, provider, "id")
	addBatchPrice(t, h, owner, providerID, "", time.Now().Add(-time.Minute))
	slug := "workflow-restart-recovery"
	draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{"slug": slug, "project_id": project, "operations": []string{"generation"}, "fidelity": map[string]any{"mode": "transformed"}, "tool_execution": "gateway", "overall_timeout_ms": 90000, "max_attempts": 1, "targets": []any{map[string]any{"provider_id": providerID, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 80000}}}, idem("workflow-restart-route"), http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+mcpExecString(t, draft, "id")+"/activate", nil, withMatch(draft, idem("workflow-restart-activate")), http.StatusOK)
	orphanKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Restart orphan", "project_id": project, "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem("workflow-restart-orphan"), http.StatusCreated)
	liveKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Restart live", "project_id": project, "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem("workflow-restart-live"), http.StatusCreated)
	env := continuationProcessEnvironment(t, h)
	worker := testutil.StartProcess(t, binary, "worker", env)
	_ = worker
	orphan := testutil.StartProcess(t, binary, "gateway", env)
	live := testutil.StartProcess(t, binary, "gateway", env)
	type workflowResponse struct {
		label  string
		status int
		body   []byte
		err    error
	}
	responses := make(chan workflowResponse, 2)
	start := func(origin, key, marker string) {
		go func() {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}],"max_tokens":16}`, slug, marker)))
			if err != nil {
				responses <- workflowResponse{label: marker, err: err}
				return
			}
			request.Header.Set("Authorization", "Bearer "+key)
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				responses <- workflowResponse{label: marker, err: err}
				return
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			responses <- workflowResponse{label: marker, status: response.StatusCode, body: body, err: err}
		}()
	}
	start(orphan.PublicOrigin, mcpExecString(t, orphanKey, "secret"), "orphaned-call")
	start(live.PublicOrigin, mcpExecString(t, liveKey, "secret"), "live-call")
	for range 2 {
		select {
		case <-started:
		case <-time.After(20 * time.Second):
			t.Fatal("both model requests did not reach the upstream")
		}
	}
	orphanID := mcpExecString(t, orphanKey, "id")
	liveID := mcpExecString(t, liveKey, "id")
	type observedDebit struct {
		state           string
		remainingCost   string
		remainingCharge string
	}
	readDebit := func(keyID string) (observedDebit, bool) {
		t.Helper()
		items := mcpExecItems(t, h.want(owner, http.MethodGet, "/api/v1/execution-debits?api_key_id="+keyID, nil, nil, http.StatusOK), "items")
		if len(items) != 1 {
			return observedDebit{}, false
		}
		item, ok := items[0].(map[string]any)
		if !ok {
			t.Fatalf("invalid debit: %v", items[0])
		}
		return observedDebit{state: mcpExecString(t, item, "state"), remainingCost: mcpExecString(t, item, "remaining_cost"), remainingCharge: mcpExecString(t, item, "remaining_charge")}, true
	}
	orphanBefore, exists := readDebit(orphanID)
	if !exists || orphanBefore.state != "dispatched" {
		t.Fatalf("model dispatch was not recorded: %v", orphanBefore)
	}
	if err := orphan.Kill(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "orphaned model debit to retain unknown exposure", 45*time.Second, func() bool {
		debit, ok := readDebit(orphanID)
		return ok && debit.state == "outcome_unknown"
	})
	liveDebit, exists := readDebit(liveID)
	if !exists || liveDebit.state != "dispatched" {
		t.Fatalf("live replica was marked orphaned: %v", liveDebit)
	}
	orphanDebit, exists := readDebit(orphanID)
	exposure, valid := new(big.Rat).SetString(orphanDebit.remainingCost)
	if !exists || !valid || exposure.Sign() <= 0 || orphanDebit.remainingCost != orphanBefore.remainingCost || orphanDebit.remainingCharge != orphanBefore.remainingCharge {
		t.Fatalf("orphaned dispatch lost exposure: %v", orphanDebit)
	}
	select {
	case unexpected := <-started:
		t.Fatalf("orphaned dispatch was retried: %s", unexpected)
	default:
	}
	if err := syscall.Kill(live.Pid(), syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(live.Pid(), syscall.SIGCONT) })
	eventually(t, "stopped replica's debit to retain unknown exposure", 45*time.Second, func() bool {
		debit, ok := readDebit(liveID)
		return ok && debit.state == "outcome_unknown"
	})
	close(release)
	select {
	case label := <-delivered:
		if label != "live-call" {
			t.Fatalf("unexpected response delivered after owner loss: %s", label)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live model did not produce its complete result")
	}
	if err := syscall.Kill(live.Pid(), syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := <-responses
		if response.label == "live-call" && (response.err != nil || response.status == http.StatusOK) {
			t.Fatalf("resumed producer returned success after losing ownership: status=%d body=%s error=%v", response.status, response.body, response.err)
		}
	}
	eventually(t, "late complete model evidence to settle once", 10*time.Second, func() bool {
		debit, ok := readDebit(liveID)
		return ok && debit.state == "settled"
	})
	fencedKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Heartbeat loss", "project_id": project, "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem("workflow-heartbeat-loss"), http.StatusCreated)
	fencedID := mcpExecString(t, fencedKey, "id")
	fenced := testutil.StartProcess(t, binary, "gateway", env)
	start(fenced.PublicOrigin, mcpExecString(t, fencedKey, "secret"), "heartbeat-loss")
	select {
	case label := <-started:
		if label != "heartbeat-loss" {
			t.Fatalf("unexpected model dispatch: %s", label)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("model did not receive the heartbeat-loss request")
	}
	if err := syscall.Kill(fenced.Pid(), syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(fenced.Pid(), syscall.SIGCONT) })
	eventually(t, "heartbeat loss to retain unknown exposure", 45*time.Second, func() bool {
		debit, ok := readDebit(fencedID)
		return ok && debit.state == "outcome_unknown"
	})
	if err := syscall.Kill(fenced.Pid(), syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case label := <-canceled:
		if label != "heartbeat-loss" {
			t.Fatalf("unexpected upstream cancellation: %s", label)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("producer did not cancel the upstream after losing its lease")
	}
	select {
	case response := <-responses:
		if response.label != "heartbeat-loss" || response.err != nil || response.status == http.StatusOK {
			t.Fatalf("unfenced producer response: %+v", response)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("producer did not stop after losing its lease")
	}
	if debit, ok := readDebit(fencedID); !ok || debit.state != "outcome_unknown" {
		t.Fatalf("lost producer exposure was not retained: %v", debit)
	}
	select {
	case unexpected := <-started:
		t.Fatalf("producer dispatched another model after losing its lease: %s", unexpected)
	default:
	}
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(upstreamURL.Port())
	if err != nil {
		t.Fatal(err)
	}
	upstream.Close()
	listener, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	listenerFile := os.NewFile(uintptr(listener), "blocked-model-listener")
	defer listenerFile.Close()
	if err := syscall.SetsockoptInt(listener, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(listener, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(listener, 1); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		connection, err := net.DialTimeout("tcp", upstreamURL.Host, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
	}
	unsentKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Restart unsent", "project_id": project, "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem("workflow-restart-unsent"), http.StatusCreated)
	unsentID := mcpExecString(t, unsentKey, "id")
	unsent := testutil.StartProcess(t, binary, "gateway", env)
	start(unsent.PublicOrigin, mcpExecString(t, unsentKey, "secret"), "unsent-call")
	eventually(t, "unconnected model dispatch to be fenced", 5*time.Second, func() bool {
		debit, ok := readDebit(unsentID)
		return ok && debit.state == "dispatched"
	})
	if err := syscall.Kill(unsent.Pid(), syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(unsent.Pid(), syscall.SIGCONT) })
	eventually(t, "unconnected dispatch to retain unknown exposure", 45*time.Second, func() bool {
		debit, ok := readDebit(unsentID)
		return ok && debit.state == "outcome_unknown"
	})
	if err := listenerFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(unsent.Pid(), syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-responses:
		if response.label != "unsent-call" || response.err != nil || response.status == http.StatusOK {
			t.Fatalf("resumed unsent producer returned success: %+v", response)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resumed producer did not finish")
	}
	eventually(t, "late proven-unsent dispatch to release exposure", 10*time.Second, func() bool {
		debit, ok := readDebit(unsentID)
		if !ok || debit.state != "not_accepted" {
			return false
		}
		remainingCost, validCost := new(big.Rat).SetString(debit.remainingCost)
		remainingCharge, validCharge := new(big.Rat).SetString(debit.remainingCharge)
		return validCost && validCharge && remainingCost.Sign() == 0 && remainingCharge.Sign() == 0
	})
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
