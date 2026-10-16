package gateway

import (
	"maps"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
)

func TestAttributionRequirementsAndPinsApplyWithoutCallerAllowlisting(t *testing.T) {
	authority := access.Authority{Policy: access.KeyPolicy{AllowedAttributionKeys: []string{"env"}, RequiredAttributionKeys: []string{"env"}, AttributionDefaults: map[string]string{"team": "core"}}, ProjectAttributionPolicy: attribution.Policy{Required: []string{"team"}}}
	if _, err := parseAttributionValues(nil, authority); err == nil || err.Code != "missing_attribution" {
		t.Fatalf("missing: %v", err)
	}
	got, err := parseAttributionValues([]string{`{"env":"prod","team":"core"}`}, authority)
	if err != nil || !maps.Equal(got, map[string]string{"env": "prod", "team": "core"}) {
		t.Fatalf("resolved=%v %v", got, err)
	}
	if _, err := parseAttributionValues([]string{`{"env":"prod","team":"other"}`}, authority); err == nil || err.Code != "pinned_attribution_override" {
		t.Fatalf("override: %v", err)
	}
	if !authority.AllowsAttribution(got) {
		t.Fatal("original labels refused")
	}
	authority.ProjectAttributionPolicy.Required = append(authority.ProjectAttributionPolicy.Required, "task")
	if authority.AllowsAttribution(got) {
		t.Fatal("new required label did not invalidate established session")
	}
}
