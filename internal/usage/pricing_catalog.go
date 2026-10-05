package usage

import (
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
)

// UnrepresentablePrice is a price component the reference catalog lists for a
// model that pricing revisions cannot yet hold, in the vendor's own terms. A
// snapshot keeps them so an operator publishing it sees what is not priced.
type UnrepresentablePrice struct {
	VendorID  string `json:"vendor_id"`
	Model     string `json:"model"`
	Operation string `json:"operation"`
	Component string `json:"component"`
	Detail    string `json:"detail"`
}

// SkippedPrice is a catalog price this build cannot store: its vendor or
// operation is unknown here, as a newer catalog may name. Refresh reports it
// rather than failing, so an older installation still takes the prices it
// understands.
type SkippedPrice struct {
	VendorID  string `json:"vendor_id"`
	Model     string `json:"model"`
	Operation string `json:"operation"`
	Reason    string `json:"reason"`
}

// SnapshotCatalog is the signed catalog a snapshot was mapped from.
type SnapshotCatalog struct {
	SHA256      string `json:"sha256"`
	PublishedAt string `json:"published_at"`
	KeyID       string `json:"key_id"`
}

// catalogDocument maps a verified catalog to the price document a snapshot
// stores: one vendor-scoped price per model identifier and alias, scoped to
// the connector kind that serves the vendor. Prices match models by exact
// name, so every alias is its own row.
func catalogDocument(c *catalog.Catalog, vendorKind func(string) (string, bool)) (sourceDocument, []SkippedPrice) {
	doc := sourceDocument{Currency: c.Currency, Prices: []Price{}}
	skipped := []SkippedPrice{}
	for _, vendor := range c.Vendors {
		kind, known := "", false
		if vendorKind != nil {
			kind, known = vendorKind(vendor.ID)
		}
		for _, model := range vendor.Models {
			for _, listed := range model.Prices {
				reason := ""
				switch {
				case !known:
					reason = "This installation does not know the vendor."
				case !validOperation(listed.Operation):
					reason = "This installation does not price the operation."
				}
				if reason != "" {
					skipped = append(skipped, SkippedPrice{VendorID: vendor.ID, Model: model.ID, Operation: listed.Operation, Reason: reason})
					continue
				}
				for _, component := range listed.Unrepresentable {
					doc.Unrepresentable = append(doc.Unrepresentable, UnrepresentablePrice{VendorID: vendor.ID, Model: model.ID, Operation: listed.Operation, Component: component.Component, Detail: component.Detail})
				}
				for _, name := range append([]string{model.ID}, model.Aliases...) {
					doc.Prices = append(doc.Prices, Price{
						VendorID: &vendor.ID, ProviderKind: kind, Model: name, Operation: listed.Operation, Currency: c.Currency,
						InputPerMillion: catalogRate(listed.InputPerMillion), CachedInputPerMillion: catalogRate(listed.CachedInputPerMillion),
						CacheWriteInputPerMillion: catalogRate(listed.CacheWriteInputPerMillion), CacheWrite5MInputPerMillion: catalogRate(listed.CacheWrite5MInputPerMillion),
						CacheWrite1HInputPerMillion: catalogRate(listed.CacheWrite1HInputPerMillion), OutputPerMillion: catalogRate(listed.OutputPerMillion), UnitPrice: catalogRate(listed.UnitPrice),
					})
				}
			}
		}
	}
	slices.SortFunc(doc.Unrepresentable, func(a, b UnrepresentablePrice) int {
		return strings.Compare(a.VendorID+"\x00"+a.Model+"\x00"+a.Operation+"\x00"+a.Component, b.VendorID+"\x00"+b.Model+"\x00"+b.Operation+"\x00"+b.Component)
	})
	return doc, skipped
}

func catalogRate(d *catalog.Decimal) *string {
	if d == nil {
		return nil
	}
	value := d.String()
	return &value
}

func (s *Server) referenceCatalog(r *http.Request, _ access.Principal) (access.Reply, error) {
	if s.Catalog == nil {
		return access.Reply{}, access.Fail(503, "reference_catalog_unavailable", "This process holds no verified reference catalog.")
	}
	return access.OK(s.Catalog.Summary()), nil
}
