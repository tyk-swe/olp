//go:build integration

package gateway

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestIntegrationVideoLifecycleReservesAndSettlesSharedQuotas(t *testing.T) {
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
					provider.Limits = &routing.Limits{RequestsPerMinute: &one, MaxConcurrency: &one}
					request = connectionRequest(&provider, 0, time.Minute, "")
				case "credential":
					provider.Slots[0].RequestsPerMinute = &one
					provider.Slots[0].MaxConcurrency = &one
					request = slotRequest(&provider.Slots[0], 0, time.Minute, "")
				}
				f.rt.keys[f.bearer] = authority
				f.snapshot.Providers[f.providerID] = provider
				f.republish(t)
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
	client, namespace := mediaValkey(t)
	limiter, err := limits.New(client, namespace)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}

// mediaValkey connects to the disposable service provisioned by make integration
// and names a namespace in it that is emptied when the test ends.
func mediaValkey(t *testing.T) (*coordination.Client, string) {
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
	return client, namespace
}
