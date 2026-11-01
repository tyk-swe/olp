package database

import (
	"encoding/json"
	"io/fs"
	"os"
	"regexp"
	"testing"

	"github.com/tyk-swe/olp/internal/retainedapi"
)

// Keep resource declarations, management filters and persistence in step.
// A newly registered kind must not fail only after a provider accepted work.
func TestRetainedResourceKindsCanBePersisted(t *testing.T) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	constraint := regexp.MustCompile(`(?s)ADD CONSTRAINT provider_resources_kind_check CHECK\s*\(kind IN\s*\((.*?)\)\)`)
	var latest []byte
	for _, entry := range entries {
		raw, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range constraint.FindAllSubmatch(raw, -1) {
			latest = match[1]
		}
	}
	if len(latest) == 0 {
		t.Fatal("resource kind constraint is missing")
	}
	persisted := map[string]bool{}
	for _, match := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(latest, -1) {
		persisted[string(match[1])] = true
	}
	raw, err := os.ReadFile("../../openapi/management.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(document.Components.Schemas["ProviderResourceItem"], &schema); err != nil {
		t.Fatal(err)
	}
	var property struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(schema.Properties["kind"], &property); err != nil {
		t.Fatal(err)
	}
	exposed := map[string]bool{}
	for _, kind := range property.Enum {
		exposed[kind] = true
		if !persisted[kind] {
			t.Errorf("management kind %q has no persistence contract", kind)
		}
	}
	for _, kind := range retainedapi.Kinds() {
		if !persisted[kind] || !exposed[kind] {
			t.Errorf("retained kind %q cannot be persisted and inspected", kind)
		}
	}
}
