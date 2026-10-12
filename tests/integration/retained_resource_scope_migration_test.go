//go:build integration

package integration_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRetainedNativeSourceBackfillKeepsHistoricalPins(t *testing.T) {
	for _, scenario := range []string{"proven", "missing-model", "numeric-model", "duplicate-target", "invalid-target", "malformed-targets", "wrong-slot"} {
		t.Run(scenario, func(t *testing.T) {
			h := newAccessHarness(t)
			fixture := newNativeScopeFixture(t)
			owner, _, route, _ := provisionOpenAI(t, h, fixture.URL,
				[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
			key := stateKey(t, h, owner, route, true)
			h.refresh()
			status, body, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(`{"model":"`+route+`","input":"historical identity","store":true}`), map[string]string{"Content-Type": "application/json"})
			if status != 200 {
				t.Fatalf("seed retained resource: %d %s", status, body)
			}
			localID := nativeScopeResponseID(t, body, false)
			var rowID, providerRevision, routeRevision, targetID, slotID, metadata string
			if err := h.Pool.QueryRow(t.Context(), `SELECT id::text,provider_revision_id::text,route_revision_id::text,target_id::text,slot_id::text,metadata::text FROM olp.provider_resources WHERE upstream_id='resp-local-1'`).Scan(&rowID, &providerRevision, &routeRevision, &targetID, &slotID, &metadata); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Pool.Exec(t.Context(), `DROP INDEX olp.provider_resources_native_identity;
ALTER TABLE olp.provider_resources DROP COLUMN target_id;
ALTER TABLE olp.provider_resources ADD CONSTRAINT provider_resources_kind_provider_id_upstream_id_key UNIQUE(kind,provider_id,upstream_id)`); err != nil {
				t.Fatal(err)
			}
			var targets []map[string]any
			var encoded []byte
			if err := h.Pool.QueryRow(t.Context(), `SELECT targets FROM olp.route_revisions WHERE id=$1`, routeRevision).Scan(&encoded); err != nil || json.Unmarshal(encoded, &targets) != nil {
				t.Fatalf("historical targets: %v", err)
			}
			switch scenario {
			case "missing-model":
				if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=metadata-'upstream_model' WHERE id=$1`, rowID); err != nil {
					t.Fatal(err)
				}
			case "numeric-model":
				if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{upstream_model}','123') WHERE id=$1`, rowID); err != nil {
					t.Fatal(err)
				}
				targets[0]["provider_model"] = "123"
				if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_revisions SET models=jsonb_set(models,'{0,upstream_model}','"123"') WHERE id=$1`, providerRevision); err != nil {
					t.Fatal(err)
				}
			case "duplicate-target":
				targets = append(targets, targets[0])
			case "invalid-target":
				targets[0]["id"] = "not-a-target-uuid"
			case "malformed-targets":
				if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.route_revisions SET targets='{}' WHERE id=$1`, routeRevision); err != nil {
					t.Fatal(err)
				}
			case "wrong-slot":
				if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET slot_id=$1 WHERE id=$2`, uuid.NewString(), rowID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "numeric-model" || scenario == "duplicate-target" || scenario == "invalid-target" {
				encoded, err := json.Marshal(targets)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.route_revisions SET targets=$1 WHERE id=$2`, encoded, routeRevision); err != nil {
					t.Fatal(err)
				}
			}
			migration, err := os.ReadFile("../../internal/database/migrations/0062_resource_native_identity.sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.Pool.Exec(t.Context(), string(migration)); err != nil {
				t.Fatal(err)
			}
			var actualTarget *string
			var actualRow, actualProvider, actualRoute, actualSlot, actualMetadata string
			if err := h.Pool.QueryRow(t.Context(), `SELECT id::text,provider_revision_id::text,route_revision_id::text,slot_id::text,metadata::text,target_id::text FROM olp.provider_resources WHERE id=$1`, rowID).Scan(&actualRow, &actualProvider, &actualRoute, &actualSlot, &actualMetadata, &actualTarget); err != nil {
				t.Fatal(err)
			}
			if actualRow != rowID || actualProvider != providerRevision || actualRoute != routeRevision || (scenario != "wrong-slot" && actualSlot != slotID) {
				t.Fatal("source migration repinned or replaced an existing local resource")
			}
			if scenario == "proven" {
				if actualTarget == nil || *actualTarget != targetID || actualMetadata != metadata {
					t.Fatal("proven historical identity was not preserved exactly")
				}
				status, body, _ := h.gatewayRaw("GET", "/v1/responses/"+localID, key, nil, nil)
				if status != 200 || !strings.Contains(string(body), "account-A") {
					t.Fatalf("historical local alias changed: %d %s", status, body)
				}
			} else {
				if actualTarget != nil {
					t.Fatal("unproven historical source acquired a guessed target")
				}
				before := len(fixture.captured())
				status, _, _ := h.gatewayRaw("GET", "/v1/responses/"+localID, key, nil, nil)
				if status < 400 || len(fixture.captured()) != before {
					t.Fatal("unproven historical source dispatched provider work")
				}
			}
		})
	}
}
