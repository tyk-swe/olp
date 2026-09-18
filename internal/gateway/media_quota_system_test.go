package gateway

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
			if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.media_jobs").Scan(&jobs); err != nil {
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
			if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM olp_go.media_jobs WHERE lifecycle_state = 'active'").Scan(&jobs); err != nil {
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
				id := created["id"].(string)
				var pinnedSlot string
				if err := f.pool.QueryRow(t.Context(), "SELECT slot_id::text FROM olp_go.media_jobs WHERE id=$1", id).Scan(&pinnedSlot); err != nil || pinnedSlot != f.slotID {
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
					resp := f.call(t, method, path, "", nil)
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || resp.StatusCode != want {
						t.Fatalf("%s %s: status=%d want=%d body=%s err=%v", method, path, resp.StatusCode, want, body, err)
					}
				}
				call(429)
				if f.upstream.getCalls.Load() != 0 || f.upstream.contentCalls.Load() != 0 || f.upstream.deleteCalls.Load() != 0 {
					t.Fatal("quota-rejected operation reached upstream")
				}
				var lifecycle string
				if err := f.pool.QueryRow(t.Context(), "SELECT lifecycle_state FROM olp_go.media_jobs WHERE id=$1", id).Scan(&lifecycle); err != nil || lifecycle != "active" {
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
