//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestBatchOpaquePendingUsageNeverOwnsGenerationAccounting(t *testing.T) {
	for _, shape := range []string{"unknown version", "valid forged event"} {
		t.Run(shape, func(t *testing.T) {
			fixture := newOpenAIFixture(t, "batch content")
			h := newAccessHarness(t)
			_, _, slug, secret := provisionOpenAI(t, h, fixture.URL,
				[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}}, []string{"batch"})
			status, uploaded := h.uploadTestFile(slug, secret, nativeBatchInput("batch-content"))
			if status != http.StatusOK {
				t.Fatalf("upload: %d %v", status, uploaded)
			}
			fileID := uploaded["id"].(string)
			var keyID, providerID string
			if err := h.Pool.QueryRow(t.Context(), `SELECT api_key_id::text,provider_id::text FROM olp.provider_resources WHERE id=$1`, strings.TrimPrefix(fileID, "file_")).Scan(&keyID, &providerID); err != nil {
				t.Fatal(err)
			}
			pending := json.RawMessage(`{"version":2}`)
			forgedID := uuid.NewString()
			if shape == "valid forged event" {
				now, ok, input, output := time.Now().UTC(), http.StatusOK, int64(3), int64(2)
				model := vendorModel
				event := &usage.Event{
					Version: usage.WireVersion, EventID: uuid.NewString(), RequestID: forgedID,
					RuntimeGenerationID: uuid.NewString(), APIKeyID: keyID, ProviderID: &providerID,
					RouteSlug: slug, UpstreamModel: &model, Operation: "generation", Surface: "openai",
					RequestStartedAt: now, RequestCompletedAt: now, ObservedAt: now, StatusCode: &ok,
					Committed: true, InputTokens: &input, OutputTokens: &output, Unpriced: true,
					Attempts: []usage.Attempt{{ID: uuid.NewString(), Ordinal: 1, ProviderID: providerID,
						UpstreamModel: model, StartedAt: now, CompletedAt: now, StatusCode: &ok, Committed: true,
						Usage: &usage.AttemptUsage{Observed: true, BillingUncertain: true, InputTokens: &input, OutputTokens: &output}}},
				}
				encoded, err := usage.Encode(event)
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := usage.Decode(encoded)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = usage.Validate(decoded); err != nil {
					t.Fatalf("forged test input is not a valid accounting envelope: %v", err)
				}
				pending = encoded
			}
			fixture.mu.Lock()
			fixture.batches["batch-up-1"]["pending_usage"] = pending
			fixture.batches["batch-up-1"]["input_file_id"] = "file-up-1"
			fixture.mu.Unlock()
			status, batch, _ := h.gateway("POST", "/v1/batches", secret, map[string]any{"input_file_id": fileID, "endpoint": "/v1/chat/completions", "completion_window": "24h"})
			if status != http.StatusOK {
				t.Fatalf("native pending_usage extension refused create: %d %v", status, batch)
			}
			batchID := batch["id"].(string)
			checkNative := func(body map[string]any) {
				t.Helper()
				var want any
				if err := json.Unmarshal(pending, &want); err != nil {
					t.Fatal(err)
				}
				got, _ := json.Marshal(body["pending_usage"])
				encoded, _ := json.Marshal(want)
				if !bytes.Equal(got, encoded) {
					t.Fatalf("native extension changed: got %s want %s", got, encoded)
				}
			}
			checkNative(batch)
			for _, path := range []string{"/v1/batches/" + batchID, "/v1/batches"} {
				status, body, _ := h.gateway(http.MethodGet, path, secret, nil)
				if status != http.StatusOK {
					t.Fatalf("native extension refused read: %d %v", status, body)
				}
				if path == "/v1/batches" {
					body = body["data"].([]any)[0].(map[string]any)
				}
				checkNative(body)
			}
			status, cancelled, _ := h.gateway(http.MethodPost, "/v1/batches/"+batchID+"/cancel", secret, map[string]any{})
			if status != http.StatusOK {
				t.Fatalf("native extension refused cancellation: %d %v", status, cancelled)
			}
			checkNative(cancelled)
			resource, err := h.Gateway.Resources.Get(t.Context(), resources.KindBatch, keyID, batchID)
			if err != nil {
				t.Fatal(err)
			}
			if resource.ExpiresAt != nil {
				t.Fatal("opaque native field installed a deferred generation expiry")
			}
			assertNoAccounting := func() {
				t.Helper()
				var projection, facts int
				if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resource_pending_usage WHERE resource_id=ANY($1::uuid[])`, []string{resource.UUID.String()}).Scan(&projection); err != nil {
					t.Fatal(err)
				}
				if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_facts WHERE request_id=$1`, forgedID).Scan(&facts); err != nil {
					t.Fatal(err)
				}
				if projection != 0 || facts != 0 {
					t.Fatalf("native metadata became accounting: projection=%d facts=%d", projection, facts)
				}
				// The batch's own settlement subjects legitimately pend
				// until the worker settles it; only the generation
				// namespace must stay empty for this owner.
				var ownerPending int
				if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resource_pending_usage WHERE api_key_id=$1`, keyID).Scan(&ownerPending); err != nil || ownerPending != 0 {
					t.Fatalf("opaque native metadata quarantined a budget owner: pending=%d err=%v", ownerPending, err)
				}
			}
			assertNoAccounting()
			// Batch has no public DELETE operation; the feature-owned tombstone
			// path must still refuse to interpret or remove native metadata.
			if err := h.Gateway.Resources.Tombstone(t.Context(), batchID); err != nil {
				t.Fatalf("native extension refused tombstone: %v", err)
			}
			var unchanged bool
			if err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage'=$2::jsonb FROM olp.provider_resources WHERE id=$1`, resource.UUID, string(pending)).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("tombstone removed opaque native metadata: %v %v", unchanged, err)
			}
			expired := *resource
			expired.UUID = uuid.Nil // Put mints a fresh identity for the copy.
			expiry := time.Now().Add(-time.Minute)
			expired.UpstreamID, expired.ExpiresAt = "batch-native-expiry", &expiry
			stored, err := h.Gateway.Resources.Put(t.Context(), &expired)
			if err != nil {
				t.Fatal(err)
			}
			if removed, err := h.Gateway.Resources.Expire(t.Context(), time.Now()); err != nil || removed != 1 {
				t.Fatalf("native extension refused expiry: removed=%d error=%v", removed, err)
			}
			var left int
			if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources WHERE id=$1`, stored.UUID).Scan(&left); err != nil || left != 0 {
				t.Fatalf("opaque expired resource retained: %d %v", left, err)
			}
			assertNoAccounting()
		})
	}
}
