//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func TestRetainedNativeFileControlsPreserveUnknownUploadAcceptance(t *testing.T) {
	fixture := newOpenAIFixture(t, "file contents")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
	secret := stateKey(t, h, owner, slug, true)
	h.refresh()
	var keyID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp.api_keys WHERE name='state key' AND revoked_at IS NULL`).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	snapshot := h.Gateway.Runtime.Release().Snapshot
	route := snapshot.Routes[slug]
	target := route.Targets[0]
	provider := snapshot.Providers[target.ProviderID]
	slot := provider.Slots[0]
	metadata, _ := json.Marshal(map[string]any{"upstream_model": target.ProviderModel, "file_operation": "generation", "purpose": "user_data", "filename": "input.txt", "bytes": 1})
	version := retainedapi.Revision
	candidate := &resources.Resource{Kind: resources.KindFile, APIKeyID: keyID, RouteSlug: slug, RouteRevisionID: route.RevisionID, TargetID: target.ID, ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, SlotID: slot.ID, CredentialID: slot.CredentialID, ContractVersion: &version, Metadata: metadata}
	store := resources.New(h.Pool)
	file, fresh, err := store.PrepareLifecycle(t.Context(), candidate, "generation:file", "unknown-upload", bytes.Repeat([]byte{1}, 32))
	if err != nil || !fresh {
		t.Fatalf("prepare upload: fresh=%v %v", fresh, err)
	}
	for _, state := range []string{"submitting", "submission_unknown"} {
		if state == "submission_unknown" {
			if err := store.DispatchLifecycle(t.Context(), file.UUID); err != nil {
				t.Fatal(err)
			}
			if err := store.UnknownLifecycle(t.Context(), file.UUID); err != nil {
				t.Fatal(err)
			}
		}
		before := fixture.dials.Load()
		for _, request := range []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodGet, "/content"}, {http.MethodDelete, ""}} {
			status, body, _ := h.gateway(request.method, "/v1/files/"+file.ID+request.suffix, secret, nil)
			if status != http.StatusConflict {
				t.Fatalf("%s file control dispatched unresolved upload: %s %d %v", state, request.method, status, body)
			}
		}
		status, listed, _ := h.gateway(http.MethodGet, "/v1/files?limit=1", secret, nil)
		items, _ := listed["data"].([]any)
		if status != http.StatusOK || len(items) != 0 || listed["has_more"] != false {
			t.Fatalf("%s upload intent appeared as a confirmed file: %d %v", state, status, listed)
		}
		stored, err := store.GetByUUID(t.Context(), file.UUID, keyID)
		if err != nil || stored.State != state || fixture.dials.Load() != before {
			t.Fatalf("unknown acceptance was changed by file controls: resource=%+v calls=%d want=%d error=%v", stored, fixture.dials.Load(), before, err)
		}
	}
	confirmed := *candidate
	confirmed.UUID, confirmed.ID = [16]byte{}, ""
	confirmed.UpstreamID, confirmed.State = "file-confirmed", "processed"
	ready, err := store.Put(t.Context(), &confirmed)
	if err != nil {
		t.Fatal(err)
	}
	// Newer intents must be excluded before the database applies the page limit.
	for i := range 2 {
		pending := *candidate
		pending.UUID, pending.ID = [16]byte{}, ""
		intent, fresh, err := store.PrepareLifecycle(t.Context(), &pending, "generation:file", fmt.Sprintf("pending-page-%d", i), bytes.Repeat([]byte{byte(i + 2)}, 32))
		if err != nil || !fresh {
			t.Fatalf("prepare page intent: fresh=%v %v", fresh, err)
		}
		if i == 1 {
			if err := store.DispatchLifecycle(t.Context(), intent.UUID); err != nil {
				t.Fatal(err)
			}
			if err := store.RejectRetained(t.Context(), intent, "", "", true); err != nil {
				t.Fatal(err)
			}
		}
	}
	status, listed, _ := h.gateway(http.MethodGet, "/v1/files?limit=1", secret, nil)
	items, _ := listed["data"].([]any)
	if status != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["id"] != ready.ID || listed["has_more"] != false {
		t.Fatalf("unresolved intents displaced the confirmed page: %d %v", status, listed)
	}
}
