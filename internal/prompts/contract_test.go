package prompts

import (
	"strings"
	"testing"
)

func TestTemplateLanguage(t *testing.T) {
	d := Definition{Template: "Hello {{name}} / {{name}}", Variables: []string{"name"}}
	got, err := d.Render(map[string]string{"name": "{{literal}}"})
	if err != nil || got != "Hello {{literal}} / {{literal}}" {
		t.Fatalf("render interpreted a value: %q %v", got, err)
	}
	for _, bad := range []Definition{{Template: "{{name", Variables: []string{"name"}}, {Template: "{{name}} }}", Variables: []string{"name"}}, {Template: "{{name + other}}", Variables: []string{"name"}}, {Template: "{{name}}", Variables: []string{"name", "name"}}, {Template: "plain", Variables: []string{"unused"}}, {Template: strings.Repeat("é", MaxTemplateBytes/2+1)}, {Template: string([]byte{0xff})}} {
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid definition: %+v", bad)
		}
	}
	for _, vars := range []map[string]string{nil, {"unknown": "x"}, {"name": "x", "extra": "x"}, {"name": strings.Repeat("x", MaxTemplateBytes)}} {
		if _, err = d.Render(vars); err == nil {
			t.Fatal("accepted missing, unknown or oversized variables")
		}
	}
}
