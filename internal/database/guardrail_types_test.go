package database

import (
	"encoding/json"
	"io/fs"
	"os"
	"regexp"
	"testing"
)

// Application type additions must have a forward migration; otherwise valid
// management requests and configuration imports fail at the persistence layer.
func TestGuardrailTypesCanBePersisted(t *testing.T) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	constraint := regexp.MustCompile(`(?s)ADD CONSTRAINT guardrails_type_check CHECK\s*\(type IN\s*\((.*?)\)\)`)
	var latest []byte
	for _, entry := range entries {
		sql, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range constraint.FindAllSubmatch(sql, -1) {
			latest = match[1]
		}
	}
	if len(latest) == 0 {
		t.Fatal("guardrail type constraint is missing")
	}
	persisted := map[string]bool{}
	for _, match := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(latest, -1) {
		persisted[string(match[1])] = true
	}
	raw, err := os.ReadFile("../../openapi/management.json")
	if err != nil {
		t.Fatal(err)
	}
	var api struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &api); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CreateGuardrailRequest", "GuardrailResponse", "ConfigurationGuardrail"} {
		kinds := api.Components.Schemas[name].Properties["type"].Enum
		if len(kinds) == 0 {
			t.Fatalf("%s type contract is missing", name)
		}
		if len(kinds) != len(persisted) {
			t.Errorf("%s exposes %d types but the database allows %d", name, len(kinds), len(persisted))
		}
		for _, kind := range kinds {
			if !persisted[kind] {
				t.Errorf("%s accepts %q without a migration permitting persistence", name, kind)
			}
		}
	}
}
