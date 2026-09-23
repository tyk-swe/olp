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
	"os"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// V2 is a separate measurement fixture. The frozen v1 provider-ID oracle and
// its strict failure remain unchanged. A strict local ID is qualified only
// after checking its independent owner, upstream mapping and public retrieval.
type lifecycleV2Fixture struct {
	*lifecycleFixture
	responses sync.Map // upstream ID -> exact terminal native response
	gets      atomic.Int64
}

func newLifecycleV2Fixture(t *testing.T) *lifecycleV2Fixture {
	t.Helper()
	f := &lifecycleV2Fixture{lifecycleFixture: &lifecycleFixture{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != vendorSecret {
			http.Error(w, "provider credential", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/openai/v1/models" {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/openai/v1/responses/") {
			id := strings.TrimPrefix(r.URL.Path, "/openai/v1/responses/")
			value, ok := f.responses.Load(id)
			if !ok {
				http.NotFound(w, r)
				return
			}
			f.gets.Add(1)
			writeJSON(w, value)
			return
		}
		if r.URL.Path == "/openai/v1/realtime" {
			if r.URL.Query().Get("model") != vendorModel {
				http.Error(w, "model", http.StatusBadRequest)
				return
			}
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			c.SetReadLimit(64 << 10)
			f.dispatches.Add(1)
			id := uuid.NewString()
			done := make(chan struct{})
			f.closed.Store(id, done)
			defer close(done)
			if err := c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created","session":{"id":"`+id+`","model":"`+vendorModel+`"}}`)); err != nil {
				return
			}
			for i := range lifecycleEvents {
				kind, data, err := c.Read(r.Context())
				if err != nil {
					return
				}
				if kind != websocket.MessageText || !bytes.Equal(data, lifecycleWire(i, false)) {
					t.Error("native realtime input changed")
					return
				}
				if err := c.Write(r.Context(), websocket.MessageText, lifecycleWire(i, true)); err != nil {
					return
				}
			}
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
			http.Error(w, "json", http.StatusBadRequest)
			return
		}
		if !f.measuring.Load() {
			writeResponsesFixture(w, vendorModel, vendorAnswer, req["stream"] == true)
			return
		}
		stream := req["stream"] == true
		want := map[string]any{"model": vendorModel, "input": lifecycleInput(), "store": true, "stream": stream}
		if !reflect.DeepEqual(req, want) {
			t.Errorf("native durable request changed: %s", raw)
			http.Error(w, "request changed", http.StatusBadRequest)
			return
		}
		n := f.dispatches.Add(1)
		id := fmt.Sprintf("resp_fidelity_%09d", n)
		f.sends.Store(id, time.Now())
		text := lifecycleText
		if stream {
			text = strings.Repeat(lifecycleText, lifecycleEvents)
		}
		result := lifecycleResponse(id, vendorModel, text)
		f.responses.Store(id, result)
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
		emit("response.completed", map[string]any{"type": "response.completed", "sequence_number": lifecycleEvents + 1, "response": result})
	}))
	t.Cleanup(f.Close)
	return f
}

type lifecycleV2Identity struct {
	id, upstream string
}
type lifecycleV2Sample struct {
	lifecycleSample
	identity lifecycleV2Identity
}

func lifecycleV2CheckNativeResponse(raw []byte, upstream string, stream bool) error {
	text := lifecycleText
	if stream {
		text = strings.Repeat(lifecycleText, lifecycleEvents)
	}
	return lifecycleCheckResult(raw, upstream, vendorModel, text)
}

// The public key ID comes from the same management response as its secret.
// The second valid owner key exercises an authenticated zero-dispatch denial.
func lifecycleV2Provision(t *testing.T, h *accessHarness, endpoint string) (slug, secret, keyID, otherSecret string) {
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
	slug = "fidelity-lifecycle"
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
	key := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "state key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	other := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "other state key", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	h.refresh()
	return slug, key["secret"].(string), key["id"].(string), other["secret"].(string)
}

func lifecycleV2IDShape(id string, strict bool) (string, error) {
	kind := "response"
	if strict {
		kind = "strict_response"
	}
	prefix := kind + "_"
	if !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+32 {
		return "", fmt.Errorf("unmapped durable id")
	}
	suffix := strings.TrimPrefix(id, prefix)
	if _, err := uuid.Parse(suffix); err != nil || strings.ToLower(suffix) != suffix {
		return "", fmt.Errorf("invalid local durable UUID")
	}
	return suffix, nil
}

func lifecycleV2MappedID(ctx context.Context, h *accessHarness, f *lifecycleV2Fixture, id, slug, owner string, strict, stream bool) (string, time.Time, error) {
	suffix, err := lifecycleV2IDShape(id, strict)
	if err != nil {
		return "", time.Time{}, err
	}
	kind := "response"
	version := ""
	if strict {
		kind, version = "strict_response", "native-responses-v1"
	}
	var actualKind, actualOwner, actualSlug, upstream, state string
	var contract *string
	var encrypted bool
	if strict {
		err = h.Pool.QueryRow(ctx, `SELECT r.kind,r.api_key_id::text,r.route_slug,r.upstream_id,r.state,r.contract_version,
	  EXISTS(SELECT 1 FROM olp_go.secrets s WHERE s.id=r.id AND s.purpose='provider_continuation')
	  FROM olp_go.provider_resources r WHERE replace(r.id::text,'-','')=$1`, suffix).Scan(&actualKind, &actualOwner, &actualSlug, &upstream, &state, &contract, &encrypted)
	} else {
		// The pre-#216 product has no contract_version column. Its original
		// legacy durable owner/mapping table is still checked independently.
		err = h.Pool.QueryRow(ctx, `SELECT r.kind,r.api_key_id::text,r.route_slug,r.upstream_id,r.state
	  FROM olp_go.provider_resources r WHERE replace(r.id::text,'-','')=$1`, suffix).Scan(&actualKind, &actualOwner, &actualSlug, &upstream, &state)
	}
	expectedState := "completed"
	if stream {
		expectedState = "in_progress"
	}
	if err != nil || actualKind != kind || actualOwner != owner || actualSlug != slug || state != expectedState || upstream == id {
		return "", time.Time{}, fmt.Errorf("observed ID has no matching durable owner/native mapping: %v", err)
	}
	if strict && (contract == nil || *contract != version || !encrypted) || !strict && (contract != nil || encrypted) {
		return "", time.Time{}, fmt.Errorf("durable contract kind or ciphertext changed")
	}
	native, ok := f.responses.Load(upstream)
	if !ok {
		return "", time.Time{}, fmt.Errorf("mapped upstream response was never retained by fixture")
	}
	nativeRaw, err := json.Marshal(native)
	if err != nil || lifecycleV2CheckNativeResponse(nativeRaw, upstream, stream) != nil {
		return "", time.Time{}, fmt.Errorf("mapped upstream response does not match exact native provider document")
	}
	v, ok := f.sends.LoadAndDelete(upstream)
	if !ok {
		return "", time.Time{}, fmt.Errorf("missing independently emitted native ID")
	}
	return upstream, v.(time.Time), nil
}

func lifecycleV2Request(ctx context.Context, h *accessHarness, f *lifecycleV2Fixture, client *http.Client, endpoint, key, model, workload, path, owner string, strict bool) (lifecycleV2Sample, error) {
	var out lifecycleV2Sample
	if path == "relay" || strings.HasPrefix(workload, "duplex") {
		v, err := lifecycleRequest(ctx, h, f.lifecycleFixture, client, endpoint, key, model, workload, path == "relay")
		out.lifecycleSample = v
		return out, err
	}
	start := time.Now()
	stream := workload == "durable_stream_64"
	input, _ := json.Marshal(lifecycleInput())
	body := fmt.Sprintf(`{"model":%q,"input":%s,"store":true,"stream":%t}`, model, input, stream)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/responses", strings.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return out, fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	observe := func(id string, at time.Time) error {
		upstream, emitted, err := lifecycleV2MappedID(ctx, h, f, id, model, owner, strict, stream)
		if err != nil {
			return err
		}
		out.identity = lifecycleV2Identity{id: id, upstream: upstream}
		// The timestamp is provider emission to client observation, before SQL
		// validation, just as in v1; the SQL check still affects request latency.
		out.publication = at.Sub(emitted)
		if out.publication < 0 {
			return fmt.Errorf("provider emission occurred after client observation")
		}
		return nil
	}
	if !stream {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		out.elapsed = time.Since(start)
		if err != nil {
			return out, err
		}
		var result struct{ ID string }
		if json.Unmarshal(raw, &result) != nil {
			return out, fmt.Errorf("invalid result")
		}
		if err := observe(result.ID, start.Add(out.elapsed)); err != nil {
			return out, err
		}
		return out, lifecycleCheckResult(raw, out.identity.id, model, lifecycleText)
	}
	reader := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	reader.Buffer(make([]byte, 4096), 64<<10)
	index, eventName, haveData := 0, "", false
	for reader.Scan() {
		line := reader.Text()
		if line == "" {
			if !haveData {
				return out, fmt.Errorf("empty or partial SSE frame")
			}
			eventName, haveData = "", false
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			if eventName != "" || haveData {
				return out, fmt.Errorf("duplicate SSE event")
			}
			eventName = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") || eventName == "" || haveData {
			return out, fmt.Errorf("unexpected SSE framing")
		}
		raw := []byte(strings.TrimPrefix(line, "data: "))
		if index == 0 {
			var created struct{ Response struct{ ID string } }
			if json.Unmarshal(raw, &created) != nil {
				return out, fmt.Errorf("invalid created event")
			}
			if err := observe(created.Response.ID, time.Now()); err != nil {
				return out, err
			}
		}
		if err := lifecycleCheckEvent(raw, eventName, index, out.identity.id, model); err != nil {
			return out, err
		}
		index++
		haveData = true
	}
	out.elapsed = time.Since(start)
	if reader.Err() != nil {
		return out, reader.Err()
	}
	if haveData || eventName != "" || index != lifecycleEvents+2 {
		return out, fmt.Errorf("incomplete stream: %d", index)
	}
	return out, nil
}

type lifecycleV2Run struct {
	lifecycleRun
	Mappings          int `json:"mapping_checks"`
	Retrievals        int `json:"retrieval_checks"`
	NegativeControls  int `json:"negative_controls"`
	NegativeDispatch  int `json:"negative_dispatches"`
	AmbiguousOutcomes int `json:"ambiguous_outcomes"`
}

func lifecycleV2Get(ctx context.Context, client *http.Client, endpoint, secret, id, model, text string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/responses/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("public retrieve: status %d: %s: %v", resp.StatusCode, raw, err)
	}
	return lifecycleCheckResult(raw, id, model, text)
}

func lifecycleV2WrongOwner(ctx context.Context, client *http.Client, endpoint, other, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/responses/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+other)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("wrong owner observed resource: %d", resp.StatusCode)
	}
	return nil
}

func TestFidelityLifecycleV2Performance(t *testing.T) {
	if os.Getenv("OLP_LIFECYCLE_V2_MEASURE") != "1" {
		t.Fatal("explicit lifecycle-v2 measurement selection required")
	}
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
	t.Logf("LIFECYCLE_V2_SETUP %s", setup)
	f := newLifecycleV2Fixture(t)
	slug, key, keyID, other := lifecycleV2Provision(t, h, f.URL)
	f.measuring.Store(true)
	relay := lifecycleRelay(t, f.lifecycleFixture)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	const repetitions, samples = 3, 24
	strict := os.Getenv("OLP_LIFECYCLE_ROUTE_FIDELITY") == `{"mode":"strict"}`
	for _, workload := range []string{"durable_unary", "durable_stream_64", "duplex_64", "duplex_slow_64"} {
		for _, concurrency := range []int{1, 4} {
			for _, path := range []string{"relay", "gateway"} {
				endpoint, secret, model := h.HTTP.URL, key, slug
				if path == "relay" {
					endpoint, secret, model = relay.URL, "lifecycle-reference-key", vendorModel
				}
				for repeat := range repetitions {
					if _, err := lifecycleV2Request(t.Context(), h, f, client, endpoint, secret, model, workload, path, keyID, strict); err != nil {
						t.Fatal(err)
					}
					goruntime.GC()
					var before, after goruntime.MemStats
					goruntime.ReadMemStats(&before)
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
								var m goruntime.MemStats
								goruntime.ReadMemStats(&m)
								peak.Store(max(peak.Load(), m.HeapAlloc))
							}
						}
					}()
					dispatches, cpu, start := f.dispatches.Load(), lifecycleCPU(), time.Now()
					results := make([]lifecycleV2Sample, samples)
					errs := make(chan error, samples)
					var wg sync.WaitGroup
					for worker := range concurrency {
						wg.Go(func() {
							for i := worker; i < samples; i += concurrency {
								ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
								sample, err := lifecycleV2Request(ctx, h, f, client, endpoint, secret, model, workload, path, keyID, strict)
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
					dispatches = f.dispatches.Load() - dispatches
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
					run := lifecycleV2Run{lifecycleRun: lifecycleRun{Name: fmt.Sprintf("%s/c%d/%s", workload, concurrency, path), Repetition: repeat, Samples: samples, Succeeded: samples, Dispatches: dispatches, Metrics: map[string]float64{"elapsed_ns": float64(elapsed), "ns/op": float64(elapsed) / float64(samples), "process-cpu-ns/op": float64(cpu) / float64(samples), "B/op": float64(after.TotalAlloc-before.TotalAlloc) / float64(samples), "allocs/op": float64(after.Mallocs-before.Mallocs) / float64(samples), "sampled-heap-growth-B": float64(peak.Load() - before.HeapAlloc)}}}
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
					// Retrieval and cross-owner denial are semantic controls after all
					// resource timings and allocation/heap samples for this repetition.
					if path == "gateway" && strings.HasPrefix(workload, "durable") {
						getsBefore := f.gets.Load()
						text := lifecycleText
						retrievedModel := vendorModel // historical B GET exposes native model
						if strict {
							retrievedModel = model // strict C projects its route identity
						}
						if workload == "durable_stream_64" {
							text = strings.Repeat(lifecycleText, lifecycleEvents)
						}
						for _, s := range results {
							if s.identity.id == "" || s.identity.upstream == "" {
								t.Fatal("missing independent local/native identity check")
							}
							run.Mappings++
							if err := lifecycleV2Get(t.Context(), client, endpoint, key, s.identity.id, retrievedModel, text); err != nil {
								t.Fatal(err)
							}
							run.Retrievals++
						}
						if f.gets.Load()-getsBefore != int64(samples) {
							t.Fatal("provider retrieval count changed")
						}
						beforeDispatch, beforeGets := f.dispatches.Load(), f.gets.Load()
						if err := lifecycleV2WrongOwner(t.Context(), client, endpoint, other, results[0].identity.id); err != nil {
							t.Fatal(err)
						}
						run.NegativeControls++
						if f.dispatches.Load() != beforeDispatch || f.gets.Load() != beforeGets {
							run.NegativeDispatch++
							t.Fatal("cross-owner negative reached provider")
						}
					}
					if run.Mappings != run.Retrievals || run.AmbiguousOutcomes != 0 {
						t.Fatal("mapping, retrieval or outcome accounting changed")
					}
					encoded, _ := json.Marshal(run)
					t.Logf("LIFECYCLE_V2_MEASUREMENT %s", encoded)
				}
			}
		}
	}
}

func TestFidelityLifecycleV2Semantic(t *testing.T) {
	if os.Getenv("OLP_LIFECYCLE_V2_MEASURE") == "1" {
		t.Fatal("semantic smoke cannot be selected as timing evidence")
	}
	h := newAccessHarness(t)
	f := newLifecycleV2Fixture(t)
	slug, key, keyID, other := lifecycleV2Provision(t, h, f.URL)
	f.measuring.Store(true)
	relay := lifecycleRelay(t, f.lifecycleFixture)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}, Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	strict := os.Getenv("OLP_LIFECYCLE_ROUTE_FIDELITY") == `{"mode":"strict"}`
	for _, workload := range []string{"durable_unary", "durable_stream_64", "duplex_64", "duplex_slow_64"} {
		for _, path := range []string{"relay", "gateway"} {
			endpoint, secret, model := h.HTTP.URL, key, slug
			if path == "relay" {
				endpoint, secret, model = relay.URL, "lifecycle-reference-key", vendorModel
			}
			before := f.dispatches.Load()
			sample, err := lifecycleV2Request(t.Context(), h, f, client, endpoint, secret, model, workload, path, keyID, strict)
			if err != nil || f.dispatches.Load() != before+1 {
				t.Fatalf("%s/%s changed: %v", workload, path, err)
			}
			if path != "gateway" || !strings.HasPrefix(workload, "durable") {
				continue
			}
			text := lifecycleText
			retrievedModel := vendorModel
			if strict {
				retrievedModel = model
			}
			if workload == "durable_stream_64" {
				text = strings.Repeat(lifecycleText, lifecycleEvents)
			}
			if err := lifecycleV2Get(t.Context(), client, endpoint, key, sample.identity.id, retrievedModel, text); err != nil {
				t.Fatal(err)
			}
			before = f.dispatches.Load()
			gets := f.gets.Load()
			if err := lifecycleV2WrongOwner(t.Context(), client, endpoint, other, sample.identity.id); err != nil || f.dispatches.Load() != before || f.gets.Load() != gets {
				t.Fatalf("cross-owner zero-dispatch control changed: %v", err)
			}
		}
	}
}

func TestLifecycleV2IdentityOracleDetectsCorruption(t *testing.T) {
	// Public service tests exercise real writes. This isolated test guards the
	// expected strict/legacy ID grammar before any database lookup is attempted.
	for _, tc := range []struct {
		id     string
		strict bool
		ok     bool
	}{
		{"response_" + strings.Repeat("a", 32), false, true},
		{"strict_response_" + strings.Repeat("a", 32), true, true},
		{"response_" + strings.Repeat("a", 32), true, false},
		{"strict_response_" + strings.Repeat("a", 32), false, false},
		{"resp_fidelity_000000001", true, false},
		{"strict_response_" + strings.Repeat("a", 31), true, false},
		{"strict_response_" + strings.Repeat("z", 32), true, false},
	} {
		_, err := lifecycleV2IDShape(tc.id, tc.strict)
		got := err == nil
		if got != tc.ok {
			t.Fatalf("ID grammar accepted %q under strict=%t", tc.id, tc.strict)
		}
	}
}

// Ensure the v2 fixture's byte oracle uses the same exact response and event
// functions as v1; no ID-mode exception can suppress field or order checks.
func TestLifecycleV2PayloadOracleDetectsCorruption(t *testing.T) {
	id := "strict_response_" + strings.Repeat("a", 32)
	good, _ := json.Marshal(lifecycleResponse(id, "route", lifecycleText))
	if err := lifecycleCheckResult(good, id, "route", lifecycleText); err != nil {
		t.Fatal(err)
	}
	var wrong map[string]any
	json.Unmarshal(good, &wrong)
	wrong["usage"] = nil
	bad, _ := json.Marshal(wrong)
	if lifecycleCheckResult(bad, id, "route", lifecycleText) == nil {
		t.Fatal("usage corruption accepted")
	}
	for _, index := range []int{0, 1, lifecycleEvents + 1} {
		kind, event := lifecycleExpectedEvent(index, id, "route")
		encoded, _ := json.Marshal(event)
		if lifecycleCheckEvent(encoded, kind, index, id, "route") != nil || lifecycleCheckEvent(encoded, kind, index+1, id, "route") == nil {
			t.Fatal("event order oracle changed")
		}
	}
	native := lifecycleResponse("resp_fidelity_000000001", vendorModel, lifecycleText)
	for _, field := range []string{"model", "id", "output"} {
		changed := structuredLifecycleCopy(native)
		if field == "model" {
			changed[field] = "wrong-upstream-model"
		} else {
			delete(changed, field)
		}
		bad, _ := json.Marshal(changed)
		if lifecycleV2CheckNativeResponse(bad, "resp_fidelity_000000001", false) == nil {
			t.Fatalf("native %s corruption accepted", field)
		}
	}
}

func structuredLifecycleCopy(value map[string]any) map[string]any {
	raw, _ := json.Marshal(value)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
