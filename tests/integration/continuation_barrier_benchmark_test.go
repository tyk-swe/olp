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
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/tests/fidelity"
	corpus "github.com/tyk-swe/olp/tests/fixtures/fidelity"
)

// This is an independent reference, NOT the production continuation protocol.
// It combines the existing strict native two-turn wire workflow with the minimal
// encrypted claim, dispatch journal and ready transaction needed by the proposed
// contract. The native client deliberately withholds tool action until durability
// is verified. It does not pretend native tool bytes were withheld by the gateway.
type barrierDocuments struct {
	initial, next, events, assistant, final []byte
	historyBytes                            int
}

func barrierCorpus(t *testing.T, historyBytes int) barrierDocuments {
	t.Helper()
	next, err := corpus.Files.ReadFile("v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	events, err := corpus.Files.ReadFile("v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(next, &fields) != nil {
		t.Fatal("invalid frozen next request")
	}
	var turns []json.RawMessage
	if json.Unmarshal(fields["messages"], &turns) != nil || len(turns) != 3 {
		t.Fatal("invalid frozen turns")
	}
	if historyBytes > 0 {
		var user map[string]json.RawMessage
		json.Unmarshal(turns[0], &user)
		user["content"], _ = json.Marshal("Weather and time in Paris?\n" + strings.Repeat("h", historyBytes))
		turns[0], _ = json.Marshal(user)
	}
	fields["messages"], _ = json.Marshal(turns)
	next, _ = json.Marshal(fields)
	fields["messages"], _ = json.Marshal(turns[:1])
	fields["stream"] = json.RawMessage("true")
	initial, _ := json.Marshal(fields)
	final := []byte(`{"id":"msg-native-tool-final","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"Weather: sunny. Time: 14:00."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":64,"output_tokens":8}}`)
	return barrierDocuments{initial, next, events, turns[1], final, historyBytes}
}
func barrierModel(source []byte, model string) []byte {
	var fields map[string]json.RawMessage
	json.Unmarshal(source, &fields)
	fields["model"], _ = json.Marshal(model)
	body, _ := json.Marshal(fields)
	return body
}

type barrierProvider struct {
	*httptest.Server
	measured    atomic.Bool
	first, next atomic.Int64
	documents   []barrierDocuments
}

func newBarrierProvider(t *testing.T, documents []barrierDocuments) *barrierProvider {
	t.Helper()
	p := &barrierProvider{documents: documents}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != vendorSecret || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
			http.Error(w, "native authentication changed", 401)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			return
		}
		if !p.measured.Load() {
			var fields map[string]json.RawMessage
			json.Unmarshal(body, &fields)
			parityGeneration(w, "anthropic", string(fields["stream"]) == "true")
			return
		}
		for _, d := range p.documents {
			if fidelity.Compare(d.initial, body) == nil {
				p.first.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				for _, frame := range bytes.Split(d.events, []byte("\n\n")) {
					if len(bytes.TrimSpace(frame)) == 0 {
						continue
					}
					if _, err := w.Write(append(append([]byte{}, frame...), '\n', '\n')); err != nil {
						return
					}
					w.(http.Flusher).Flush()
				}
				return
			}
			if fidelity.Compare(d.next, body) == nil {
				p.next.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Write(d.final)
				return
			}
		}
		t.Error("provider request differs from the independent native tool corpus")
		http.Error(w, "native request changed", 400)
	}))
	t.Cleanup(p.Close)
	return p
}
func newBarrierRelay(t *testing.T, p *barrierProvider) *httptest.Server {
	t.Helper()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	client := policy.Client(30 * time.Second)
	t.Cleanup(client.CloseIdleConnections)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "barrier-reference-key" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			http.Error(w, "reference scope", 403)
			return
		}
		if _, err := policy.ValidateEndpoint(p.URL); err != nil {
			http.Error(w, "egress", 403)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.URL+"/v1/messages", http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", vendorSecret)
		req.Header.Set("Anthropic-Version", "2023-06-01")
		response, err := client.Do(req)
		if err != nil {
			http.Error(w, "reference transport", 502)
			return
		}
		defer response.Body.Close()
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		w.WriteHeader(response.StatusCode)
		reader := bufio.NewReader(io.LimitReader(response.Body, 1<<20))
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

type barrierBinding struct {
	installation, key, provider, providerRevision, route, routeRevision, slot, credential string
	ring                                                                                  *secrets.KeyRing
	h                                                                                     *accessHarness
}

func newBarrierBinding(t *testing.T, h *accessHarness, providerID, slug, key string) barrierBinding {
	t.Helper()
	release := h.Runtime.Release()
	provider := release.Snapshot.Providers[providerID]
	route := release.Snapshot.Routes[slug]
	authority, err := h.Runtime.Authenticate(key)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(h.Ring))
	if err != nil {
		t.Fatal(err)
	}
	return barrierBinding{installation, authority.ID, providerID, provider.RevisionID, slug, route.RevisionID, provider.Slots[0].ID, *provider.Slots[0].CredentialID, ring, h}
}
func barrierPayload(d barrierDocuments, ready bool) []byte {
	fields := map[string]json.RawMessage{"schema": json.RawMessage(`"benchmark-native-dependencies/1"`), "request": d.initial}
	if ready {
		fields["assistant"] = d.assistant
	}
	body, _ := json.Marshal(fields)
	return body
}
func (b barrierBinding) claim(ctx context.Context, id string, payload []byte, expires time.Time) (time.Duration, error) {
	start := time.Now()
	tx, err := b.h.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err = b.ring.Store(ctx, tx, b.installation, id, "provider_continuation", payload, &expires); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp_go.provider_resources
  (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,metadata,expires_at,contract_version,submission_id)
  VALUES($1::uuid,'continuation',$2,$3,$4,$5,$6,$7,$8,($1::uuid)::text,'claimed','{}',$9,'benchmark-native-dependencies/1',($1::uuid)::text)`, id, b.key, b.route, b.provider, b.providerRevision, b.routeRevision, b.slot, b.credential, expires)
	if err != nil {
		return 0, err
	}
	err = tx.Commit(ctx)
	return time.Since(start), err
}
func (b barrierBinding) journal(ctx context.Context, id string) (time.Duration, error) {
	start := time.Now()
	tag, err := b.h.Pool.Exec(ctx, `UPDATE olp_go.provider_resources SET state='dispatching' WHERE id=$1 AND api_key_id=$2 AND state='claimed'`, id, b.key)
	if err == nil && tag.RowsAffected() != 1 {
		err = fmt.Errorf("reference dispatch journal did not transition")
	}
	return time.Since(start), err
}
func (b barrierBinding) ready(ctx context.Context, id string, payload []byte, expires time.Time) (time.Duration, error) {
	start := time.Now()
	tx, err := b.h.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err = b.ring.Store(ctx, tx, b.installation, id, "provider_continuation", payload, &expires); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp_go.provider_resources SET state='ready' WHERE id=$1 AND api_key_id=$2 AND state='dispatching'`, id, b.key)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() != 1 {
		return 0, fmt.Errorf("reference ready state did not transition")
	}
	err = tx.Commit(ctx)
	return time.Since(start), err
}
func (b barrierBinding) verify(ctx context.Context, id string, want []byte) error {
	tx, err := b.h.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state, metadata string
	var ciphertext []byte
	if err = tx.QueryRow(ctx, `SELECT r.state,r.metadata::text,s.ciphertext FROM olp_go.provider_resources r JOIN olp_go.secrets s ON s.id=r.id WHERE r.id=$1 AND r.api_key_id=$2 AND r.expires_at>now() AND r.contract_version='benchmark-native-dependencies/1'`, id, b.key).Scan(&state, &metadata, &ciphertext); err != nil {
		return err
	}
	if state != "ready" || bytes.Equal(ciphertext, want) || strings.Contains(metadata, "opaque-fixture-signature") {
		return fmt.Errorf("reference state is not durable encrypted ready state")
	}
	got, err := b.ring.Read(ctx, tx, b.installation, id, "provider_continuation")
	if err != nil {
		return err
	}
	return fidelity.Compare(want, got)
}

type barrierSample struct {
	workflow, firstEvent, wireTool, actionReady, claim, journal, ready time.Duration
	events, actions                                                    int
}

func barrierRequest(ctx context.Context, client *http.Client, endpoint, key string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != 200 {
		response.Body.Close()
		return nil, fmt.Errorf("native workflow status %d", response.StatusCode)
	}
	return response, nil
}
func barrierWorkflow(ctx context.Context, b barrierBinding, p *barrierProvider, client *http.Client, d barrierDocuments, endpoint, key, model, path string) (barrierSample, error) {
	var sample barrierSample
	start := time.Now()
	id := uuid.NewString()
	expires := time.Now().Add(time.Hour)
	var err error
	if path == "reference" {
		if sample.claim, err = b.claim(ctx, id, barrierPayload(d, false), expires); err != nil {
			return sample, err
		}
		if sample.journal, err = b.journal(ctx, id); err != nil {
			return sample, err
		}
	}
	response, err := barrierRequest(ctx, client, endpoint, key, barrierModel(d.initial, model))
	if err != nil {
		return sample, err
	}
	var stream, frame bytes.Buffer
	reader := bufio.NewReader(response.Body)
	for {
		line, e := reader.ReadBytes('\n')
		if len(line) > 0 {
			stream.Write(line)
			frame.Write(line)
			if len(bytes.TrimSpace(line)) == 0 && frame.Len() > 2 {
				if sample.events == 0 {
					sample.firstEvent = time.Since(start)
				}
				if sample.events == 8 {
					sample.wireTool = time.Since(start)
				}
				sample.events++
				frame.Reset()
			}
		}
		if stream.Len() > 64<<10 {
			response.Body.Close()
			return sample, fmt.Errorf("native event corpus exceeded bound")
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
	expectedEvents := bytes.ReplaceAll(d.events, []byte(`"model":"fixture-model"`), []byte(`"model":"`+model+`"`))
	if err = fidelity.CompareEvents(expectedEvents, stream.Bytes()); err != nil {
		return sample, err
	}
	if sample.events != 19 || sample.firstEvent <= 0 || sample.wireTool <= 0 {
		return sample, fmt.Errorf("native event coverage changed")
	}
	if path == "reference" {
		payload := barrierPayload(d, true)
		if sample.ready, err = b.ready(ctx, id, payload, expires); err != nil {
			return sample, err
		}
		if err = b.verify(ctx, id, payload); err != nil {
			return sample, err
		}
	}
	// An action is enabled only after the complete native SDK-equivalent history
	// and, on the reference path, recoverable dependencies have been verified.
	sample.actionReady = time.Since(start)
	sample.actions = 2
	response, err = barrierRequest(ctx, client, endpoint, key, barrierModel(d.next, model))
	if err != nil {
		return sample, err
	}
	final, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	response.Body.Close()
	if err != nil {
		return sample, err
	}
	if err = fidelity.Compare(barrierModel(d.final, model), final); err != nil {
		return sample, err
	}
	sample.workflow = time.Since(start)
	return sample, nil
}

type barrierRun struct {
	Name          string             `json:"name"`
	Repetition    int                `json:"repetition"`
	Samples       int                `json:"samples"`
	Dispatches    int64              `json:"dispatches"`
	FirstRequests int64              `json:"first_requests"`
	NextRequests  int64              `json:"next_requests"`
	Events        int                `json:"events"`
	Actions       int                `json:"actions"`
	HistoryBytes  int                `json:"history_bytes"`
	StateBytes    int                `json:"state_bytes"`
	Metrics       map[string]float64 `json:"metrics"`
}

func TestContinuationBarrierReference(t *testing.T) {
	h := newAccessHarness(t)
	documents := []barrierDocuments{barrierCorpus(t, 0), barrierCorpus(t, 256<<10)}
	provider := newBarrierProvider(t, documents)
	fixture := &strictProviderFixture{Server: provider.Server, profile: "anthropic-messages"}
	owner := h.owner()
	slug, key := publishStrictProvider(t, h, owner, fixture, nil, nil, "strict")
	key = stateKey(t, h, owner, slug, true)
	h.refresh()
	binding := newBarrierBinding(t, h, fixture.providerID, slug, key)
	relay := newBarrierRelay(t, provider)
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
	t.Logf("BARRIER_SETUP %s", setup)
	repetitions, samples := 1, 8
	if os.Getenv("OLP_BARRIER_MEASURE") == "1" {
		repetitions, samples = 3, 24
	}
	for _, d := range documents {
		size := "small"
		if d.historyBytes > 0 {
			size = "large"
		}
		for _, concurrency := range []int{1, 8} {
			for _, path := range []string{"relay", "gateway", "reference"} {
				endpoint, secret, model := h.HTTP.URL+"/anthropic", key, slug
				if path == "relay" {
					endpoint, secret, model = relay.URL, "barrier-reference-key", vendorModel
				}
				for repeat := range repetitions {
					if _, err := barrierWorkflow(t.Context(), binding, provider, client, d, endpoint, secret, model, path); err != nil {
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
					first, next, cpu, start := provider.first.Load(), provider.next.Load(), lifecycleCPU(), time.Now()
					results := make([]barrierSample, samples)
					errs := make(chan error, samples)
					var wg sync.WaitGroup
					for worker := range concurrency {
						wg.Go(func() {
							for i := worker; i < samples; i += concurrency {
								ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
								sample, err := barrierWorkflow(ctx, binding, provider, client, d, endpoint, secret, model, path)
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
					first = provider.first.Load() - first
					next = provider.next.Load() - next
					close(stop)
					<-stopped
					goruntime.ReadMemStats(&after)
					peak.Store(max(peak.Load(), after.HeapAlloc))
					close(errs)
					for err := range errs {
						t.Fatal(err)
					}
					if first != int64(samples) || next != int64(samples) {
						t.Fatalf("native dispatch counts changed: %d/%d, expected %d/%d", first, next, samples, samples)
					}
					run := barrierRun{Name: fmt.Sprintf("%s/c%d/%s", size, concurrency, path), Repetition: repeat, Samples: samples, Dispatches: first + next, FirstRequests: first, NextRequests: next, HistoryBytes: d.historyBytes, StateBytes: len(barrierPayload(d, true)), Metrics: map[string]float64{"elapsed_ns": float64(elapsed), "ns/op": float64(elapsed) / float64(samples), "process-cpu-ns/op": float64(cpu) / float64(samples), "B/op": float64(after.TotalAlloc-before.TotalAlloc) / float64(samples), "allocs/op": float64(after.Mallocs-before.Mallocs) / float64(samples), "sampled-heap-growth-B": float64(peak.Load() - before.HeapAlloc)}}
					values := map[string][]time.Duration{"workflow": {}, "first-event": {}, "wire-tool": {}, "action-ready": {}}
					if path == "reference" {
						values["claim-commit"] = nil
						values["dispatch-journal"] = nil
						values["ready-commit"] = nil
					}
					for _, s := range results {
						run.Events += s.events
						run.Actions += s.actions
						values["workflow"] = append(values["workflow"], s.workflow)
						values["first-event"] = append(values["first-event"], s.firstEvent)
						values["wire-tool"] = append(values["wire-tool"], s.wireTool)
						values["action-ready"] = append(values["action-ready"], s.actionReady)
						if path == "reference" {
							values["claim-commit"] = append(values["claim-commit"], s.claim)
							values["dispatch-journal"] = append(values["dispatch-journal"], s.journal)
							values["ready-commit"] = append(values["ready-commit"], s.ready)
						}
					}
					if run.Events != samples*19 || run.Actions != samples*2 {
						t.Fatal("complete interaction coverage changed")
					}
					for name, observations := range values {
						for _, p := range []int{50, 95, 99} {
							run.Metrics[fmt.Sprintf("%s-p%d-us", name, p)] = lifecyclePercentile(observations, p)
						}
					}
					raw, _ := json.Marshal(run)
					t.Logf("BARRIER_MEASUREMENT %s", raw)
				}
			}
		}
	}
}
func TestContinuationBarrierReferenceCorruption(t *testing.T) {
	d := barrierCorpus(t, 0)
	if err := fidelity.CompareEvents(d.events, d.events); err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{bytes.Replace(d.events, []byte("opaque-fixture-signature-do-not-log"), []byte("lost"), 1), bytes.Replace(d.events, []byte("call-clock"), []byte("call-weather"), 1), bytes.Replace(d.events, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), nil, 1)} {
		if fidelity.CompareEvents(d.events, changed) == nil {
			t.Fatal("changed dependency or terminal accepted")
		}
	}
	initial, ready := barrierPayload(d, false), barrierPayload(d, true)
	if fidelity.Compare(initial, ready) == nil {
		t.Fatal("missing native assistant dependency accepted")
	}
	changedNext := bytes.Replace(d.next, []byte("call-weather"), []byte("call-lost"), 1)
	if fidelity.Compare(d.next, changedNext) == nil {
		t.Fatal("changed next-turn dependency accepted")
	}
}

func TestContinuationBarrierReferenceStorageCorruption(t *testing.T) {
	h := newAccessHarness(t)
	d := barrierCorpus(t, 0)
	provider := newBarrierProvider(t, []barrierDocuments{d})
	fixture := &strictProviderFixture{Server: provider.Server, profile: "anthropic-messages"}
	owner := h.owner()
	slug, _ := publishStrictProvider(t, h, owner, fixture, nil, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	b := newBarrierBinding(t, h, fixture.providerID, slug, key)
	id, expires := uuid.NewString(), time.Now().Add(time.Hour)
	initial, ready := barrierPayload(d, false), barrierPayload(d, true)
	if _, err := b.claim(t.Context(), id, initial, expires); err != nil {
		t.Fatal(err)
	}
	if err := b.verify(t.Context(), id, ready); err == nil {
		t.Fatal("claim without ready dependencies was actionable")
	}
	if _, err := b.journal(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ready(t.Context(), id, ready, expires); err != nil {
		t.Fatal(err)
	}
	if err := b.verify(t.Context(), id, ready); err != nil {
		t.Fatal(err)
	}
	if err := b.verify(t.Context(), id, initial); err == nil {
		t.Fatal("missing assistant dependencies passed the durable oracle")
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.secrets SET ciphertext=set_byte(ciphertext,0,get_byte(ciphertext,0)#1) WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := b.verify(t.Context(), id, ready); err == nil {
		t.Fatal("corrupt ciphertext was actionable")
	}
}
