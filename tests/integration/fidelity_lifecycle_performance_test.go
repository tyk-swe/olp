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
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
)

// These scripted native references do not call production protocol codecs. The
// measured process includes client, provider, gateway and independent checking;
// PostgreSQL is a separate required local service. No model-quality claim follows.
const lifecycleText = "lifecycle-fidelity-0123456789"
const lifecycleEvents = 64

type lifecycleSample struct {
	elapsed, publication, cancellation time.Duration
	roundTrips                         []time.Duration
}
type lifecycleRun struct {
	Name       string             `json:"name"`
	Repetition int                `json:"repetition"`
	Samples    int                `json:"samples"`
	Succeeded  int                `json:"succeeded"`
	Dispatches int64              `json:"dispatches"`
	Events     int                `json:"events_per_request"`
	Metrics    map[string]float64 `json:"metrics"`
}
type lifecycleFixture struct {
	*httptest.Server
	sends      sync.Map
	closed     sync.Map
	dispatches atomic.Int64
	measuring  atomic.Bool
}

func lifecycleInput() any {
	return []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": lifecycleText}}}}
}

func lifecycleResponse(id, model, text string) map[string]any {
	return map[string]any{"id": id, "object": "response", "created_at": float64(1), "model": model, "status": "completed", "error": nil, "incomplete_details": nil, "output": []any{map[string]any{"id": "msg_fidelity", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}, "usage": map[string]any{"input_tokens": float64(4), "output_tokens": float64(64), "total_tokens": float64(68)}}
}
func lifecycleWire(i int, reply bool) []byte {
	if i == 0 {
		if reply {
			return []byte(`{"type":"session.updated","event_id":"event_0","session":{"id":"session_fixture","modalities":["audio","text"],"input_audio_format":"pcm16","output_audio_format":"pcm16"}}`)
		}
		return []byte(`{"type":"session.update","event_id":"event_0","session":{"modalities":["audio","text"],"input_audio_format":"pcm16","output_audio_format":"pcm16"}}`)
	}
	if reply {
		return []byte(fmt.Sprintf(`{"type":"response.audio.delta","event_id":"event_%d","response_id":"response_fixture","item_id":"item_fixture","output_index":0,"content_index":0,"delta":"%s"}`, i, strings.Repeat("AQID", 256)))
	}
	return []byte(fmt.Sprintf(`{"type":"input_audio_buffer.append","event_id":"event_%d","audio":"%s"}`, i, strings.Repeat("AQID", 256)))
}
func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	f := &lifecycleFixture{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != vendorSecret {
			http.Error(w, "provider credential", 401)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/openai/v1/models" {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		if r.URL.Path == "/openai/v1/realtime" {
			if r.URL.Query().Get("model") != vendorModel {
				http.Error(w, "model", 400)
				return
			}
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			c.SetReadLimit(64 << 10)
			f.dispatches.Add(1)
			// The first fixed native event establishes the session; a provider-selected
			// connection identifier is returned out of band only to this local harness.
			id := uuid.NewString()
			done := make(chan struct{})
			f.closed.Store(id, done)
			defer close(done)
			if err := c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created","session":{"id":"`+id+`","model":"`+vendorModel+`"}}`)); err != nil {
				return
			}
			for i := range lifecycleEvents {
				typ, data, err := c.Read(r.Context())
				if err != nil {
					return
				}
				if typ != websocket.MessageText || !bytes.Equal(data, lifecycleWire(i, false)) {
					t.Error("native realtime input changed")
					return
				}
				if err := c.Write(r.Context(), websocket.MessageText, lifecycleWire(i, true)); err != nil {
					return
				}
			}
			// Wait for cancellation; returning earlier would make propagation vacuous.
			if _, _, err := c.Read(r.Context()); err == nil {
				t.Error("unexpected extra realtime event")
			}
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/openai/v1/responses" {
			http.NotFound(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			return
		}
		var req map[string]any
		if json.Unmarshal(raw, &req) != nil {
			http.Error(w, "json", 400)
			return
		}
		if !f.measuring.Load() {
			writeResponsesFixture(w, vendorModel, vendorAnswer, req["stream"] == true)
			return
		} // public certification, outside measurements
		stream := req["stream"] == true
		want := map[string]any{"model": vendorModel, "input": lifecycleInput(), "store": true, "stream": stream}
		if !reflect.DeepEqual(req, want) {
			t.Errorf("native durable request changed: %s", raw)
			http.Error(w, "request changed", 400)
			return
		}
		n := f.dispatches.Add(1)
		id := fmt.Sprintf("resp_fidelity_%09d", n)
		f.sends.Store(id, time.Now())
		result := lifecycleResponse(id, vendorModel, lifecycleText)
		if !stream {
			writeJSON(w, result)
			return
		}
		emit := func(kind string, value any) {
			body, _ := json.Marshal(value)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, body)
			w.(http.Flusher).Flush()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit("response.created", map[string]any{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": id, "object": "response", "created_at": 1, "model": vendorModel, "status": "in_progress", "output": []any{}}})
		for i := range lifecycleEvents {
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "sequence_number": i + 1, "item_id": "msg_fidelity", "output_index": 0, "content_index": 0, "delta": lifecycleText})
		}
		result = lifecycleResponse(id, vendorModel, strings.Repeat(lifecycleText, lifecycleEvents))
		emit("response.completed", map[string]any{"type": "response.completed", "sequence_number": lifecycleEvents + 1, "response": result})
	}))
	t.Cleanup(f.Close)
	return f
}

func lifecycleProvision(t *testing.T, h *accessHarness, endpoint string) (string, string) {
	t.Helper()
	owner := h.owner()
	cfg := map[string]any{"kind": "azure_openai", "auth_mode": "api_key", "endpoint": endpoint, "deployment": vendorModel, "profile_id": "azure-v1-responses", "profile_revision": "1"}
	p := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "lifecycle benchmark", "configuration": cfg, "model": vendorModel, "credential": vendorSecret}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	path := "/api/v3/providers/" + p["id"].(string)
	if probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(p), 200); probe["succeeded"] != true {
		t.Fatalf("probe: %v", probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	caps := []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}, map[string]any{"operation": "realtime", "surface": "openai", "mode": "realtime"}}
	p = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": caps}, etagHeader(p), 200)
	if proof := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(p), 200); proof["status"] != "certified" {
		t.Fatalf("certification: %v", proof)
	}
	p = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(p, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	slug := "fidelity-lifecycle"
	draft := map[string]any{"slug": slug, "operations": []string{"generation", "realtime"}, "overall_timeout_ms": 30000, "max_attempts": 1, "targets": []any{map[string]any{"provider_id": p["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 20000}}}
	if raw := os.Getenv("OLP_LIFECYCLE_ROUTE_FIDELITY"); raw != "" {
		var v map[string]any
		if json.Unmarshal([]byte(raw), &v) != nil || len(v) != 1 || (v["mode"] != "legacy" && v["mode"] != "strict") {
			t.Fatal("explicit fidelity must contain exactly a legacy or strict mode")
		}
		draft["fidelity"] = v
	}
	d := h.want(owner, "POST", "/api/v3/route-drafts", draft, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+d["id"].(string)+"/activate", nil, withMatch(d, map[string]string{"Idempotency-Key": uuid.NewString()}), 200)
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	return slug, key
}

func lifecycleRelay(t *testing.T, f *lifecycleFixture) *httptest.Server {
	t.Helper()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	client := policy.Client(30 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer lifecycle-reference-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		if _, err := policy.ValidateEndpoint(f.URL); err != nil {
			http.Error(w, "egress", 403)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/realtime" {
			if r.URL.Query().Get("model") != vendorModel {
				http.Error(w, "model", 403)
				return
			}
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			upstream, _, err := websocket.Dial(ctx, strings.Replace(f.URL, "http://", "ws://", 1)+"/openai/v1/realtime?model="+vendorModel, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Api-Key": {vendorSecret}}})
			if err != nil {
				http.Error(w, "provider", 502)
				return
			}
			defer upstream.CloseNow()
			upstream.SetReadLimit(64 << 10)
			downstream, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer downstream.CloseNow()
			downstream.SetReadLimit(64 << 10)
			done := make(chan struct{}, 2)
			forward := func(dst, src *websocket.Conn) {
				defer func() { done <- struct{}{} }()
				for {
					kind, data, e := src.Read(ctx)
					if e != nil {
						return
					}
					if dst.Write(ctx, kind, data) != nil {
						return
					}
				}
			}
			go forward(upstream, downstream)
			go forward(downstream, upstream)
			<-done
			cancel()
			upstream.CloseNow()
			downstream.CloseNow()
			<-done
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, f.URL+"/openai/v1/responses", http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			return
		}
		req.Header.Set("Api-Key", vendorSecret)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "provider", 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		reader := bufio.NewReader(io.LimitReader(resp.Body, 1<<20))
		for {
			line, e := reader.ReadBytes('\n')
			if len(line) > 0 {
				if _, err := w.Write(line); err != nil {
					return
				}
				if len(bytes.TrimSpace(line)) == 0 {
					w.(http.Flusher).Flush()
				}
			}
			if e != nil {
				return
			}
		}
	}))
	t.Cleanup(relay.Close)
	return relay
}

func lifecycleCheckResult(raw []byte, id, model, text string) error {
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		return err
	}
	if !reflect.DeepEqual(got, lifecycleResponse(id, model, text)) {
		return fmt.Errorf("complete native result changed: %s", raw)
	}
	return nil
}
func lifecycleExpectedEvent(index int, id, model string) (string, map[string]any) {
	if index == 0 {
		return "response.created", map[string]any{"type": "response.created", "sequence_number": float64(0), "response": map[string]any{"id": id, "object": "response", "created_at": float64(1), "model": model, "status": "in_progress", "output": []any{}}}
	}
	if index <= lifecycleEvents {
		return "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "sequence_number": float64(index), "item_id": "msg_fidelity", "output_index": float64(0), "content_index": float64(0), "delta": lifecycleText}
	}
	return "response.completed", map[string]any{"type": "response.completed", "sequence_number": float64(lifecycleEvents + 1), "response": lifecycleResponse(id, model, strings.Repeat(lifecycleText, lifecycleEvents))}
}
func lifecycleCheckEvent(raw []byte, eventName string, index int, id, model string) error {
	if index > lifecycleEvents+1 {
		return fmt.Errorf("extra stream event")
	}
	kind, want := lifecycleExpectedEvent(index, id, model)
	var got map[string]any
	if json.Unmarshal(raw, &got) != nil || eventName != kind || !reflect.DeepEqual(got, want) {
		return fmt.Errorf("complete event %d changed: %s", index, raw)
	}
	return nil
}

func lifecycleRequest(ctx context.Context, h *accessHarness, f *lifecycleFixture, client *http.Client, endpoint, key, model, workload string, relay bool) (lifecycleSample, error) {
	var sample lifecycleSample
	start := time.Now()
	if strings.HasPrefix(workload, "duplex") {
		c, _, err := websocket.Dial(ctx, strings.Replace(endpoint, "http://", "ws://", 1)+"/v1/realtime?model="+model, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
		if err != nil {
			return sample, err
		}
		defer c.CloseNow()
		c.SetReadLimit(64 << 10)
		_, raw, err := c.Read(ctx)
		if err != nil {
			return sample, err
		}
		var created struct {
			Type    string                     `json:"type"`
			Session struct{ ID, Model string } `json:"session"`
		}
		if json.Unmarshal(raw, &created) != nil || created.Type != "session.created" || created.Session.Model != vendorModel {
			return sample, fmt.Errorf("session identity changed: %s", raw)
		}
		var actualCreated any
		json.Unmarshal(raw, &actualCreated)
		expectedCreated := map[string]any{"type": "session.created", "session": map[string]any{"id": created.Session.ID, "model": vendorModel}}
		if !reflect.DeepEqual(actualCreated, expectedCreated) {
			return sample, fmt.Errorf("complete session event changed")
		}
		value, ok := f.closed.LoadAndDelete(created.Session.ID)
		if !ok {
			return sample, fmt.Errorf("unknown session")
		}
		closed := value.(chan struct{})
		for i := range lifecycleEvents {
			sent := time.Now()
			if err := c.Write(ctx, websocket.MessageText, lifecycleWire(i, false)); err != nil {
				return sample, err
			}
			if workload == "duplex_slow_64" {
				time.Sleep(500 * time.Microsecond)
			}
			kind, got, err := c.Read(ctx)
			if err != nil {
				return sample, err
			}
			if kind != websocket.MessageText || !bytes.Equal(got, lifecycleWire(i, true)) {
				return sample, fmt.Errorf("duplex event %d changed", i)
			}
			sample.roundTrips = append(sample.roundTrips, time.Since(sent))
		}
		sample.elapsed = time.Since(start)
		closing := time.Now()
		c.CloseNow()
		select {
		case <-closed:
			sample.cancellation = time.Since(closing)
		case <-ctx.Done():
			return sample, ctx.Err()
		}
		return sample, nil
	}
	stream := workload == "durable_stream_64"
	input, _ := json.Marshal(lifecycleInput())
	body := fmt.Sprintf(`{"model":%q,"input":%s,"store":true,"stream":%t}`, model, input, stream)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/responses", strings.NewReader(body))
	if err != nil {
		return sample, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return sample, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return sample, fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	var localID, upstreamID string
	observe := func(id string, at time.Time) error {
		localID = id
		upstreamID = id
		if !relay {
			if !strings.HasPrefix(id, "response_") {
				return fmt.Errorf("unmapped durable id")
			}
			if err := h.Pool.QueryRow(ctx, `SELECT upstream_id FROM olp_go.provider_resources WHERE kind='response' AND replace(id::text,'-','')=$1`, strings.TrimPrefix(id, "response_")).Scan(&upstreamID); err != nil {
				return fmt.Errorf("observed ID not durable: %w", err)
			}
		}
		v, ok := f.sends.LoadAndDelete(upstreamID)
		if !ok {
			return fmt.Errorf("missing provider emission")
		}
		sample.publication = at.Sub(v.(time.Time))
		return nil
	}
	if !stream {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		sample.elapsed = time.Since(start)
		if err != nil {
			return sample, err
		}
		var out struct{ ID string }
		if json.Unmarshal(raw, &out) != nil {
			return sample, fmt.Errorf("invalid response")
		}
		if err := observe(out.ID, start.Add(sample.elapsed)); err != nil {
			return sample, err
		}
		return sample, lifecycleCheckResult(raw, localID, model, lifecycleText)
	}
	reader := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	reader.Buffer(make([]byte, 4096), 64<<10)
	index := 0
	eventName := ""
	haveData := false
	for reader.Scan() {
		line := reader.Text()
		if line == "" {
			if !haveData {
				return sample, fmt.Errorf("empty or partial SSE frame")
			}
			eventName = ""
			haveData = false
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			if eventName != "" || haveData {
				return sample, fmt.Errorf("duplicate SSE event")
			}
			eventName = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") || eventName == "" || haveData {
			return sample, fmt.Errorf("unexpected SSE framing")
		}
		raw := []byte(strings.TrimPrefix(line, "data: "))
		if index == 0 {
			var created struct{ Response struct{ ID string } }
			if json.Unmarshal(raw, &created) != nil {
				return sample, fmt.Errorf("invalid created event")
			}
			if err := observe(created.Response.ID, time.Now()); err != nil {
				return sample, err
			}
		}
		if err := lifecycleCheckEvent(raw, eventName, index, localID, model); err != nil {
			return sample, err
		}
		index++
		haveData = true
	}
	if haveData || eventName != "" {
		return sample, fmt.Errorf("unterminated SSE frame")
	}

	sample.elapsed = time.Since(start)
	if reader.Err() != nil {
		return sample, reader.Err()
	}
	if index != lifecycleEvents+2 {
		return sample, fmt.Errorf("stream incomplete: %d", index)
	}
	return sample, nil
}

func lifecycleCPU() int64 {
	var r syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r)
	return r.Utime.Nano() + r.Stime.Nano()
}
func lifecyclePercentile(values []time.Duration, p int) float64 {
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return float64(values[(len(values)-1)*p/100]) / float64(time.Microsecond)
}

func TestFidelityLifecyclePerformance(t *testing.T) {
	h := newAccessHarness(t)
	var postgres string
	var tls bool
	if err := h.Pool.QueryRow(t.Context(), "SHOW server_version_num").Scan(&postgres); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid()),false)").Scan(&tls); err != nil {
		t.Fatal(err)
	}
	setup, _ := json.Marshal(map[string]any{"postgresql_server_version": postgres, "postgresql_tls": tls})
	t.Logf("LIFECYCLE_SETUP %s", setup)
	fixture := newLifecycleFixture(t)
	slug, key := lifecycleProvision(t, h, fixture.URL)
	fixture.measuring.Store(true)
	relay := lifecycleRelay(t, fixture)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	repetitions, samples := 1, 4
	if os.Getenv("OLP_LIFECYCLE_MEASURE") == "1" {
		repetitions, samples = 3, 24
	}
	for _, workload := range []string{"durable_unary", "durable_stream_64", "duplex_64", "duplex_slow_64"} {
		for _, concurrency := range []int{1, 4} {
			for _, path := range []string{"relay", "gateway"} {
				endpoint, secret, model := h.HTTP.URL, key, slug
				if path == "relay" {
					endpoint, secret, model = relay.URL, "lifecycle-reference-key", vendorModel
				}
				for repeat := range repetitions {
					// One independently validated warmup per path/repetition, outside counters.
					if _, err := lifecycleRequest(t.Context(), h, fixture, client, endpoint, secret, model, workload, path == "relay"); err != nil {
						t.Fatal(err)
					}
					goruntime.GC()
					var before, after goruntime.MemStats
					goruntime.ReadMemStats(&before)
					var peak atomic.Uint64
					peak.Store(before.HeapAlloc)
					stop := make(chan struct{})
					stopped := make(chan struct{})
					go func() {
						defer close(stopped)
						ticker := time.NewTicker(time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-stop:
								return
							case <-ticker.C:
								var m goruntime.MemStats
								goruntime.ReadMemStats(&m)
								peak.Store(max(peak.Load(), m.HeapAlloc))
							}
						}
					}()
					dispatches, cpu, start := fixture.dispatches.Load(), lifecycleCPU(), time.Now()
					results := make([]lifecycleSample, samples)
					errs := make(chan error, samples)
					var wg sync.WaitGroup
					for worker := range concurrency {
						wg.Go(func() {
							for i := worker; i < samples; i += concurrency {
								ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
								sample, err := lifecycleRequest(ctx, h, fixture, client, endpoint, secret, model, workload, path == "relay")
								cancel()
								if err != nil {
									errs <- err
								}
								results[i] = sample
							}
						})
					}
					wg.Wait()
					elapsed := time.Since(start)
					cpu = lifecycleCPU() - cpu
					dispatches = fixture.dispatches.Load() - dispatches
					close(stop)
					<-stopped
					goruntime.ReadMemStats(&after)
					peak.Store(max(peak.Load(), after.HeapAlloc))
					close(errs)
					for err := range errs {
						t.Fatal(err)
					}
					if dispatches != int64(samples) {
						t.Fatalf("dispatch count changed: %d != %d", dispatches, samples)
					}
					run := lifecycleRun{Name: fmt.Sprintf("%s/c%d/%s", workload, concurrency, path), Repetition: repeat, Samples: samples, Succeeded: samples, Dispatches: dispatches, Metrics: map[string]float64{"elapsed_ns": float64(elapsed), "ns/op": float64(elapsed) / float64(samples), "process-cpu-ns/op": float64(cpu) / float64(samples), "B/op": float64(after.TotalAlloc-before.TotalAlloc) / float64(samples), "allocs/op": float64(after.Mallocs-before.Mallocs) / float64(samples), "sampled-heap-growth-B": float64(peak.Load() - before.HeapAlloc)}}
					observations := map[string][]time.Duration{"latency": {}}
					for _, s := range results {
						observations["latency"] = append(observations["latency"], s.elapsed)
						if strings.HasPrefix(workload, "durable") {
							observations["publication"] = append(observations["publication"], s.publication)
						} else {
							observations["cancellation"] = append(observations["cancellation"], s.cancellation)
							observations["event-roundtrip"] = append(observations["event-roundtrip"], s.roundTrips...)
						}
					}
					if workload != "durable_unary" {
						run.Events = lifecycleEvents
						if workload == "durable_stream_64" {
							run.Events += 2
						}
					}
					for category, values := range observations {
						for _, p := range []int{50, 95, 99} {
							run.Metrics[fmt.Sprintf("%s-p%d-us", category, p)] = lifecyclePercentile(values, p)
						}
					}
					raw, _ := json.Marshal(run)
					t.Logf("LIFECYCLE_MEASUREMENT %s", raw)
				}
			}
		}
	}
}

func TestLifecyclePerformanceOracleDetectsCorruption(t *testing.T) {
	good := lifecycleResponse("response_local", "route", lifecycleText)
	raw, _ := json.Marshal(good)
	if err := lifecycleCheckResult(raw, "response_local", "route", lifecycleText); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"output", "usage", "error", "model", "status"} {
		t.Run(field, func(t *testing.T) {
			var altered map[string]any
			json.Unmarshal(raw, &altered)
			delete(altered, field)
			bad, _ := json.Marshal(altered)
			if lifecycleCheckResult(bad, "response_local", "route", lifecycleText) == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
}

func TestLifecycleEventOracleDetectsCorruption(t *testing.T) {
	for _, index := range []int{0, 1, lifecycleEvents + 1} {
		kind, event := lifecycleExpectedEvent(index, "response_local", "route")
		raw, _ := json.Marshal(event)
		if err := lifecycleCheckEvent(raw, kind, index, "response_local", "route"); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"type", "sequence_number"} {
			var changed map[string]any
			json.Unmarshal(raw, &changed)
			delete(changed, field)
			bad, _ := json.Marshal(changed)
			if lifecycleCheckEvent(bad, kind, index, "response_local", "route") == nil {
				t.Fatal("deleted event field accepted")
			}
		}
		if lifecycleCheckEvent(raw, kind, index+1, "response_local", "route") == nil {
			t.Fatal("reordered event accepted")
		}
		if lifecycleCheckEvent(raw, "error", index, "response_local", "route") == nil {
			t.Fatal("event-name corruption accepted")
		}
	}
}
