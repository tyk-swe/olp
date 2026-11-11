package reconciliation

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage/pricebook"
)

const maxRangeDays = 93

type API struct {
	server *access.Server
	now    func() time.Time
}

func NewAPI(server *access.Server, now func() time.Time) *API {
	return &API{server: server, now: now}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/usage/reconciliation", a.report)
}

type Range struct {
	Start time.Time
	End   time.Time
}

func ParseRange(query url.Values) (Range, error) {
	startText, endText := query.Get("start"), query.Get("end")
	if startText == "" || endText == "" {
		return Range{}, access.Fail(http.StatusBadRequest, "invalid_range", "start and end query parameters are required, both YYYY-MM-DD")
	}
	start, err := time.Parse(time.DateOnly, startText)
	if err != nil {
		return Range{}, access.Invalid("start", "start must be a YYYY-MM-DD date")
	}
	end, err := time.Parse(time.DateOnly, endText)
	if err != nil {
		return Range{}, access.Invalid("end", "end must be a YYYY-MM-DD date")
	}
	if !end.After(start) || end.Sub(start) > maxRangeDays*24*time.Hour {
		return Range{}, access.Fail(http.StatusBadRequest, "invalid_range",
			fmt.Sprintf("Use a positive range no longer than %d days, with end exclusive.", maxRangeDays))
	}
	return Range{Start: start, End: end}, nil
}

func providerFilter(raw string) (uuid.NullUUID, error) {
	if raw == "" {
		return uuid.NullUUID{}, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.NullUUID{}, access.Fail(http.StatusBadRequest, "invalid_identifier", "Use a valid UUID.")
	}
	return uuid.NullUUID{UUID: id, Valid: true}, nil
}

func (a *API) report(r *http.Request, p access.Principal) (access.Reply, error) {
	query := r.URL.Query()
	window, err := ParseRange(query)
	if err != nil {
		return access.Reply{}, err
	}
	provider, err := providerFilter(query.Get("provider_id"))
	if err != nil {
		return access.Reply{}, err
	}
	ctx, pool := r.Context(), a.server.Pool
	imports, err := visibleImports(ctx, pool, p, provider)
	if err != nil {
		return access.Reply{}, err
	}
	if provider.Valid && len(imports) == 0 {
		return access.Reply{}, access.Fail(http.StatusNotFound, "not_found", "No billing import covers this provider.")
	}
	providers := importedProviders(imports)
	coverage, err := coveredDays(ctx, pool, providers, window)
	if err != nil {
		return access.Reply{}, err
	}
	invoiced, err := invoicedDays(ctx, pool, providers, window)
	if err != nil {
		return access.Reply{}, err
	}
	spend, err := attributedSpend(ctx, pool, providers, window)
	if err != nil {
		return access.Reply{}, err
	}
	currency, err := pricebook.InstallationCurrency(ctx, pool)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusOK, Body: build(inputs{
		window:   window,
		imports:  imports,
		coverage: coverage,
		invoiced: invoiced,
		spend:    spend,
		currency: currency,
		today:    a.now().UTC().Truncate(24 * time.Hour),
	})}, nil
}
