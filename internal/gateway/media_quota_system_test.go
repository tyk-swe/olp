//go:build integration

package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestVideoCreateReservesAndSettlesSharedQuotas(t *testing.T) {
	for _, scope := range []string{"key", "provider", "credential"} {
		t.Run(scope, func(t *testing.T) {
			limiter := mediaLimiter(t)
			f := seedMediaFixture(t, "none", false)
			f.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, f.log)
			one := int64(1)
			authority := f.rt.keys[f.bearer]
			authority.LookupID = strings.ReplaceAll(uuid.NewString(), "-", "")
			authority.Policy.RequestsPerMinute = &one
			if scope == "key" {
				authority.Policy.MaxConcurrency = &one
			}
			f.rt.keys[f.bearer] = authority
			provider := f.snapshot.Providers[f.providerID]
			var request limits.Request
			switch scope {
			case "key":
				request = keyRequest(authority, 0, time.Minute)
			case "provider":
				provider.Limits = &runtime.Limits{RequestsPerMinute: &one, MaxConcurrency: &one}
				request = connectionRequest(&provider, 0, time.Minute)
			case "credential":
				provider.Slots[0].RequestsPerMinute = &one
				provider.Slots[0].MaxConcurrency = &one
				request = slotRequest(&provider.Slots[0], 0, time.Minute)
			}
			f.snapshot.Providers[f.providerID] = provider
			held, err := limiter.Reserve(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			call := func(want int) {
				t.Helper()
				resp := f.call(t, http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != want {
					t.Fatalf("create status=%d want=%d body=%s err=%v", resp.StatusCode, want, body, err)
				}
			}
			call(http.StatusTooManyRequests)
			var jobs int
			if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.media_jobs").Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if jobs != 0 {
				t.Fatal("quota-rejected request reserved a durable video job")
			}
			if err := held.Refund(t.Context()); err != nil {
				t.Fatal(err)
			}
			call(http.StatusCreated)
			call(http.StatusTooManyRequests)
			if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.media_jobs WHERE lifecycle_state = 'active'").Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if jobs != 1 {
				t.Fatalf("active jobs=%d, want one dispatched create", jobs)
			}
			_, err = limiter.Reserve(t.Context(), request)
			var exceeded *limits.ExceededError
			if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionRequests {
				t.Fatalf("video create refunded the %s RPM reservation: %v", scope, err)
			}
			request.RequestsPerMinute = nil
			free, err := limiter.Reserve(t.Context(), request)
			if err != nil {
				t.Fatalf("video create retained %s concurrency: %v", scope, err)
			}
			if err := free.Refund(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVideoLifecycleReservesAndSettlesSharedQuotas(t *testing.T) {
	for _, scope := range []string{"key", "provider", "credential"} {
		for _, operation := range []string{"list", "get", "content", "delete"} {
			t.Run(scope+"/"+operation, func(t *testing.T) {
				limiter := mediaLimiter(t)
				f := seedMediaFixture(t, "none", false)
				f.upstream.getStatus.Store("queued")
				resp := f.call(t, "POST", "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
				created := decodeJSON(t, resp)
				if resp.StatusCode != 201 {
					t.Fatalf("create: %d %v", resp.StatusCode, created)
				}
				// Reading a response body does not guarantee that the server's
				// deferred quota settlement has run yet.
				f.awaitCompleted(t, 1)
				id := created["id"].(string)
				var pinnedSlot string
				if err := f.pool.QueryRow(t.Context(), "SELECT slot_id::text FROM olp.media_jobs WHERE id=$1", id).Scan(&pinnedSlot); err != nil || pinnedSlot != f.slotID {
					t.Fatalf("job lost selected slot: %q %v", pinnedSlot, err)
				}
				f.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, f.log)
				one := int64(1)
				authority := f.rt.keys[f.bearer]
				authority.LookupID = strings.ReplaceAll(uuid.NewString(), "-", "")
				authority.Policy.MaxConcurrency = &one
				provider := f.snapshot.Providers[f.providerID]
				var request limits.Request
				switch scope {
				case "key":
					authority.Policy.RequestsPerMinute = &one
					request = keyRequest(authority, 0, time.Minute)
				case "provider":
					provider.Limits = &runtime.Limits{RequestsPerMinute: &one, MaxConcurrency: &one}
					request = connectionRequest(&provider, 0, time.Minute)
				case "credential":
					provider.Slots[0].RequestsPerMinute = &one
					provider.Slots[0].MaxConcurrency = &one
					request = slotRequest(&provider.Slots[0], 0, time.Minute)
				}
				f.rt.keys[f.bearer] = authority
				f.snapshot.Providers[f.providerID] = provider
				held, err := limiter.Reserve(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				method, path := "GET", "/v1/videos/"+id
				switch operation {
				case "list":
					path = "/v1/videos"
				case "content":
					path += "/content"
				case "delete":
					method = "DELETE"
				}
				call := func(want int) {
					t.Helper()
					completed := f.completed.Load()
					resp := f.call(t, method, path, "", nil)
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || resp.StatusCode != want {
						t.Fatalf("%s %s: status=%d want=%d body=%s err=%v", method, path, resp.StatusCode, want, body, err)
					}
					f.awaitCompleted(t, completed+1)
				}
				call(429)
				if f.upstream.getCalls.Load() != 0 || f.upstream.contentCalls.Load() != 0 || f.upstream.deleteCalls.Load() != 0 {
					t.Fatal("quota-rejected operation reached upstream")
				}
				var lifecycle string
				if err := f.pool.QueryRow(t.Context(), "SELECT lifecycle_state FROM olp.media_jobs WHERE id=$1", id).Scan(&lifecycle); err != nil || lifecycle != "active" {
					t.Fatalf("quota rejection persisted work for reconciliation: %s %v", lifecycle, err)
				}
				if err := held.Refund(t.Context()); err != nil {
					t.Fatal(err)
				}
				call(200)
				_, err = limiter.Reserve(t.Context(), request)
				var exceeded *limits.ExceededError
				if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionRequests {
					t.Fatalf("request did not consume %s RPM: %v", scope, err)
				}
				request.RequestsPerMinute = nil
				free, err := limiter.Reserve(t.Context(), request)
				if err != nil {
					t.Fatalf("operation retained %s concurrency: %v", scope, err)
				}
				if err := free.Refund(t.Context()); err != nil {
					t.Fatal(err)
				}
				if scope != "key" {
					free, err := limiter.Reserve(t.Context(), keyRequest(authority, 0, time.Minute))
					if err != nil {
						t.Fatalf("operation retained key concurrency: %v", err)
					}
					free.Refund(t.Context())
				}
			})
		}
	}
}

func (f *mediaFixture) awaitCompleted(t *testing.T, minimum int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for f.completed.Load() < minimum {
		select {
		case <-ctx.Done():
			t.Fatalf("media handler did not finish quota settlement: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

// mediaLimiter uses the disposable service provisioned by make integration.
func mediaLimiter(t *testing.T) *limits.Limiter {
	t.Helper()
	endpoint := os.Getenv("OLP_TEST_VALKEY_URL")
	if endpoint == "" {
		t.Fatal("OLP_TEST_VALKEY_URL is required; run make integration")
	}
	cfg, err := coordination.Configuration(endpoint, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client, err := coordination.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	namespace := "olp_media_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		keys, err := client.Do(ctx, "KEYS", namespace+"*")
		if err != nil {
			t.Error(err)
			return
		}
		for _, key := range keys.([]any) {
			if _, err := client.Do(ctx, "DEL", key.(string)); err != nil {
				t.Error(err)
			}
		}
	})
	limiter, err := limits.New(client, namespace)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

func TestMediaKeySettlementChargesDispatchedRequests(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "unary"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			limiter := mediaLimiter(t)
			h := newMediaHarness(t)
			h.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, h.gateway.log)
			authority := h.rt.keys[fullKey]
			authority.LookupID = strings.ReplaceAll(uuid.NewString(), "-", "")
			rpm, tpm, concurrency := int64(1), int64(100), int64(1)
			authority.Policy.RequestsPerMinute = &rpm
			authority.Policy.TokensPerMinute = &tpm
			authority.Policy.MaxConcurrency = &concurrency
			h.rt.keys[fullKey] = authority
			request := `{"model":"team-chat","prompt":"photo"}`
			if streaming {
				request = `{"model":"team-chat","prompt":"photo","stream":true}`
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"type\":\"image_generation.completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3}}\n\n")
				})
			} else {
				h.mock.set("a", status(200, `{"data":[{"url":"https://example.com/i.png"}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`))
			}
			resp := h.do(t.Context(), "POST", "/v1/images/generations", fullKey, []byte(request), nil)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("first status=%d", resp.StatusCode)
			}
			resp = h.do(t.Context(), "POST", "/v1/images/generations", fullKey, []byte(request), nil)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 429 || h.mock.count("a") != 1 {
				t.Fatalf("consumed RPM refunded: status=%d calls=%d", resp.StatusCode, h.mock.count("a"))
			}
			authority.Policy.RequestsPerMinute = nil
			_, err := limiter.Reserve(t.Context(), keyRequest(authority, 96, time.Second))
			var exceeded *limits.ExceededError
			if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionTokens {
				t.Fatalf("final usage not reconciled: %v", err)
			}
			lease, err := limiter.Reserve(t.Context(), keyRequest(authority, 95, time.Second))
			if err != nil {
				t.Fatalf("concurrency not released or usage overcharged: %v", err)
			}
			if err := lease.Refund(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMediaStreamHoldsTargetConcurrencyUntilCompletion(t *testing.T) {
	for _, scope := range []string{"provider", "credential"} {
		t.Run(scope, func(t *testing.T) {
			limiter := mediaLimiter(t)
			h := newMediaHarness(t)
			h.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, h.gateway.log)
			var request limits.Request
			for id, p := range h.rt.release.Snapshot.Providers {
				if p.Name != "a" {
					continue
				}
				one := int64(1)
				if scope == "provider" {
					p.Limits = &runtime.Limits{MaxConcurrency: &one}
					request = connectionRequest(&p, 0, time.Second)
				} else {
					p.Slots[0].MaxConcurrency = &one
					request = slotRequest(&p.Slots[0], 0, time.Second)
				}
				h.rt.release.Snapshot.Providers[id] = p
			}
			finish := make(chan struct{})
			defer func() {
				select {
				case <-finish:
				default:
					close(finish)
				}
			}()
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"type\":\"image_generation.partial_image\"}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-finish:
					io.WriteString(w, "data: {\"type\":\"image_generation.completed\"}\n\n")
				case <-r.Context().Done():
				}
			})
			resp := h.do(t.Context(), "POST", "/v1/images/generations", fullKey,
				[]byte(`{"model":"team-chat","prompt":"photo","stream":true}`), nil)
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			_, err := limiter.Reserve(t.Context(), request)
			var exceeded *limits.ExceededError
			if !errors.As(err, &exceeded) || exceeded.Dimension != limits.DimensionConcurrency {
				t.Fatalf("%s concurrency released at response headers: %v", scope, err)
			}
			close(finish)
			io.Copy(io.Discard, reader)
			lease, err := limiter.Reserve(t.Context(), request)
			if err != nil {
				t.Fatalf("%s concurrency not released after completion: %v", scope, err)
			}
			if err := lease.Refund(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
