//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	gort "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/tests/fidelity"
)

// The frozen reference is a native-wire client with an independent persistence
// barrier. This candidate is the production negotiated Chat carrier with a Go
// SDK-equivalent assembler; separate public tests run both official SDKs.
const candidateContract = "negotiated-chat-anthropic-tools-v1/go-sdk-equivalent/1"

type candidateSample struct {
	workflow, firstEvent, toolVisible, actionReady                      time.Duration
	observations, finalObservations, nativeEvents, actions, readyChecks int
	submission                                                          string
}
type candidateRun struct {
	Name                  string             `json:"name"`
	Repetition            int                `json:"repetition"`
	Samples               int                `json:"samples"`
	Dispatches            int64              `json:"dispatches"`
	FirstRequests         int64              `json:"first_requests"`
	NextRequests          int64              `json:"next_requests"`
	NativeEvents          int                `json:"native_events"`
	FirstTurnObservations int                `json:"first_turn_observations"`
	FinalObservations     int                `json:"final_observations"`
	Actions               int                `json:"actions"`
	ReadyChecks           int                `json:"ready_checks"`
	Rejected              int                `json:"rejected"`
	HistoryBytes          int                `json:"history_bytes"`
	StateBytes            int                `json:"state_bytes"`
	Contract              string             `json:"contract"`
	Metrics               map[string]float64 `json:"metrics"`
}

func candidateSource(d barrierDocuments, slug string) ([]byte, error) {
	var native map[string]json.RawMessage
	if err := json.Unmarshal(d.initial, &native); err != nil {
		return nil, err
	}
	var nativeTools []map[string]json.RawMessage
	if err := json.Unmarshal(native["tools"], &nativeTools); err != nil {
		return nil, err
	}
	tools := make([]map[string]json.RawMessage, 0, len(nativeTools))
	for _, tool := range nativeTools {
		function := map[string]json.RawMessage{"name": tool["name"], "parameters": tool["input_schema"]}
		if value, ok := tool["description"]; ok {
			function["description"] = value
		}
		encoded, err := json.Marshal(function)
		if err != nil {
			return nil, err
		}
		tools = append(tools, map[string]json.RawMessage{"type": json.RawMessage(`"function"`), "function": encoded})
	}
	var nativeMessages []json.RawMessage
	if err := json.Unmarshal(native["messages"], &nativeMessages); err != nil {
		return nil, err
	}
	if len(nativeMessages) != 1 {
		return nil, fmt.Errorf("invalid frozen first turn")
	}
	return json.Marshal(map[string]any{"model": slug, "stream": true, "tools": tools, "messages": nativeMessages})
}
func candidateNext(first []byte, assistant json.RawMessage, results []json.RawMessage) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(first, &request); err != nil {
		return nil, err
	}
	var history []json.RawMessage
	if err := json.Unmarshal(request["messages"], &history); err != nil {
		return nil, err
	}
	delete(request, "stream")
	delete(request, "stream_options")
	history = append(history, assistant)
	history = append(history, results...)
	request["messages"], _ = json.Marshal(history)
	return json.Marshal(request)
}
func candidatePost(ctx context.Context, client *http.Client, origin, key string, body []byte, submission, handle string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OLP-Continuation", continuationClientVersion)
	req.Header.Set("X-OLP-Submission-ID", submission)
	if handle != "" {
		req.Header.Set("X-OLP-Continuation-Handle", handle)
	}
	return client.Do(req)
}
func candidateRecover(ctx context.Context, client *http.Client, origin, key, submission string) (string, json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/v1/continuation-submissions/"+submission, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-OLP-Continuation", continuationClientVersion)
	response, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != http.StatusOK || bytes.Contains(body, []byte("opaque-fixture-signature-do-not-log")) {
		return "", nil, fmt.Errorf("no authenticated ready continuation: status=%d err=%v", response.StatusCode, err)
	}
	var state struct {
		Version   string          `json:"version"`
		State     string          `json:"state"`
		Handle    string          `json:"handle"`
		Assistant json.RawMessage `json:"assistant"`
	}
	if json.Unmarshal(body, &state) != nil || state.Version != continuationClientVersion || state.State != "ready" || state.Handle == "" || len(state.Assistant) == 0 {
		return "", nil, fmt.Errorf("invalid recoverable delivery")
	}
	return state.Handle, state.Assistant, nil
}

func candidateWorkflow(ctx context.Context, d barrierDocuments, client *http.Client, origin, key, slug string) (candidateSample, error) {
	var sample candidateSample
	start := time.Now()
	first, err := candidateSource(d, slug)
	if err != nil {
		return sample, err
	}
	submission := resources.SubmissionID(time.Now(), uuid.New())
	sample.submission = submission
	response, err := candidatePost(ctx, client, origin, key, first, submission, "")
	if err != nil {
		return sample, err
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		return sample, fmt.Errorf("translated initial status %d: %s", response.StatusCode, body)
	}
	var content strings.Builder
	var toolCalls []json.RawMessage
	var readyHandle string
	var readyAssistant json.RawMessage
	var finished, done bool
	var starts []string
	reader := bufio.NewReader(response.Body)
	for {
		line, e := reader.ReadBytes('\n')
		if len(line) > 0 && bytes.HasPrefix(line, []byte("data: ")) {
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data: ")))
			if bytes.Equal(data, []byte("[DONE]")) {
				done = true
			} else {
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content   string            `json:"content"`
							ToolCalls []json.RawMessage `json:"tool_calls"`
						} `json:"delta"`
						Finish string `json:"finish_reason"`
					} `json:"choices"`
					OLP struct {
						Version     string `json:"version"`
						Observation *struct {
							Type  string `json:"type"`
							Phase string `json:"phase"`
						} `json:"observation"`
						Ready  bool   `json:"ready"`
						Handle string `json:"handle"`
					} `json:"olp"`
				}
				if err := json.Unmarshal(data, &chunk); err != nil || chunk.OLP.Version != continuationClientVersion || len(chunk.Choices) != 1 {
					response.Body.Close()
					return sample, fmt.Errorf("invalid negotiated SDK chunk")
				}
				if sample.firstEvent == 0 {
					sample.firstEvent = time.Since(start)
				}
				if chunk.OLP.Observation != nil {
					sample.observations++
					if chunk.OLP.Observation.Phase == "start" {
						starts = append(starts, chunk.OLP.Observation.Type)
					}
				}
				choice := chunk.Choices[0]
				content.WriteString(choice.Delta.Content)
				for _, rawCall := range choice.Delta.ToolCalls {
					if sample.toolVisible == 0 {
						sample.toolVisible = time.Since(start)
						readyHandle, readyAssistant, err = candidateRecover(ctx, client, origin, key, submission)
						if err != nil {
							response.Body.Close()
							return sample, err
						}
						sample.readyChecks++
						sample.actionReady = time.Since(start)
					}
					var call map[string]json.RawMessage
					if json.Unmarshal(rawCall, &call) != nil {
						response.Body.Close()
						return sample, fmt.Errorf("malformed projected tool call")
					}
					var index int
					if json.Unmarshal(call["index"], &index) != nil || index != len(toolCalls) {
						response.Body.Close()
						return sample, fmt.Errorf("reordered projected tool call")
					}
					delete(call, "index")
					encoded, _ := json.Marshal(call)
					toolCalls = append(toolCalls, encoded)
				}
				if choice.Finish != "" {
					if choice.Finish != "tool_calls" || !chunk.OLP.Ready || chunk.OLP.Handle != readyHandle {
						response.Body.Close()
						return sample, fmt.Errorf("uncommitted or changed terminal handle")
					}
					finished = true
				}
			}
		}
		if e != nil {
			if e != io.EOF {
				response.Body.Close()
				return sample, e
			}
			break
		}
	}
	response.Body.Close()
	if !done || !finished || sample.observations != 13 || sample.readyChecks != 1 || sample.actionReady < sample.toolVisible || content.String() != "beforeafter" || strings.Join(starts, ",") != "thinking,text,tool_use,tool_use,text" || len(toolCalls) != 2 {
		return sample, fmt.Errorf("incomplete negotiated observations or tool barrier")
	}
	assistant, _ := json.Marshal(map[string]any{"role": "assistant", "content": content.String(), "tool_calls": toolCalls})
	if err := fidelity.Compare(readyAssistant, assistant); err != nil {
		return sample, fmt.Errorf("recoverable assistant differs: %w", err)
	}
	var nativeAssistant struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(d.assistant, &nativeAssistant); err != nil {
		return sample, err
	}
	var results []json.RawMessage
	for _, rawCall := range toolCalls {
		var call struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if err := json.Unmarshal(rawCall, &call); err != nil {
			return sample, err
		}
		found := false
		for _, block := range nativeAssistant.Content {
			if block.Type != "tool_use" || block.ID != call.ID || block.Name != call.Function.Name {
				continue
			}
			if err := fidelity.Compare(block.Input, []byte(call.Function.Arguments)); err != nil {
				return sample, fmt.Errorf("projected tool argument differs: %w", err)
			}
			found = true
		}
		if !found {
			return sample, fmt.Errorf("unknown projected tool identity")
		}
		result := ""
		switch call.ID {
		case "call-weather":
			result = "sunny"
		case "call-clock":
			result = "14:00"
		default:
			return sample, fmt.Errorf("unsupported fixture action")
		}
		encoded, _ := json.Marshal(map[string]string{"role": "tool", "tool_call_id": call.ID, "content": result})
		results = append(results, encoded)
		sample.actions++
	}
	next, err := candidateNext(first, assistant, results)
	if err != nil {
		return sample, err
	}
	response, err = candidatePost(ctx, client, origin, key, next, resources.SubmissionID(time.Now(), uuid.New()), readyHandle)
	if err != nil {
		return sample, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != http.StatusOK {
		return sample, fmt.Errorf("translated next status %d: %s; %v", response.StatusCode, body, err)
	}
	var direct struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(d.final, &direct); err != nil {
		return sample, err
	}
	var final struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role      string            `json:"role"`
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		OLP struct {
			Ready        bool              `json:"ready"`
			Version      string            `json:"version"`
			Handle       string            `json:"handle"`
			Observations []json.RawMessage `json:"observations"`
		} `json:"olp"`
	}
	if json.Unmarshal(body, &final) != nil || len(direct.Content) != 1 || direct.Content[0].Type != "text" || direct.StopReason != "end_turn" || direct.Model != "fixture-model" || direct.Usage.InputTokens != 64 || direct.Usage.OutputTokens != 8 || len(final.Choices) != 1 || final.ID != direct.ID || final.Model != slug || final.Choices[0].Message.Role != "assistant" || final.Choices[0].Message.Content != direct.Content[0].Text || len(final.Choices[0].Message.ToolCalls) != 0 || final.Choices[0].Finish != "stop" || final.Usage.PromptTokens != direct.Usage.InputTokens || final.Usage.CompletionTokens != direct.Usage.OutputTokens || final.Usage.TotalTokens != direct.Usage.InputTokens+direct.Usage.OutputTokens || !final.OLP.Ready || final.OLP.Version != continuationClientVersion || final.OLP.Handle == "" || len(final.OLP.Observations) != 2 || bytes.Contains(body, []byte("opaque-fixture-signature-do-not-log")) {
		return sample, fmt.Errorf("incomplete final SDK result")
	}
	for i, raw := range final.OLP.Observations {
		var observation struct {
			Index int    `json:"index"`
			Type  string `json:"type"`
			Phase string `json:"phase"`
			Text  string `json:"text"`
		}
		if json.Unmarshal(raw, &observation) != nil || observation.Index != 0 || observation.Type != "text" || observation.Phase != []string{"start", "end"}[i] || i == 0 && observation.Text != direct.Content[0].Text || i == 1 && observation.Text != "" {
			return sample, fmt.Errorf("final SDK observations lost native text boundary")
		}
	}
	sample.finalObservations = len(final.OLP.Observations)
	sample.nativeEvents = 19 // Fixed provider stream; ready requires its terminal.
	sample.workflow = time.Since(start)
	return sample, nil
}

func TestNegotiatedContinuationCandidateBenchmark(t *testing.T) {
	h := newAccessHarness(t)
	documents := []barrierDocuments{barrierCorpus(t, 0), barrierCorpus(t, 256<<10)}
	provider := newBarrierProvider(t, documents)
	fixture := &strictProviderFixture{Server: provider.Server, profile: "anthropic-messages"}
	options := map[string]any{"bindings": map[string]any{vendorModel: map[string]any{"model": "fixture-model"}}, "operation_defaults": map[string]any{"generation": map[string]any{"dialect": "anthropic-messages", "values": map[string]any{"max_tokens": 2048, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}}}}
	owner := h.owner()
	slug, _ := publishStrictProvider(t, h, owner, fixture, options, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	provider.measured.Store(true)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	var version string
	var tls bool
	if err := h.Pool.QueryRow(t.Context(), "SHOW server_version_num").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()),false)").Scan(&tls); err != nil {
		t.Fatal(err)
	}
	setup, _ := json.Marshal(map[string]any{"postgresql_server_version": version, "postgresql_tls": tls})
	t.Logf("CANDIDATE_SETUP %s", setup)
	repetitions, samples := 1, 8
	if os.Getenv("OLP_CONTINUATION_CANDIDATE_MEASURE") == "1" {
		repetitions, samples = 3, 24
	}
	for _, d := range documents {
		size := "small"
		if d.historyBytes > 0 {
			size = "large"
		}
		for _, concurrency := range []int{1, 8} {
			for repeat := range repetitions {
				warmup, err := candidateWorkflow(t.Context(), d, client, h.HTTP.URL, key, slug)
				if err != nil {
					t.Fatal(err)
				}
				var stateBytes int
				if err := h.Pool.QueryRow(t.Context(), `SELECT octet_length(s.ciphertext) FROM olp_go.provider_resources r JOIN olp_go.secrets s ON s.id=r.id WHERE r.submission_id=$1 AND r.state='ready'`, warmup.submission).Scan(&stateBytes); err != nil || stateBytes <= d.historyBytes {
					t.Fatalf("missing complete encrypted candidate state: bytes=%d err=%v", stateBytes, err)
				}
				gort.GC()
				var before, after gort.MemStats
				gort.ReadMemStats(&before)
				var peak atomic.Uint64
				peak.Store(before.HeapAlloc)
				stop, stopped := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(stopped)
					ticker := time.NewTicker(time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-stop:
							return
						case <-ticker.C:
							var m gort.MemStats
							gort.ReadMemStats(&m)
							peak.Store(max(peak.Load(), m.HeapAlloc))
						}
					}
				}()
				first, next, cpu, started := provider.first.Load(), provider.next.Load(), lifecycleCPU(), time.Now()
				results := make([]candidateSample, samples)
				errs := make(chan error, samples)
				var wg sync.WaitGroup
				for worker := range concurrency {
					wg.Go(func() {
						for i := worker; i < samples; i += concurrency {
							ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
							sample, runErr := candidateWorkflow(ctx, d, client, h.HTTP.URL, key, slug)
							cancel()
							results[i] = sample
							if runErr != nil {
								errs <- runErr
							}
						}
					})
				}
				wg.Wait()
				elapsed := time.Since(started)
				cpu = lifecycleCPU() - cpu
				first, next = provider.first.Load()-first, provider.next.Load()-next
				close(stop)
				<-stopped
				gort.ReadMemStats(&after)
				peak.Store(max(peak.Load(), after.HeapAlloc))
				close(errs)
				for err := range errs {
					t.Fatal(err)
				}
				if first != int64(samples) || next != int64(samples) {
					t.Fatalf("native dispatch inventory changed: %d/%d, expected %d/%d", first, next, samples, samples)
				}
				run := candidateRun{Name: fmt.Sprintf("%s/c%d/translated", size, concurrency), Repetition: repeat, Samples: samples, Dispatches: first + next, FirstRequests: first, NextRequests: next, HistoryBytes: d.historyBytes, StateBytes: stateBytes, Contract: candidateContract, Metrics: map[string]float64{"elapsed_ns": float64(elapsed), "ns/op": float64(elapsed) / float64(samples), "process-cpu-ns/op": float64(cpu) / float64(samples), "B/op": float64(after.TotalAlloc-before.TotalAlloc) / float64(samples), "allocs/op": float64(after.Mallocs-before.Mallocs) / float64(samples), "sampled-heap-growth-B": float64(peak.Load() - before.HeapAlloc)}}
				values := map[string][]time.Duration{"workflow": {}, "first-event": {}, "tool-visible": {}, "action-ready": {}}
				for _, sample := range results {
					run.NativeEvents += sample.nativeEvents
					run.FirstTurnObservations += sample.observations
					run.FinalObservations += sample.finalObservations
					run.Actions += sample.actions
					run.ReadyChecks += sample.readyChecks
					values["workflow"] = append(values["workflow"], sample.workflow)
					values["first-event"] = append(values["first-event"], sample.firstEvent)
					values["tool-visible"] = append(values["tool-visible"], sample.toolVisible)
					values["action-ready"] = append(values["action-ready"], sample.actionReady)
				}
				if run.NativeEvents != samples*19 || run.FirstTurnObservations != samples*13 || run.FinalObservations != samples*2 || run.Actions != samples*2 || run.ReadyChecks != samples {
					t.Fatal("candidate semantic coverage changed")
				}
				for name, observations := range values {
					for _, p := range []int{50, 95, 99} {
						run.Metrics[fmt.Sprintf("%s-p%d-us", name, p)] = lifecyclePercentile(observations, p)
					}
				}
				raw, _ := json.Marshal(run)
				t.Logf("CANDIDATE_MEASUREMENT %s", raw)
			}
		}
	}
}
