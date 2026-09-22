package gateway

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/runtime"
)

// The provider documents below are authored independently of production codecs.
// These benchmarks include the HTTP client, local provider, validation, relay or
// gateway, and instrumentation in one process. They are not gateway-only CPU or
// allocation measurements, a WAN simulation, or empirical model quality evidence.
type fidelityWorkload struct {
	name, kind, path, ingress, native, response string
	stream, rejected                            bool
	events, eventTextBytes                      int
	readDelay                                   time.Duration
}

func fidelityWorkloads() []fidelityWorkload {
	chat := `{"model":"team-chat","messages":[{"role":"user","content":"hello"}],"max_tokens":64}`
	native := strings.Replace(chat, "team-chat", "model-a", 1)
	response := `{"id":"chatcmpl-bench","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
	anthropic := `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"max_tokens":64,"stream":false}`
	anthropicResponse := `{"id":"msg-bench","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}`
	stream := strings.TrimSuffix(chat, "}") + `,"stream":true,"stream_options":{"include_usage":true}}`
	// A real deterministic PNG, not a malformed asset that only exercises JSON.
	picture := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	random := rand.New(rand.NewPCG(209, 210))
	for y := range 512 {
		for x := range 512 {
			picture.SetNRGBA(x, y, color.NRGBA{uint8(random.Uint32()), uint8(random.Uint32()), uint8(random.Uint32()), 255})
		}
	}
	var asset bytes.Buffer
	if err := png.Encode(&asset, picture); err != nil {
		panic(err)
	}
	large := `{"model":"team-chat","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(asset.Bytes()) + `"}}]}],"max_tokens":64}`
	return []fidelityWorkload{
		{name: "native_unary", kind: "openai_compatible", path: "/v1/chat/completions", ingress: chat, native: native, response: response},
		{name: "native_stream_256", kind: "openai_compatible", path: "/v1/chat/completions", ingress: stream, native: strings.Replace(stream, "team-chat", "model-a", 1), stream: true, events: 256, eventTextBytes: 128},
		{name: "native_slow_stream_64", kind: "openai_compatible", path: "/v1/chat/completions", ingress: stream, native: strings.Replace(stream, "team-chat", "model-a", 1), stream: true, events: 64, eventTextBytes: 16384, readDelay: 100 * time.Microsecond},
		{name: "native_asset_png", kind: "openai_compatible", path: "/v1/chat/completions", ingress: large, native: strings.Replace(large, "team-chat", "model-a", 1), response: response},
		{name: "translated_unary", kind: "anthropic", path: "/v1/messages", ingress: strings.TrimSuffix(chat, "}") + `,"stream":false}`, native: anthropic, response: anthropicResponse},
		{name: "rejected_extension", kind: "anthropic", path: "/v1/messages", ingress: strings.TrimSuffix(chat, "}") + `,"unsupported_benchmark_extension":{"required":true}}`, rejected: true},
	}
}

type fidelitySink struct{ calls atomic.Int64 }

func (s *fidelitySink) Terminal(Envelope) { s.calls.Add(1) }

// fidelityContract lets later strict implementations reuse this exact harness
// and payload/oracle hash. Unknown fields must fail instead of benchmarking legacy.
// This input is test-only: it cannot change existing routing/policy/timeout fields.
func fidelityContract[T runtime.Route | runtime.Provider](b *testing.B, w fidelityWorkload, target T, setting string, allowed []string) T {
	b.Helper()
	raw := os.Getenv(setting)
	if raw == "" {
		return target
	}
	var contracts map[string]map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &contracts); err != nil || len(contracts) != 3 {
		b.Fatal("contract configuration must contain native, translated and rejected objects")
	}
	for _, name := range []string{"native", "translated", "rejected"} {
		if len(contracts[name]) == 0 {
			b.Fatalf("explicit %s route contract is required", name)
		}
	}
	category := "native"
	if w.kind == "anthropic" {
		category = "translated"
	}
	if w.rejected {
		category = "rejected"
	}
	encoded, _ := json.Marshal(target)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		b.Fatal(err)
	}
	for name, value := range contracts[category] {
		if !slices.Contains(allowed, name) {
			b.Fatalf("%s cannot change fixture %s", setting, name)
		}
		fields[name] = value
	}
	encoded, _ = json.Marshal(fields)
	if err := json.Unmarshal(encoded, &target); err != nil {
		b.Fatal(err)
	}
	encoded, _ = json.Marshal(target)
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &persisted); err != nil {
		b.Fatal(err)
	}
	for name, value := range contracts[category] {
		var want, got any
		if json.Unmarshal(value, &want) != nil || json.Unmarshal(persisted[name], &got) != nil || !reflect.DeepEqual(want, got) {
			b.Fatalf("%s field %s is unavailable or changed; refusing implicit legacy measurement", setting, name)
		}
	}
	return target
}

type fidelityFixture struct {
	client     *http.Client
	url        string
	body       string
	dispatches atomic.Int64
	completed  atomic.Int64
}

func newFidelityFixture(b *testing.B, w fidelityWorkload, relay bool) *fidelityFixture {
	b.Helper()
	f := &fidelityFixture{}
	var expected any
	if !w.rejected {
		if err := json.Unmarshal([]byte(w.native), &expected); err != nil {
			b.Fatal(err)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		f.dispatches.Add(1)
		defer f.completed.Add(1)
		var actual any
		err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&actual)
		auth := r.Header.Get("Authorization") == "Bearer benchmark-provider-key"
		if w.kind == "anthropic" {
			auth = r.Header.Get("X-Api-Key") == "benchmark-provider-key" && r.Header.Get("Anthropic-Version") == "2023-06-01"
		}
		if w.rejected || r.Method != http.MethodPost || r.URL.Path != w.path || !auth || err != nil || !reflect.DeepEqual(expected, actual) {
			b.Errorf("provider request differs from independent fixture: workload=%s path=%s auth=%t decode=%v", w.name, r.URL.Path, auth, err)
			http.Error(rw, "fixture mismatch", http.StatusBadRequest)
			return
		}
		if !w.stream {
			rw.Header().Set("Content-Type", "application/json")
			io.WriteString(rw, w.response)
			return
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		text := strings.Repeat("x", w.eventTextBytes)
		for range w.events {
			if _, err := fmt.Fprintf(rw, "data: {\"id\":\"bench\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"model-a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", text); err != nil {
				return
			}
			rw.(http.Flusher).Flush()
		}
		io.WriteString(rw, "data: {\"id\":\"bench\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"model-a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":256,\"total_tokens\":258}}\n\ndata: [DONE]\n\n")
	}))
	b.Cleanup(upstream.Close)
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	providerID, credentialID, slotID, routeID, targetID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	version := 1
	snapshot := &runtime.Snapshot{
		Generation: runtime.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers: map[string]runtime.Provider{providerID: {
			ID: providerID, Name: "benchmark", Kind: w.kind, Enabled: true, RevisionID: uuid.NewString(), Endpoint: upstream.URL + "/v1", AuthMode: "api_key", ActiveCredential: &credentialID,
			Capabilities: []runtime.Capability{{Model: "model-a", Operation: "generation", Surface: "openai", Mode: "unary"}, {Model: "model-a", Operation: "generation", Surface: "openai", Mode: "streaming"}},
			Slots:        []runtime.Slot{{ID: slotID, Name: "default", Enabled: true, Weight: 1, CredentialID: &credentialID, CredentialVersion: &version}},
		}},
		Routes: map[string]runtime.Route{"team-chat": {
			ID: routeID, Slug: "team-chat", Operations: []string{"generation"}, OverallTimeout: 30000, MaxAttempts: 1, RoutingID: routeID, RevisionID: uuid.NewString(), Revision: 1,
			Targets: []runtime.Target{{ID: targetID, ProviderID: providerID, ProviderModel: "model-a", Weight: 1, Timeout: 20000, RoutingID: targetID}},
		}},
	}
	if !relay {
		snapshot.Routes["team-chat"] = fidelityContract(b, w, snapshot.Routes["team-chat"], "OLP_FIDELITY_BENCH_ROUTE_CONTRACT", []string{"contract", "fidelity", "fidelity_mode", "fidelity_contract", "interaction_contract", "client_contract", "continuation", "qualification"})
		snapshot.Providers[providerID] = fidelityContract(b, w, snapshot.Providers[providerID], "OLP_FIDELITY_BENCH_PROVIDER_CONTRACT", []string{"profile", "profile_id", "profile_revision", "serving_identity", "model_bindings"})
	}
	release, err := runtime.NewRelease(uuid.NewString(), 1, snapshot, map[string][]byte{credentialID: []byte("benchmark-provider-key")})
	if err != nil {
		b.Fatal(err)
	}
	rt := &fakeRuntime{release: release, keys: map[string]access.Authority{"benchmark-client-key": {ID: uuid.NewString(), Policy: access.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{"team-chat"}}}}}
	var handler http.Handler
	if relay {
		// Fixed authorized destination, bounded request/body, independent native
		// input, provider credential injection and the SAME pinned egress policy.
		client := policy.Client(20 * time.Second)
		b.Cleanup(client.CloseIdleConnections)
		handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
				http.NotFound(rw, r)
				return
			}
			secret, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if _, err := rt.Authenticate(secret); !bearer || err != nil {
				http.Error(rw, "unauthorized", http.StatusUnauthorized)
				return
			}
			if _, err := policy.ValidateEndpoint(upstream.URL); err != nil {
				http.Error(rw, "egress refused", http.StatusForbidden)
				return
			}
			req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.URL+w.path, http.MaxBytesReader(rw, r.Body, 8<<20))
			if err != nil {
				b.Error(err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			if w.kind == "anthropic" {
				req.Header.Set("X-Api-Key", "benchmark-provider-key")
				req.Header.Set("Anthropic-Version", "2023-06-01")
			} else {
				req.Header.Set("Authorization", "Bearer benchmark-provider-key")
			}
			resp, err := client.Do(req)
			if err != nil {
				b.Error(err)
				http.Error(rw, "upstream failed", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			rw.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			rw.WriteHeader(resp.StatusCode)
			if w.stream {
				reader := bufio.NewReader(resp.Body)
				for {
					line, err := reader.ReadBytes('\n')
					if len(line) > 0 {
						if _, err := rw.Write(line); err != nil {
							return
						}
						if len(bytes.TrimSpace(line)) == 0 {
							rw.(http.Flusher).Flush()
						}
					}
					if err != nil {
						return
					}
				}
			}
			io.Copy(rw, io.LimitReader(resp.Body, 8<<20))
		})
		f.body = w.native
	} else {
		gw := New(rt, policy, Config{MaxInFlight: 32, MaxBodyBytes: 8 << 20, MaxResponseBytes: 8 << 20, MaxEventBytes: 64 << 10, InlineMedia: protocols.InlineMediaLimits{Items: 4, ItemBytes: 4 << 20, TotalBytes: 4 << 20}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		gw.Sink = &fidelitySink{}
		b.Cleanup(gw.client.CloseIdleConnections)
		mux := http.NewServeMux()
		gw.Register(mux)
		handler = mux
		f.body = w.ingress
	}
	server := httptest.NewServer(handler)
	b.Cleanup(server.Close)
	f.url = server.URL + "/v1/chat/completions"
	f.client = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 32}, Timeout: 30 * time.Second}
	b.Cleanup(f.client.CloseIdleConnections)
	return f
}

type fidelitySample struct {
	elapsed, first, maximumGap time.Duration
	events, textBytes          int
}

func (f *fidelityFixture) request(w fidelityWorkload, relay bool) (fidelitySample, error) {
	var sample fidelitySample
	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, f.url, strings.NewReader(f.body))
	req.Header.Set("Authorization", "Bearer benchmark-client-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return sample, err
	}
	defer resp.Body.Close()
	wantStatus := http.StatusOK
	if w.rejected {
		wantStatus = http.StatusBadRequest
	}
	if resp.StatusCode != wantStatus {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return sample, fmt.Errorf("status=%d want=%d body=%s", resp.StatusCode, wantStatus, body)
	}
	if !w.stream {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return sample, err
		}
		var response map[string]any
		if err := json.Unmarshal(body, &response); err != nil {
			return sample, err
		}
		if w.rejected {
			problem, _ := response["error"].(map[string]any)
			if problem["code"] != "unsupported_parameter" {
				return sample, fmt.Errorf("wrong rejection: %s", body)
			}
		} else if relay && w.kind == "anthropic" {
			if !bytes.Contains(body, []byte(`"text":"hello"`)) || response["stop_reason"] != "end_turn" {
				return sample, fmt.Errorf("incomplete native response: %s", body)
			}
		} else {
			choices, _ := response["choices"].([]any)
			if len(choices) != 1 {
				return sample, fmt.Errorf("wrong choice count: %s", body)
			}
			choice, _ := choices[0].(map[string]any)
			message, _ := choice["message"].(map[string]any)
			if message["content"] != "hello" || choice["finish_reason"] != "stop" {
				return sample, fmt.Errorf("incomplete response: %s", body)
			}
		}
	} else {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 4096), 64<<10)
		var previous time.Time
		done, stopped := false, false
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				done = true
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta        struct{ Content string } `json:"delta"`
					FinishReason *string                  `json:"finish_reason"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				return sample, err
			}
			for _, choice := range chunk.Choices {
				if choice.FinishReason != nil && *choice.FinishReason == "stop" {
					stopped = true
				}
				if choice.Delta.Content == "" {
					continue
				}
				now := time.Now()
				if sample.events == 0 {
					sample.first = now.Sub(start)
				} else {
					sample.maximumGap = max(sample.maximumGap, now.Sub(previous))
				}
				previous = now
				sample.events++
				sample.textBytes += len(choice.Delta.Content)
				if w.readDelay > 0 {
					time.Sleep(w.readDelay)
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return sample, err
		}
		if !done || !stopped || sample.events != w.events || sample.textBytes != w.events*w.eventTextBytes {
			return sample, fmt.Errorf("incomplete stream: done=%t stop=%t events=%d bytes=%d", done, stopped, sample.events, sample.textBytes)
		}
	}
	sample.elapsed = time.Since(start)
	return sample, nil
}

func fidelityCPU() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second + time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond
}

// BenchmarkFidelity freezes representative legacy baselines before OIF replaces
// the dispatch path. Rejection has no relay comparator: no native invocation is
// equivalent to an intentionally unmappable input. Never omit its workload.
func BenchmarkFidelity(b *testing.B) {
	for _, w := range fidelityWorkloads() {
		for _, concurrency := range []int{1, 8} {
			for _, relay := range []bool{true, false} {
				if relay && w.rejected {
					continue
				}
				mode := "gateway"
				if relay {
					mode = "relay"
				}
				b.Run(fmt.Sprintf("%s/c%d/%s", w.name, concurrency, mode), func(b *testing.B) {
					f := newFidelityFixture(b, w, relay)
					// Warm connection pools outside the measured interval.
					if _, err := f.request(w, relay); err != nil {
						b.Fatal(err)
					}
					f.dispatches.Store(0)
					f.completed.Store(0)
					samples := make([]fidelitySample, b.N)
					goruntime.GC()
					var memory goruntime.MemStats
					goruntime.ReadMemStats(&memory)
					initialHeap := memory.HeapAlloc
					var peak atomic.Uint64
					peak.Store(initialHeap)
					stop, stopped := make(chan struct{}), make(chan struct{})
					go func() {
						defer close(stopped)
						ticker := time.NewTicker(5 * time.Millisecond)
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
					var next atomic.Int64
					var workers sync.WaitGroup
					b.ReportAllocs()
					b.SetBytes(int64(len(f.body)))
					b.ResetTimer()
					cpuStart := fidelityCPU()
					for range concurrency {
						workers.Go(func() {
							for {
								i := int(next.Add(1) - 1)
								if i >= b.N {
									return
								}
								var err error
								samples[i], err = f.request(w, relay)
								if err != nil {
									b.Error(err)
									return
								}
							}
						})
					}
					workers.Wait()
					cpu := fidelityCPU() - cpuStart
					b.StopTimer()
					close(stop)
					<-stopped
					goruntime.ReadMemStats(&memory)
					peak.Store(max(peak.Load(), memory.HeapAlloc))
					wantDispatches := int64(b.N)
					if w.rejected {
						wantDispatches = 0
					}
					if got := f.dispatches.Load(); got != wantDispatches || f.completed.Load() != wantDispatches {
						b.Fatalf("provider effects: dispatched=%d completed=%d want=%d", got, f.completed.Load(), wantDispatches)
					}
					b.ReportMetric(float64(wantDispatches), "dispatches")
					b.ReportMetric(float64(b.N)-float64(wantDispatches), "rejected")
					b.ReportMetric(float64(wantDispatches), "succeeded")
					b.ReportMetric(float64(len(f.body)), "request-bytes")
					b.ReportMetric(float64(peak.Load()-initialHeap), "sampled-heap-growth-B")
					b.ReportMetric(float64(cpu.Nanoseconds())/float64(b.N), "process-cpu-ns/op")
					b.ReportMetric(float64(w.events), "content-events/op")
					report := func(name string, pick func(fidelitySample) time.Duration) {
						values := make([]time.Duration, len(samples))
						for i, s := range samples {
							values[i] = pick(s)
						}
						slices.Sort(values)
						for _, p := range []int{50, 95, 99} {
							index := max(0, int(math.Ceil(float64(p)*float64(len(values))/100))-1)
							b.ReportMetric(float64(values[index].Nanoseconds())/1000, fmt.Sprintf("%s-p%d-us", name, p))
						}
					}
					report("latency", func(s fidelitySample) time.Duration { return s.elapsed })
					if w.stream {
						report("first-event", func(s fidelitySample) time.Duration { return s.first })
						report("max-inter-event-gap", func(s fidelitySample) time.Duration { return s.maximumGap })
					}
				})
			}
		}
	}
}
