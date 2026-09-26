//go:build integration

package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/runtime"
)

// mediaTestPolicy mirrors the loopback egress policy the harnesses use.
func mediaTestPolicy() *egress.Policy {
	return &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
}

// secondMediaGateway builds an independently constructed gateway sharing the
// harness runtime, upstream, and limiter — the shape two replicas take.
func secondMediaGateway(t *testing.T, h *harness, limiter *limits.Limiter) (*Server, *httptest.Server, *capture) {
	t.Helper()
	gw := New(h.rt, mediaTestPolicy(), Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096}, h.gateway.log)
	spool, err := media.NewSpool(t.TempDir(), media.MinCapacityBytes, h.gateway.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	gw.Media = &MediaDeps{
		Jobs: &media.Service{Log: h.gateway.log, Transport: &media.Transport{
			Client: gw.client, Auth: gw.auth, Egress: gw.egress,
			Spool: spool, MaxResponseBytes: 1 << 20,
		}},
		Admission: media.NewAdmissionState(media.MinCapacityBytes),
	}
	gw.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, h.gateway.log)
	sink := &capture{}
	gw.Sink = sink
	mux := http.NewServeMux()
	gw.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return gw, server, sink
}

func postMedia(t *testing.T, url string, body []byte, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	data, _ := io.ReadAll(resp.Body)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("invalid JSON response %q: %v", data, err)
		}
	}
	return resp, decoded
}

// TestMediaCooldownSharedAcrossGateways proves a cooldown one replica records
// is enforced by an independently constructed gateway before dispatch: the
// cooled slot is skipped at the shared gate rather than rediscovered upstream.
func TestMediaCooldownSharedAcrossGateways(t *testing.T) {
	limiter := mediaLimiter(t)
	h := newMediaHarness(t)
	h.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, h.gateway.log)
	_, server2, sink2 := secondMediaGateway(t, h, limiter)

	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		status(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`)(w, r)
	})
	h.mock.set("b", status(200, `{"data":[{"url":"https://example.com/i.png"}]}`))

	resp, _ := mediaRequest(t, h, http.MethodPost, "/v1/images/generations",
		[]byte(`{"model":"team-chat","prompt":"photo"}`), nil)
	if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 || h.mock.count("b") != 1 {
		t.Fatalf("first request: status=%d calls a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
	}

	// The second gateway has no local memory of the rejection: only the
	// shared cooldown can keep provider a's slot out of this dispatch.
	resp2, _ := postMedia(t, server2.URL+"/v1/images/generations",
		[]byte(`{"model":"team-chat","prompt":"photo"}`), nil)
	if resp2.StatusCode != http.StatusOK || h.mock.count("a") != 1 || h.mock.count("b") != 2 {
		t.Fatalf("shared cooldown not honored: status=%d calls a=%d b=%d", resp2.StatusCode, h.mock.count("a"), h.mock.count("b"))
	}
	env := sink2.last(t)
	if len(env.Attempts) != 1 || env.Attempts[0].UpstreamModel != modelB {
		t.Fatalf("cooled slot was dispatched: %+v", env.Attempts)
	}
}

// TestMediaConnectionQuotaRejectsOnceForSiblingCredentials proves a
// connection-wide quota refusal stops the sibling credentials that share the
// connection instead of reserving — and recording — through each of them.
func TestMediaConnectionQuotaRejectsOnceForSiblingCredentials(t *testing.T) {
	limiter := mediaLimiter(t)
	h := newMediaHarness(t)
	h.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, h.gateway.log)

	release := h.rt.release
	snapshot := release.Snapshot
	siblingID, credentialID := uuid.NewString(), uuid.NewString()
	credentials := map[string][]byte{credentialID: []byte("sibling-secret")}
	for id, provider := range snapshot.Providers {
		for _, slot := range provider.Slots {
			credentials[*slot.CredentialID], _ = release.Credential(*slot.CredentialID)
		}
		if provider.Name == "a" {
			one := int64(1)
			provider.Limits = &runtime.Limits{RequestsPerMinute: &one}
			provider.Slots = append(provider.Slots, runtime.Slot{ID: siblingID, Name: "sibling", Enabled: true, Priority: 1, Weight: 1, CredentialID: &credentialID})
			snapshot.Providers[id] = provider
		}
	}
	var err error
	h.rt.release, err = runtime.NewRelease(release.ID, release.Sequence, snapshot, credentials)
	if err != nil {
		t.Fatal(err)
	}
	aID, a := mediaProvider(t, h, "a")
	held, err := limiter.Reserve(t.Context(), connectionRequest(&a, 0, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Refund(t.Context())

	resp, _ := mediaRequest(t, h, http.MethodPost, "/v1/images/generations",
		[]byte(`{"model":"team-chat","prompt":"photo"}`), nil)
	if resp.StatusCode != http.StatusOK || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
		t.Fatalf("status=%d calls a=%d b=%d", resp.StatusCode, h.mock.count("a"), h.mock.count("b"))
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[0].Class != classRateLimit || env.Attempts[0].SlotID != h.slotA ||
		env.Attempts[1].Class != classSuccess || env.Attempts[1].UpstreamModel != modelB {
		t.Fatalf("connection rejection tried sibling credentials: %+v", env.Attempts)
	}
	if h.gateway.OpenCircuits() != 0 {
		t.Fatal("local admission rejection opened the provider circuit")
	}
	if _, granted := h.gateway.health.claim(aID); !granted {
		t.Fatal("local admission rejection retained the provider probe")
	}
}

func TestVideoCreatePreservesAttemptOrdinalsAfterSlotQuotaRejection(t *testing.T) {
	f := seedMediaFixture(t, "api_key", true)
	limiter := mediaLimiter(t)
	f.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, f.log)
	route := f.snapshot.Routes["video-default"]
	route.MaxAttempts = 2
	f.snapshot.Routes["video-default"] = route

	provider := f.snapshot.Providers[f.providerID]
	rejected := provider.Slots[0]
	rejected.ID = uuid.NewString()
	rejected.Priority = -1
	one := int64(1)
	rejected.RequestsPerMinute = &one
	provider.Slots = append([]runtime.Slot{rejected}, provider.Slots...)
	f.snapshot.Providers[f.providerID] = provider
	held, err := limiter.Reserve(t.Context(), slotRequest(&rejected, 0, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Refund(t.Context())

	resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	body := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusCreated || f.upstream.createCalls.Load() != 1 {
		t.Fatalf("status=%d body=%v creates=%d", resp.StatusCode, body, f.upstream.createCalls.Load())
	}
	attempts := f.sink.last(t).Attempts
	if len(attempts) != 2 {
		t.Fatalf("attempts=%+v", attempts)
	}
	if attempts[0].Ordinal != 1 || attempts[0].Class != classRateLimit || attempts[0].SlotID != rejected.ID ||
		attempts[1].Ordinal != 2 || attempts[1].Class != classSuccess || attempts[1].SlotID != f.slotID {
		t.Fatalf("attempts=%+v", attempts)
	}
}

// TestVideoCreateKeepsSingleUpstreamAttemptUnderWiderBudget proves widening
// the route budget cannot turn one durable create into repeated upstream
// work: an ambiguous failure retires the reservation without a second create.
func TestVideoCreateKeepsSingleUpstreamAttemptUnderWiderBudget(t *testing.T) {
	f := seedMediaFixture(t, "none", false)
	route := f.snapshot.Routes["video-default"]
	route.MaxAttempts = 3
	f.snapshot.Routes["video-default"] = route
	f.upstream.createStatus.Store(http.StatusInternalServerError)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/v1/videos", strings.NewReader(videoCreateBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.bearer)
	req.Header.Set("Content-Type", videoCreateContentType)
	req.Header.Set(routingHeader, `{"max_attempts":3}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "ambiguous_upstream_result" {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if got := f.upstream.createCalls.Load(); got != 1 {
		t.Fatalf("durable create dispatched %d upstream times", got)
	}
	var state string
	if err := f.pool.QueryRow(t.Context(), "SELECT lifecycle_state FROM olp.media_jobs").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(media.LifecycleCreateAmbiguous) {
		t.Fatalf("lifecycle %q", state)
	}
	env := f.sink.last(t)
	if len(env.Attempts) != 1 || env.Attempts[0].Class != classAmbiguous {
		t.Fatalf("attempts %+v", env.Attempts)
	}
}

// TestVideoCreateReleasesProbeWhenReservationFails proves a half-open probe
// admitted for the durable create is returned when the local persistence step
// fails before any upstream dispatch.
func TestVideoCreateReleasesProbeWhenReservationFails(t *testing.T) {
	f := seedMediaFixture(t, "none", false)
	for i := 0; i < circuitFailures; i++ {
		f.gateway.health.record(f.providerID, AttemptFact{Class: classConnect})
	}
	f.gateway.health.mu.Lock()
	f.gateway.health.provider(f.providerID).openUntil = time.Now().Add(-time.Second)
	f.gateway.health.mu.Unlock()

	f.pool.Close() // durable reservation can no longer persist
	resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	body := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || errorCode(t, body) != "media_job_unavailable" {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if got := f.upstream.createCalls.Load(); got != 0 {
		t.Fatalf("create dispatched without a durable reservation: %d", got)
	}
	if got := f.gateway.OpenCircuits(); got != 0 {
		t.Fatalf("abandoned create retained the half-open probe: %d", got)
	}
	if _, granted := f.gateway.health.claim(f.providerID); !granted {
		t.Fatal("released probe cannot be claimed again")
	}
}

// TestRetainedVideoOperationsStayPinnedToRecordedTarget proves request
// routing preferences are never reinterpreted as permission to move a
// historical job: the recorded provider target is used regardless.
func TestRetainedVideoOperationsStayPinnedToRecordedTarget(t *testing.T) {
	f := seedMediaFixture(t, "none", false)
	resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	created := decodeJSON(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, created)
	}
	videoID := created["id"].(string)

	get := func(headers map[string]string) (*http.Response, map[string]any) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/v1/videos/"+videoID, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+f.bearer)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, decodeJSON(t, resp)
	}

	// A preference that would exclude the pinned provider during planning is
	// not a reason to reroute — or refuse — the recorded job.
	resp, got := get(map[string]string{routingHeader: `{"ignore":["provider:` + f.providerID + `"],"only":["provider:` + uuid.NewString() + `"]}`})
	if resp.StatusCode != http.StatusOK || got["id"] != videoID {
		t.Fatalf("pinned get: status=%d body=%v", resp.StatusCode, got)
	}
	if got := f.upstream.getCalls.Load(); got != 1 {
		t.Fatalf("pinned get upstream calls %d", got)
	}

	// Header validation still applies to retained operations.
	resp, _ = get(map[string]string{routingHeader: `not json`})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed routing header on pinned get: status=%d", resp.StatusCode)
	}
	if got := f.upstream.getCalls.Load(); got != 1 {
		t.Fatalf("rejected request reached upstream %d times", got)
	}
}
