// Package drift compares the reference catalog with what vendors list today,
// so maintainers can see where the catalog has fallen behind. It proposes
// nothing on its own: a person checks each finding against the vendor's
// pages, edits the catalog and re-signs it.
package drift

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/catalog"
)

// StaleAfter is how old an observation may grow before it is re-checked.
const StaleAfter = 90 * 24 * time.Hour

// Report is what changed between the catalog and the vendors' listings.
type Report struct {
	// Unlisted are models a vendor lists that the catalog does not describe.
	Unlisted map[string][]string
	// Missing are catalog models a vendor no longer lists and the catalog
	// does not mark retired.
	Missing map[string][]string
	// Retired are catalog models past their documented retirement that a
	// vendor still lists, or that the catalog still describes.
	Retired map[string][]string
	// Stale are catalog models whose prices were observed longer ago than
	// StaleAfter.
	Stale map[string][]string
	// Compared are the catalog vendors whose listing was compared, and
	// Unchecked those with no listing to compare against.
	Compared, Unchecked []string
}

// Compare reports drift between a catalog and the models each vendor lists,
// by vendor identifier. A vendor absent from listed is not compared.
func Compare(c *catalog.Catalog, listed map[string][]string, now time.Time) Report {
	report := Report{Unlisted: map[string][]string{}, Missing: map[string][]string{}, Retired: map[string][]string{}, Stale: map[string][]string{}}
	for _, vendor := range c.Vendors {
		names := map[string]bool{}
		for _, model := range vendor.Models {
			names[model.ID] = true
			for _, alias := range model.Aliases {
				names[alias] = true
			}
			if retired(model, now) {
				report.Retired[vendor.ID] = append(report.Retired[vendor.ID], model.ID)
			}
			for _, price := range model.Prices {
				if now.Sub(price.Provenance.ObservedAt) > StaleAfter {
					report.Stale[vendor.ID] = append(report.Stale[vendor.ID], model.ID)
					break
				}
			}
		}
		models, compared := listed[vendor.ID]
		if !compared {
			report.Unchecked = append(report.Unchecked, vendor.ID)
			continue
		}
		report.Compared = append(report.Compared, vendor.ID)
		listing := map[string]bool{}
		for _, name := range models {
			listing[name] = true
			if !names[name] {
				report.Unlisted[vendor.ID] = append(report.Unlisted[vendor.ID], name)
			}
		}
		for _, model := range vendor.Models {
			present := listing[model.ID] || slices.ContainsFunc(model.Aliases, func(alias string) bool { return listing[alias] })
			if !present && !retired(model, now) {
				report.Missing[vendor.ID] = append(report.Missing[vendor.ID], model.ID)
			}
		}
	}
	for _, findings := range []map[string][]string{report.Unlisted, report.Missing, report.Retired, report.Stale} {
		for vendor := range findings {
			slices.Sort(findings[vendor])
		}
	}
	slices.Sort(report.Unchecked)
	return report
}

func retired(model catalog.Model, now time.Time) bool {
	return model.Lifecycle != nil && model.Lifecycle.RetiresAt != nil && !model.Lifecycle.RetiresAt.Time().After(now)
}

// Empty reports a catalog in step with every vendor compared.
func (r Report) Empty() bool {
	return len(r.Unlisted) == 0 && len(r.Missing) == 0 && len(r.Retired) == 0 && len(r.Stale) == 0
}

// Markdown renders the report for a maintainer.
func (r Report) Markdown(c *catalog.Catalog, now time.Time) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Reference catalog published %s, compared %s.\n\n", c.PublishedAt.UTC().Format(time.DateOnly), now.UTC().Format(time.DateOnly))
	switch {
	case len(r.Compared) == 0:
		out.WriteString("No vendor listing was compared.\n\n")
	case r.Empty():
		fmt.Fprintf(&out, "The catalog matches every vendor compared: %s.\n\n", strings.Join(r.Compared, ", "))
	}
	section := func(title, explanation string, findings map[string][]string) {
		if len(findings) == 0 {
			return
		}
		fmt.Fprintf(&out, "## %s\n\n%s\n\n", title, explanation)
		vendors := make([]string, 0, len(findings))
		for vendor := range findings {
			vendors = append(vendors, vendor)
		}
		slices.Sort(vendors)
		for _, vendor := range vendors {
			fmt.Fprintf(&out, "- **%s**: `%s`\n", vendor, strings.Join(findings[vendor], "`, `"))
		}
		out.WriteString("\n")
	}
	section("Listed but not in the catalog", "Add each from the vendor's pricing and model pages, or note why it has no list price.", r.Unlisted)
	section("In the catalog but no longer listed", "Check the vendor's deprecation page and record the retirement.", r.Missing)
	section("Past retirement", "Remove each retired model from the catalog.", r.Retired)
	section("Observed more than 90 days ago", "Re-read each vendor's pricing page and update the observation time.", r.Stale)
	if len(r.Unchecked) > 0 {
		fmt.Fprintf(&out, "Not compared, no listing credentials: %s.\n", strings.Join(r.Unchecked, ", "))
	}
	return out.String()
}
