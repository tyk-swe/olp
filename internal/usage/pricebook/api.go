package pricebook

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/signing"
)

type API struct {
	Access     *access.Server
	Normalizer Normalizer

	Egress *egress.Policy

	// Catalog is the reference catalog this release ships, verified at
	// start-up; a catalog pricing source without a URL reads it.
	Catalog *catalog.Signed
	// CatalogKeys verify catalogs a pricing source fetches.
	CatalogKeys signing.Keyring
}

func (s *API) Register(mux *http.ServeMux) {
	s.route(mux, "GET /api/v1/pricing/revisions", s.listPricingRevisions)
	// A revision may carry thousands of rates, well past the default body cap.
	s.route(mux, "POST /api/v1/pricing/revisions", s.createPricingRevision, access.MaxBody(1<<20))
	s.route(mux, "GET /api/v1/reference-catalog", s.referenceCatalog)
	s.route(mux, "GET /api/v1/pricing/discounts", s.listPricingDiscounts)
	s.route(mux, "POST /api/v1/pricing/discounts", s.createPricingDiscount)
	s.route(mux, "POST /api/v1/pricing/quote", s.quotePricing)
	s.route(mux, "GET /api/v1/rate-cards", s.listRateCards)
	s.route(mux, "POST /api/v1/rate-cards", s.createRateCard)
	s.route(mux, "GET /api/v1/rate-cards/{rate_card_id}", s.getRateCard)
	s.route(mux, "GET /api/v1/rate-cards/{rate_card_id}/revisions", s.listRateCardRevisions)
	s.route(mux, "POST /api/v1/rate-cards/{rate_card_id}/revisions", s.createRateCardRevision, access.MaxBody(1<<20))
	s.route(mux, "GET /api/v1/pricing/sources", s.listPricingSources)
	s.route(mux, "POST /api/v1/pricing/sources", s.createPricingSource)
	s.route(mux, "GET /api/v1/pricing/sources/{pricing_source_id}", s.getPricingSource)
	s.route(mux, "PATCH /api/v1/pricing/sources/{pricing_source_id}", s.updatePricingSource)
	s.route(mux, "POST /api/v1/pricing/sources/{pricing_source_id}/refresh", s.refreshPricingSource)
	s.route(mux, "GET /api/v1/pricing/sources/{pricing_source_id}/snapshots", s.listPricingSourceSnapshots)
	s.route(mux, "POST /api/v1/pricing/source-snapshots/{pricing_source_snapshot_id}/publish",
		s.publishPricingSourceSnapshot, access.MaxBody(1<<20))
}

func (s *API) route(mux *http.ServeMux, pattern string, handler access.Handler, options ...access.RouteOption) {
	s.Access.Route(mux, pattern, func(r *http.Request, principal access.Principal) (access.Reply, error) {
		reply, err := handler(r, principal)
		return reply, Problem(err)
	}, options...)
}
