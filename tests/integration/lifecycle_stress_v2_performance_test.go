//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/providers"
)

// This version preserves the v1 native workload and oracle helpers without
// altering their frozen source hash. Only the publicly configured video
// profile is corrected to a registered, versioned direct OpenAI profile.
func stressV2SeededCertification(t *testing.T, h *accessHarness, providerID string) {
	t.Helper()
	// The existing local video fixture cannot make paid video certification
	// calls. Reuse its exact capability evidence, then bind it to the public
	// registered profile revision, which the v1 fixture did not have.
	certifyVideoSeeded(t, h, providerID)
	var raw, capabilities []byte
	var credentialID, priorValidation string
	if err := h.Pool.QueryRow(t.Context(), "SELECT configuration FROM olp_go.providers WHERE id=$1", providerID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var cfg providers.Configuration
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ProfileID != "openai-responses" || cfg.ProfileRevision != "1" {
		t.Fatal("video fixture has no registered OpenAI Responses revision 1")
	}
	parts := []any{cfg.Kind, cfg.AuthMode, cfg.Endpoint, cfg.CloudRegion, cfg.CloudProject, cfg.Deployment, cfg.APIVersion, cfg.Options.CredentialHeaders, cfg.Options.ParameterDefaults, cfg.Options.Models, cfg.Options.VendorID,
		cfg.ProfileID, cfg.ProfileRevision, cfg.Options.SemanticHeaders, cfg.Options.QuerySettings, cfg.Options.OperationDefaults, cfg.Options.Bindings}
	if cfg.Options.Network != nil {
		parts = append(parts, cfg.Options.Network)
	}
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	transportFP := hex.EncodeToString(digest[:])[:32]
	if err := h.Pool.QueryRow(t.Context(), "SELECT credential_id::text,validated_fingerprint FROM olp_go.provider_slots WHERE provider_id=$1 AND is_default", providerID).Scan(&credentialID, &priorValidation); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT capabilities FROM olp_go.provider_models WHERE provider_id=$1 AND upstream_model=$2", providerID, vendorModel).Scan(&capabilities); err != nil {
		t.Fatal(err)
	}
	var caps []map[string]any
	if err := json.Unmarshal(capabilities, &caps); err != nil {
		t.Fatal(err)
	}
	credentialFP := transportFP + ":" + credentialID
	for _, cap := range caps {
		cap["credential_fingerprint"] = credentialFP
	}
	capabilities, _ = json.Marshal(caps)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.provider_models SET capabilities=$1 WHERE provider_id=$2 AND upstream_model=$3", capabilities, providerID, vendorModel); err != nil {
		t.Fatal(err)
	}
	tail := strings.LastIndex(priorValidation, ":")
	if tail < 0 {
		t.Fatal("fixture capability inventory has no validation digest")
	}
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.provider_slots SET validated_fingerprint=$1 WHERE provider_id=$2 AND is_default", credentialFP+priorValidation[tail:], providerID); err != nil {
		t.Fatal(err)
	}
}

func stressV2ProvisionVideo(t *testing.T, h *accessHarness, owner *browser, endpoint string) string {
	t.Helper()
	path := "/api/v3/providers"
	provider := h.want(owner, http.MethodPost, path, map[string]any{
		"name": "Lifecycle stress video reference", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai", "auth_mode": "api_key", "endpoint": endpoint, "profile_id": "openai-responses", "profile_revision": "1"},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	path += "/" + provider["id"].(string)
	if probe := h.want(owner, http.MethodPost, path+"/probe", nil, etagHeader(provider), http.StatusOK); probe["succeeded"] != true {
		t.Fatalf("video probe failed: %v", probe)
	}
	stressV2SeededCertification(t, h, provider["id"].(string))
	provider = h.want(owner, http.MethodGet, path, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, path+"/activate", nil, withMatch(provider, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	slug := "stress-video-" + uuid.NewString()[:8]
	draft := map[string]any{
		"slug": slug, "overall_timeout_ms": 30000, "max_attempts": 1,
		"operations": []string{"video_create", "video_get", "video_content", "video_delete"},
		"targets":    []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 20000}},
	}
	raw := os.Getenv("OLP_LIFECYCLE_ROUTE_FIDELITY")
	if raw == "" {
		raw = `{"mode":"legacy"}`
	}
	var fidelity map[string]any
	if json.Unmarshal([]byte(raw), &fidelity) != nil || len(fidelity) != 1 || (fidelity["mode"] != "legacy" && fidelity["mode"] != "strict") {
		t.Fatal("video benchmark route fidelity must contain exactly legacy or strict mode")
	}
	draft["fidelity"] = fidelity
	created := h.want(owner, http.MethodPost, "/api/v3/route-drafts", draft, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v3/route-drafts/"+created["id"].(string)+"/activate", nil, withMatch(created, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	return slug
}

func stressV2AssertProfiles(t *testing.T, video, duplex *accessHarness) {
	t.Helper()
	for _, check := range []struct {
		name, id string
		h        *accessHarness
	}{
		{name: "video", id: "openai-responses", h: video},
		{name: "realtime", id: "azure-v1-responses", h: duplex},
	} {
		var raw []byte
		if err := check.h.Pool.QueryRow(t.Context(), "SELECT configuration FROM olp_go.providers").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var config struct {
			ProfileID       string `json:"profile_id"`
			ProfileRevision string `json:"profile_revision"`
		}
		if err := json.Unmarshal(raw, &config); err != nil || config.ProfileID != check.id || config.ProfileRevision != "1" {
			t.Fatalf("%s serving profile changed: %s", check.name, raw)
		}
	}
	t.Logf("LIFECYCLE_STRESS_V2_PROFILES %s", `{"video":{"id":"openai-responses","revision":"1"},"realtime":{"id":"azure-v1-responses","revision":"1"}}`)
}

func TestLifecycleStressV2Performance(t *testing.T) {
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
	t.Logf("LIFECYCLE_STRESS_V2_SETUP %s", setup)
	image := stressImage()
	video := newStressVideoFixture(t, image)
	owner := h.owner()
	videoSlug := stressV2ProvisionVideo(t, h, owner, video.URL+"/v1")
	videoKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "stress media key", "scopes": []string{"inference"}, "allowed_routes": []string{videoSlug}}, map[string]string{"Idempotency-Key": "stress-media-key"}, 201)["secret"].(string)
	videoKey2 := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "stress media key 2", "scopes": []string{"inference"}, "allowed_routes": []string{videoSlug}}, map[string]string{"Idempotency-Key": "stress-media-key-2"}, 201)["secret"].(string)
	h.refresh()
	videoRelay := stressRelay(t, video)
	duplexHarness := newAccessHarness(t)
	duplex := newLifecycleFixture(t)
	duplexSlug, duplexKey := lifecycleProvision(t, duplexHarness, duplex.URL)
	stressV2AssertProfiles(t, h, duplexHarness)
	duplex.measuring.Store(true)
	duplexRelay := lifecycleRelay(t, duplex)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 32}, Timeout: 45 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	mediaBodies := map[string][]byte{}
	mediaTypes := map[string]string{}
	mediaBodies["gateway"], mediaTypes["gateway"] = stressMultipart(t, videoSlug, image)
	mediaBodies["relay"], mediaTypes["relay"] = stressMultipart(t, vendorModel, image)
	repetitions, samples := 1, 2
	if testing.Short() {
		samples = 1
	}
	if os.Getenv("OLP_LIFECYCLE_STRESS_V2_MEASURE") == "1" {
		repetitions, samples = 3, 8
	}
	for _, workload := range []string{"media_cycle_4m", "media_slow_content_1m", "duplex_jitter_64"} {
		concurrencies := []int{1, 2}
		if workload == "duplex_jitter_64" {
			concurrencies = []int{1, 4}
		}
		for _, concurrency := range concurrencies {
			for _, path := range []string{"relay", "gateway"} {
				mediaEndpoint, mediaModel := h.HTTP.URL, videoSlug
				duplexEndpoint, duplexSecret, duplexModel := duplexHarness.HTTP.URL, duplexKey, duplexSlug
				if path == "relay" {
					mediaEndpoint, mediaModel = videoRelay.URL, vendorModel
					duplexEndpoint, duplexSecret, duplexModel = duplexRelay.URL, "lifecycle-reference-key", vendorModel
				}
				mediaSecret := func(worker int) string {
					if path == "relay" {
						return fmt.Sprintf("lifecycle-stress-reference-key-%d", worker)
					}
					if worker == 0 {
						return videoKey
					}
					return videoKey2
				}
				for repetition := range repetitions {
					seedIDs := make([]string, concurrency)
					if workload == "media_slow_content_1m" {
						for worker := range concurrency {
							_, id, err := stressVideoCycle(t.Context(), client, mediaEndpoint, mediaSecret(worker), mediaModel, mediaBodies[path], mediaTypes[path], video)
							if err != nil {
								t.Fatal(err)
							}
							seedIDs[worker] = id
						}
					}
					measureOne := func(ctx context.Context, worker int) (stressSample, error) {
						switch workload {
						case "media_cycle_4m":
							sample, _, err := stressVideoCycle(ctx, client, mediaEndpoint, mediaSecret(worker), mediaModel, mediaBodies[path], mediaTypes[path], video)
							return sample, err
						case "media_slow_content_1m":
							return stressContent(ctx, client, mediaEndpoint, mediaSecret(worker), seedIDs[worker], video, true)
						default:
							return stressDuplex(ctx, duplexHarness, duplex, client, duplexEndpoint, duplexSecret, duplexModel, path == "relay")
						}
					}
					if _, err := measureOne(t.Context(), 0); err != nil {
						t.Fatalf("%s warmup: %v", workload, err)
					}
					runtime.GC()
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					var peakHeap, peakSpool atomic.Uint64
					peakHeap.Store(before.HeapAlloc)
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
								var m runtime.MemStats
								runtime.ReadMemStats(&m)
								peakHeap.Store(max(peakHeap.Load(), m.HeapAlloc))
								peakSpool.Store(max(peakSpool.Load(), uint64(h.Media.Transport.Spool.UsedBytes())))
							}
						}
					}()
					beforeDispatches := video.posts.Load() + video.gets.Load() + video.contents.Load() + duplex.dispatches.Load()
					cpu, started := stressCPU(), time.Now()
					results := make([]stressSample, samples)
					errs := make(chan error, samples)
					var workers sync.WaitGroup
					for worker := range concurrency {
						workers.Go(func() {
							for i := worker; i < samples; i += concurrency {
								ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
								sample, err := measureOne(ctx, worker)
								cancel()
								if err != nil {
									errs <- err
								}
								results[i] = sample
							}
						})
					}
					workers.Wait()
					elapsed := time.Since(started)
					cpu = stressCPU() - cpu
					dispatches := video.posts.Load() + video.gets.Load() + video.contents.Load() + duplex.dispatches.Load() - beforeDispatches
					close(stop)
					<-stopped
					runtime.ReadMemStats(&after)
					peakHeap.Store(max(peakHeap.Load(), after.HeapAlloc))
					close(errs)
					for err := range errs {
						t.Fatal(err)
					}
					observations := map[string][]time.Duration{"latency": {}, "first-byte": {}, "inter-read-gap": {}, "event-rtt": {}, "event-jitter": {}, "cancellation": {}}
					run := stressRun{Name: fmt.Sprintf("%s/c%d/%s", workload, concurrency, path), Repetition: repetition, Samples: samples, Succeeded: samples, Dispatches: int(dispatches), Metrics: map[string]float64{
						"elapsed_ns": float64(elapsed), "ns/op": float64(elapsed) / float64(samples), "process-cpu-ns/op": float64(cpu) / float64(samples),
						"B/op": float64(after.TotalAlloc-before.TotalAlloc) / float64(samples), "allocs/op": float64(after.Mallocs-before.Mallocs) / float64(samples),
						"sampled-heap-growth-B": float64(peakHeap.Load() - before.HeapAlloc), "sampled-spool-peak-B": float64(peakSpool.Load()),
						"spool-final-B": float64(h.Media.Transport.Spool.UsedBytes()),
					}}
					for _, result := range results {
						observations["latency"] = append(observations["latency"], result.elapsed)
						observations["first-byte"] = append(observations["first-byte"], result.firstByte)
						observations["inter-read-gap"] = append(observations["inter-read-gap"], result.gaps...)
						observations["event-rtt"] = append(observations["event-rtt"], result.eventRTT...)
						observations["event-jitter"] = append(observations["event-jitter"], result.eventJitter...)
						observations["cancellation"] = append(observations["cancellation"], result.cancellation)
						run.Accepted += result.accepted
						run.Partial += result.partial
						run.Retrieved += result.retrieved
						run.Content += result.content
						run.Events += result.events
						run.RTTObservations += len(result.eventRTT)
						run.JitterSamples += len(result.eventJitter)
						run.UploadedBytes += result.uploaded
						run.DownloadedBytes += result.downloaded
					}
					categories := []string{"latency", "first-byte", "inter-read-gap"}
					if workload == "duplex_jitter_64" {
						categories = []string{"latency", "event-rtt", "event-jitter", "cancellation"}
					}
					for _, category := range categories {
						for _, percentile := range []int{50, 95, 99} {
							run.Metrics[fmt.Sprintf("%s-p%d-us", category, percentile)] = stressPercentile(observations[category], percentile)
						}
					}
					if run.Dispatches != map[string]int{"media_cycle_4m": 4 * samples, "media_slow_content_1m": samples, "duplex_jitter_64": samples}[workload] || run.Metrics["spool-final-B"] != 0 {
						t.Fatalf("work conservation/spool changed: %+v", run)
					}
					raw, _ := json.Marshal(run)
					t.Logf("LIFECYCLE_STRESS_V2_MEASUREMENT %s", raw)
				}
			}
		}
	}
	stressNegativeControls(t, h, video, videoSlug, videoKey, image)
}
