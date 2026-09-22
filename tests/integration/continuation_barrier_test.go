//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/secrets"
)

func continuationBarrierFixture(t *testing.T, h *accessHarness, run func(http.ResponseWriter, *http.Request, []byte)) (string, string) {
	t.Helper()
	return continuationBarrierFixtureOwner(t, h, h.owner(), run)
}

func continuationBarrierFixtureOwner(t *testing.T, h *accessHarness, owner *browser, run func(http.ResponseWriter, *http.Request, []byte)) (string, string) {
	t.Helper()
	f := &strictProviderFixture{profile: "anthropic-messages"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if bytes.Contains(body, []byte("Weather and time in Paris?")) {
			run(w, r, body)
			return
		}
		var input map[string]json.RawMessage
		_ = json.Unmarshal(body, &input)
		parityGeneration(w, "anthropic", string(input["stream"]) == "true")
	}))
	t.Cleanup(f.Close)
	options := map[string]any{"bindings": map[string]any{vendorModel: map[string]any{"model": "fixture-model"}}, "operation_defaults": map[string]any{"generation": map[string]any{"dialect": "anthropic-messages", "values": map[string]any{"max_tokens": 2048, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}}}}
	slug, _ := publishStrictProvider(t, h, owner, f, options, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	return slug, key
}
func continuationHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", "X-OLP-Continuation": continuationClientVersion, "X-OLP-Submission-ID": resources.SubmissionID(time.Now(), uuid.New())}
}
func TestPublicContinuationBarrierPrecedesActionableToolBytes(t *testing.T) {
	h := newAccessHarness(t)
	stream, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	waiting, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unlock()
	var calls atomic.Int64
	slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls.Add(1)
		// Claim+dispatch are already durable. Holding the installation key lock now
		// prevents the final encryption/ready transaction from committing.
		tx, err := h.Pool.Begin(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(r.Context(), `SELECT active_key_version FROM olp_go.installation WHERE singleton FOR UPDATE`); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream)
		_ = http.NewResponseController(w).Flush()
		close(waiting)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if err = tx.Commit(r.Context()); err != nil {
			t.Error(err)
		}
	})
	source := strings.Replace(continuationInput, "ROUTE", slug, 1)
	headers := continuationHeaders()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/v1/chat/completions", strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	action, complete, observed := make(chan bool, 1), make(chan error, 1), make(chan struct{}, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			complete <- err
			return
		}
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024), 1<<20)
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data: {")) {
				continue
			}
			select {
			case observed <- struct{}{}:
			default:
			}
			if bytes.Contains(line, []byte(`"tool_calls"`)) {
				var ready bool
				err = h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp_go.provider_resources r JOIN olp_go.secrets s ON s.id=r.id WHERE r.submission_id=$1 AND r.state='ready' AND s.purpose='provider_continuation')`, headers["X-OLP-Submission-ID"]).Scan(&ready)
				select {
				case action <- ready && err == nil:
				default:
				}
			}
		}
		complete <- scanner.Err()
	}()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not reach barrier")
	}
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("non-actionable observations did not stream")
	}
	select {
	case ready := <-action:
		t.Fatalf("actionable tool escaped blocked commit: ready=%t", ready)
	case <-time.After(100 * time.Millisecond):
	}
	var state string
	if err = h.Pool.QueryRow(t.Context(), `SELECT state FROM olp_go.provider_resources WHERE submission_id=$1`, headers["X-OLP-Submission-ID"]).Scan(&state); err != nil || state != resources.StateDispatching {
		t.Fatalf("uncommitted barrier state=%s err=%v", state, err)
	}
	unlock()
	select {
	case ready := <-action:
		if !ready {
			t.Fatal("tool preceded committed ciphertext")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ready tool not delivered")
	}
	select {
	case err := <-complete:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not finish")
	}
	if calls.Load() != 1 {
		t.Fatalf("provider dispatches=%d", calls.Load())
	}
}

func TestPublicContinuationFaultsNeverReplayUnknownInference(t *testing.T) {
	stream, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, body string }{
		{"terminal lost", strings.Replace(string(stream), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "", 1)},
		{"unknown terminal", strings.Replace(string(stream), `"stop_reason":"tool_use"`, `"stop_reason":"unknown-terminal"`, 1)},
		{"signature lost", strings.Replace(string(stream), "opaque-fixture-signature-do-not-log", "", 1)},
		{"event overflow", strings.Replace(string(stream), "fixture reasoning", strings.Repeat("x", 1<<17), 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAccessHarness(t)
			sink := &captureSink{}
			h.Gateway.Sink = sink
			var calls atomic.Int64
			slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
			})
			source := strings.Replace(continuationInput, "ROUTE", slug, 1)
			headers := continuationHeaders()
			_, body, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
			if bytes.Contains(body, []byte(`"tool_calls"`)) || bytes.Contains(body, []byte(`"handle"`)) {
				t.Fatalf("actionable output after incomplete dependency: %s", body)
			}
			first := sink.last()
			if len(first.Attempts) != 1 {
				t.Fatalf("attempts=%d", len(first.Attempts))
			}
			status, retry, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
			if status != 409 || calls.Load() != 1 || !bytes.Contains(retry, []byte("continuation_outcome_unknown")) {
				t.Fatalf("unknown work replayed: status=%d calls=%d body=%s", status, calls.Load(), retry)
			}
			status, recovery, _ := h.gatewayRaw("GET", "/v1/continuation-submissions/"+headers["X-OLP-Submission-ID"], key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
			if status != 409 || calls.Load() != 1 || !bytes.Contains(recovery, []byte("continuation_outcome_unknown")) {
				t.Fatalf("unsafe recovery: %d %s", status, recovery)
			}
			for _, event := range sink.all()[1:] {
				if len(event.Attempts) != 0 {
					t.Fatal("delivery recovery created another inference Attempt")
				}
			}
		})
	}
}

func TestPublicContinuationClaimFailureNeverDispatchesProvider(t *testing.T) {
	h := newAccessHarness(t)
	var calls atomic.Int64
	slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		calls.Add(1)
		http.Error(w, "unexpected provider dispatch", 500)
	})
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	badRing, err := secrets.ParseRing([]byte(`{"active_version":2,"keys":[{"version":2,"key":"abababababababababababababababababababababababababababababababab"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	h.Gateway.Resources = resources.NewEncrypted(h.Pool, installation, badRing)
	headers := continuationHeaders()
	source := strings.Replace(continuationInput, "ROUTE", slug, 1)
	status, body, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status < 400 || calls.Load() != 0 || !bytes.Contains(body, []byte("continuation_unavailable")) {
		t.Fatalf("failed encrypted claim dispatched: status=%d calls=%d %s", status, calls.Load(), body)
	}
	var claimed int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.provider_resources WHERE submission_id=$1`, headers["X-OLP-Submission-ID"]).Scan(&claimed); err != nil || claimed != 0 {
		t.Fatalf("failed encrypted claim left a dispatch journal: count=%d err=%v", claimed, err)
	}
}
