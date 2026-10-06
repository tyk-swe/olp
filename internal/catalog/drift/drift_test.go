package drift

import (
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/catalog"
)

func TestCompareFindsEveryKindOfDrift(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	fresh := catalog.Provenance{SourceURL: "https://example.test", ObservedAt: now.Add(-24 * time.Hour)}
	old := catalog.Provenance{SourceURL: "https://example.test", ObservedAt: now.Add(-100 * 24 * time.Hour)}
	retires := catalog.Date("2026-10-01")
	model := func(id string, provenance catalog.Provenance, aliases ...string) catalog.Model {
		return catalog.Model{ID: id, Aliases: aliases, Prices: []catalog.Price{{Operation: "generation", Provenance: provenance}}}
	}
	retiredModel := model("retired", fresh)
	retiredModel.Lifecycle = &catalog.Lifecycle{RetiresAt: &retires}
	c := &catalog.Catalog{PublishedAt: now, Vendors: []catalog.Vendor{
		{ID: "acme", Models: []catalog.Model{model("current", fresh, "current-2026"), model("gone", fresh), model("dusty", old), retiredModel}},
		{ID: "quiet", Models: []catalog.Model{model("other", fresh)}},
	}}
	report := Compare(c, map[string][]string{"acme": {"current-2026", "dusty", "brand-new", "retired"}}, now)
	if strings.Join(report.Unlisted["acme"], ",") != "brand-new" || strings.Join(report.Missing["acme"], ",") != "gone" ||
		strings.Join(report.Retired["acme"], ",") != "retired" || strings.Join(report.Stale["acme"], ",") != "dusty" ||
		strings.Join(report.Unchecked, ",") != "quiet" {
		t.Fatalf("report = %+v", report)
	}
	text := report.Markdown(c, now)
	for _, want := range []string{"## Listed but not in the catalog", "**acme**: `brand-new`", "## Past retirement", "Not compared, no listing credentials: quiet."} {
		if !strings.Contains(text, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, text)
		}
	}
}

func TestAnInStepCatalogReportsNothing(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	c := &catalog.Catalog{PublishedAt: now, Vendors: []catalog.Vendor{{ID: "acme", Models: []catalog.Model{{ID: "m", Prices: []catalog.Price{{Provenance: catalog.Provenance{ObservedAt: now}}}}}}}}
	report := Compare(c, map[string][]string{"acme": {"m"}}, now)
	if !report.Empty() || !strings.Contains(report.Markdown(c, now), "matches every vendor") {
		t.Fatalf("report = %+v", report)
	}
}
