package providers

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestProviderSummaryPreservesListContract(t *testing.T) {
	fields := []string{
		"id", "name", "kind", "vendor_id", "state", "connector_ready", "etag",
		"pending_activation", "active_revision", "created_by_email", "last_probe_at",
		"last_probe_status", "created_at", "updated_at", "model_count",
		"enabled_model_count", "capability_count", "certified_capability_count",
		"project_id", "project_name",
	}
	nullableFields := []string{"vendor_id", "active_revision", "created_by_email", "last_probe_at", "last_probe_status", "project_id", "project_name"}
	detailFields := []string{"configuration", "draft_credential_id", "draft_credential_version", "runtime_credential_id", "runtime_credential_version", "last_probe_detail"}
	at := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	for _, populated := range []bool{false, true} {
		name := "null fields"
		if populated {
			name = "populated fields"
		}
		t.Run(name, func(t *testing.T) {
			d := detail{
				providerSummary: providerSummary{
					ID: "provider", Name: "Example", Kind: KindOpenAI, State: "active",
					ConnectorReady: true, ETag: "etag", PendingActivation: true,
					CreatedAt: at, UpdatedAt: at,
					ModelCount: 9007199254740993, EnabledModelCount: 2,
					CapabilityCount: 3, CertifiedCapabilityCount: 1,
				},
				Configuration:     Configuration{Kind: KindOpenAI, AuthMode: AuthAPIKey},
				DraftCredentialID: new("draft"), DraftCredentialVersion: new(2),
				RuntimeCredentialID: new("runtime"), RuntimeCredentialVersion: new(1),
				LastProbeDetail: new("provider diagnostic"),
			}
			if populated {
				d.VendorID = new("openai")
				d.ProjectID = new("project")
				d.ProjectName = new("Project")
				d.ActiveRevision = new(1)
				d.CreatedByEmail = new("operator@example.test")
				d.LastProbeAt = &at
				d.LastProbeStatus = new("success")
			}
			encode := func(value any) map[string]json.RawMessage {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				return fields
			}
			summary, full := encode(d.providerSummary), encode(d)
			if len(summary) != len(fields) || len(full) != len(fields)+len(detailFields) {
				t.Fatalf("unexpected response fields: summary=%v detail=%v", summary, full)
			}
			for _, field := range fields {
				value, ok := summary[field]
				if !ok || string(value) != string(full[field]) {
					t.Errorf("field %s differs between list and detail: %s, %s", field, value, full[field])
				}
				if slices.Contains(nullableFields, field) && (string(value) == "null") == populated {
					t.Errorf("field %s has incorrect null presence: %s", field, value)
				}
			}
			for _, field := range detailFields {
				if _, ok := summary[field]; ok {
					t.Errorf("detail-only field %s leaked into list", field)
				}
				if _, ok := full[field]; !ok {
					t.Errorf("detail-only field %s is missing", field)
				}
			}
			if string(summary["model_count"]) != "9007199254740993" {
				t.Errorf("model count lost precision: %s", summary["model_count"])
			}
		})
	}
}
