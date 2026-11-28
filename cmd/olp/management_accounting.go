package main

import (
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/billing/imports"
	"github.com/tyk-swe/olp/internal/billing/reconciliation"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/export/focus"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage/completeness"
	"github.com/tyk-swe/olp/internal/usage/pricebook"
	"github.com/tyk-swe/olp/internal/usage/reports"
)

func (m Management) registerAccounting(mux *http.ServeMux) {
	sinks.NewAPI(m.Access, m.core.policy).Register(mux)
	// Usage, pricing, request history and recovery reporting are part
	// of the management surface; their patterns are more specific than
	// its catch-all, which answers everything no surface claims.
	(&reports.API{Access: m.Access}).Register(mux)
	(&pricebook.API{Access: m.Access, Normalizer: pricebook.NewNormalizer(vendors.Kind), Egress: m.core.policy, Catalog: m.documents.catalog, CatalogKeys: m.documents.catalogKeys}).Register(mux)
	(&completeness.API{Access: m.Access}).Register(mux)
	destination.NewAPI(m.Access, m.core.policy).Register(mux)
	focus.NewAPI(m.Access).Register(mux)
	capture.NewAPI(m.Access).Register(mux)
	imports.NewAPI(m.Access).Register(mux)
	reconciliation.NewAPI(m.Access, time.Now).Register(mux)
}
