package vendors

import (
	"os"
	"regexp"
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
	header := regexp.MustCompile(`(?m)^\|\s*ID\s*\|\s*Provider\s*\|\s*Endpoint\s*\|\s*Profile\s*\|`)
	found := header.FindStringIndex(guide)
	if found == nil {
		t.Fatal("docs/configuration.md lacks the preset table header")
	}
	table := guide[found[0]:]
	table = table[:strings.Index(table, "\n\n")+1]
	pipes := regexp.MustCompile(`\s*\|\s*`)
	canonical := func(row string) string {
		return pipes.ReplaceAllString(strings.TrimSpace(row), "|")
	}
	canonicalTable := ""
	for _, line := range strings.Split(table, "\n") {
		canonicalTable += canonical(line) + "\n"
	}
	listed := strings.Count(canonicalTable, "\n|`")
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
		row := "|`" + c.ID + "`|" + c.Name + "|" + endpoint + "|" + profile + "|"
		if !strings.Contains(canonicalTable, row+"\n") {
			t.Errorf("docs/configuration.md lacks the preset row %s", row)
		}
	}
	if listed != presets {
		t.Errorf("docs/configuration.md lists %d presets; the catalogue has %d", listed, presets)
	}
}
