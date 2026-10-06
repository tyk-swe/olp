package vendors

import (
	"os"
	"strings"
	"testing"
)

// TestConfigurationGuideListsEveryPreset keeps the preset table in
// docs/configuration.md in step with the reviewed contracts.
func TestConfigurationGuideListsEveryPreset(t *testing.T) {
	raw, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	guide := string(raw)
	table := guide[strings.Index(guide, "| ID | Provider | Endpoint | Profile |"):]
	table = table[:strings.Index(table, "\n\n")+1]
	listed := strings.Count(table, "\n| `")
	presets := 0
	for _, c := range All() {
		if c.Preset == nil {
			continue
		}
		presets++
		endpoint := "`" + c.Endpoint + "`"
		if c.Preset.Placeholder {
			endpoint += " (placeholder)"
		}
		profile := ""
		if c.Preset.Profile != nil {
			profile = "`" + c.Preset.Profile.ID + "`"
		}
		row := "| `" + c.ID + "` | " + c.Name + " | " + endpoint + " | " + profile + " |"
		if !strings.Contains(table, row+"\n") {
			t.Errorf("docs/configuration.md lacks the preset row %s", row)
		}
	}
	if listed != presets {
		t.Errorf("docs/configuration.md lists %d presets; the catalogue has %d", listed, presets)
	}
}
