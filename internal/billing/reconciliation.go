package billing

import (
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

const dayFormat = "2006-01-02"

// MaxRangeDays bounds an invoiced-cost or reconciliation window: invoice
// import windows are quarterly, so a longer range is refused rather than
// silently truncated.
const MaxRangeDays = 93

// dayRange reads the mandatory start/end=YYYY-MM-DD pair, end exclusive.
func dayRange(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	startText, endText := q.Get("start"), q.Get("end")
	if startText == "" || endText == "" {
		return time.Time{}, time.Time{}, access.Fail(400, "invalid_range", "start and end query parameters are required, both YYYY-MM-DD")
	}
	start, err := time.Parse(dayFormat, startText)
	if err != nil {
		return time.Time{}, time.Time{}, access.Invalid("start", "start must be a YYYY-MM-DD date")
	}
	end, err := time.Parse(dayFormat, endText)
	if err != nil {
		return time.Time{}, time.Time{}, access.Invalid("end", "end must be a YYYY-MM-DD date")
	}
	if !end.After(start) || end.Sub(start) > MaxRangeDays*24*time.Hour {
		return time.Time{}, time.Time{}, access.Fail(400, "invalid_range",
			"Use a positive range no longer than 93 days, with end exclusive.")
	}
	return start.UTC(), end.UTC(), nil
}

// visibleImports lists the billing imports a principal may see: a
// project-scoped principal sees only imports on providers in its projects.
func (s *Server) visibleImports(r *http.Request, p access.Principal, providerID *string) ([]importRow, error) {
	// importColumns are unqualified; the join with olp.providers needs each
	// column bound to the import alias (etag exists on both tables).
	query := `SELECT ` + qualifiedImportColumns("bi") + ` FROM olp.provider_billing_imports bi
		JOIN olp.providers pr ON pr.id=bi.provider_id
		WHERE ($1::text IS NULL OR bi.provider_id=$1::uuid)
		  AND ($2 OR pr.project_id::text=ANY($3::text[]))`
	rows, err := s.Access.Pool.Query(r.Context(), query+" ORDER BY bi.provider_id",
		providerID, p.AllProjects, p.ProjectIDs())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []importRow
	for rows.Next() {
		row, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *row)
	}
	return items, rows.Err()
}

// reconciliation answers how much of each provider's invoice the accounting
// recorded: invoiced line items versus attributed spend, per UTC day.
func (s *Server) reconciliation(r *http.Request, p access.Principal) (access.Reply, error) {
	start, end, err := dayRange(r)
	if err != nil {
		return access.Reply{}, err
	}
	var providerID *string
	if raw := r.URL.Query().Get("provider_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return access.Reply{}, access.Fail(400, "invalid_identifier", "Use a valid UUID.")
		}
		parsed := id.String()
		providerID = &parsed
	}
	imports, err := s.visibleImports(r, p, providerID)
	if err != nil {
		return access.Reply{}, err
	}
	if providerID != nil && len(imports) == 0 {
		// Either the import does not exist or the provider sits outside the
		// principal's scope; both answer like a missing resource.
		return access.Reply{}, access.Fail(404, "not_found", "No billing import covers this provider.")
	}
	providerIDs := make([]string, 0, len(imports))
	byProvider := map[string]*importRow{}
	for i := range imports {
		providerIDs = append(providerIDs, imports[i].ProviderID)
		byProvider[imports[i].ProviderID] = &imports[i]
	}
	// Invoiced totals per provider and day over the whole window.
	invoiced := map[string]map[string]*invoiceDay{}
	if len(providerIDs) > 0 {
		rows, err := s.Access.Pool.Query(r.Context(), `SELECT provider_id::text,day::text,sum(amount)::text,
				string_agg(DISTINCT btrim(currency),',' ORDER BY btrim(currency))
			FROM olp.provider_invoiced_costs
			WHERE provider_id=ANY($1::uuid[]) AND day>=$2::date AND day<$3::date
			GROUP BY provider_id,day`, providerIDs, start.Format(dayFormat), end.Format(dayFormat))
		if err != nil {
			return access.Reply{}, err
		}
		for rows.Next() {
			var provider, day, amount, currencies string
			if err := rows.Scan(&provider, &day, &amount, &currencies); err != nil {
				rows.Close()
				return access.Reply{}, err
			}
			if invoiced[provider] == nil {
				invoiced[provider] = map[string]*invoiceDay{}
			}
			amountDec, err := decimal.NewFromString(amount)
			if err != nil {
				rows.Close()
				return access.Reply{}, err
			}
			invoiced[provider][day] = &invoiceDay{amount: amountDec, currencies: currencies}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return access.Reply{}, err
		}
	}
	spend, err := usage.ReconciliationSpend(r.Context(), s.Access.Pool, start, end, providerIDs)
	if err != nil {
		return access.Reply{}, err
	}
	// The currency row is set by the first pricing revision; until then the
	// installation prices in USD, the same fallback rate_cards.go uses.
	var pricingCurrency string
	if err := s.Access.Pool.QueryRow(r.Context(),
		`SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton), 'USD')`).Scan(&pricingCurrency); err != nil {
		return access.Reply{}, err
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	twoDaysAgo := today.Add(-48 * time.Hour)
	items := []map[string]any{}
	// A day is an item when accounting or the invoice recorded anything for
	// it; covered days the provider's invoice does not mention at all still
	// report invoiced "0".
	type cell struct {
		spend    *usage.ProviderDaySpend
		invoiced *invoiceDay
	}
	byProviderDay := map[string]map[string]*cell{}
	for i := range spend {
		row := &spend[i]
		if byProviderDay[row.ProviderID] == nil {
			byProviderDay[row.ProviderID] = map[string]*cell{}
		}
		byProviderDay[row.ProviderID][row.Day] = &cell{spend: row}
	}
	for provider, days := range invoiced {
		for day, inv := range days {
			if byProviderDay[provider] == nil {
				byProviderDay[provider] = map[string]*cell{}
			}
			if byProviderDay[provider][day] == nil {
				byProviderDay[provider][day] = &cell{}
			}
			byProviderDay[provider][day].invoiced = inv
		}
	}
	type providerTotal struct {
		attributed, attributedCovered, invoiced decimal.Decimal
		invoicedDays                            int
		priced, unpriced, incomplete            int64
		excludedCaller                          int64
		currencyMismatch, unimported            bool
		zeroInvoice                             bool
	}
	totals := map[string]*providerTotal{}
	providers := make([]string, 0, len(byProviderDay))
	for provider := range byProviderDay {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	for _, provider := range providers {
		days := byProviderDay[provider]
		imp := byProvider[provider]
		daysList := make([]string, 0, len(days))
		for day := range days {
			daysList = append(daysList, day)
		}
		sort.Strings(daysList)
		total := totals[provider]
		if total == nil {
			total = &providerTotal{attributed: decimal.Zero, invoiced: decimal.Zero, attributedCovered: decimal.Zero}
			totals[provider] = total
		}
		for _, day := range daysList {
			c := days[day]
			dayTime, _ := time.Parse(dayFormat, day)
			attributed := decimal.Zero
			var priced, unpriced, incomplete, excluded int64
			if c.spend != nil {
				if v, err := decimal.NewFromString(c.spend.AttributedCost); err == nil {
					attributed = v
				}
				priced, unpriced, incomplete = c.spend.PricedAttempts, c.spend.UnpricedAttempts, c.spend.IncompleteAttempts
				excluded = c.spend.ExcludedCallerAttempts
			}
			var invoicedAmount *decimal.Decimal
			var invoicedCurrency *string
			var covered bool
			// Invoiced cost reads "0" on covered days the import returned
			// without buckets, and null outside the imported window.
			if imp != nil && imp.ImportedFrom != nil && imp.ImportedThrough != nil &&
				day >= *imp.ImportedFrom && day <= *imp.ImportedThrough {
				covered = true
				amount := decimal.Zero
				currencies := ""
				if c.invoiced != nil {
					amount = c.invoiced.amount
					currencies = c.invoiced.currencies
				}
				invoicedAmount = &amount
				if currencies != "" {
					invoicedCurrency = &currencies
				}
			}
			var rate *string
			var reason *string
			switch {
			case !covered:
				r := "not_imported"
				reason = &r
				total.unimported = true
			case invoicedCurrency != nil && *invoicedCurrency != pricingCurrency:
				r := "currency_mismatch"
				reason = &r
				total.currencyMismatch = true
			case invoicedAmount.IsZero():
				r := "zero_invoice"
				reason = &r
				total.zeroInvoice = true
			default:
				v := attributed.Div(*invoicedAmount).Round(6).String()
				rate = &v
			}
			provisional := !dayTime.Before(twoDaysAgo) || imp == nil || imp.LastSuccessAt == nil ||
				imp.LastSuccessAt.Before(dayTime.Add(30*time.Hour))
			var importStatus map[string]any
			if imp != nil {
				importStatus = map[string]any{
					"last_success_at": imp.LastSuccessAt,
					"last_error_code": imp.LastErrorCode,
				}
			}
			items = append(items, map[string]any{
				"provider_id":                     provider,
				"day":                             day,
				"attributed_cost":                 attributed.String(),
				"invoiced_cost":                   nullableDecimal(invoicedAmount),
				"invoiced_currency":               invoicedCurrency,
				"capture_rate":                    rate,
				"capture_rate_unavailable_reason": reason,
				"priced_attempts":                 priced,
				"unpriced_attempts":               unpriced,
				"incomplete_attempts":             incomplete,
				"unpriced_share":                  unpricedShare(priced, unpriced),
				"excluded_caller_attempts":        excluded,
				"provisional":                     provisional,
				"import":                          importStatus,
			})
			total.attributed = total.attributed.Add(attributed)
			total.priced += priced
			total.unpriced += unpriced
			total.incomplete += incomplete
			total.excludedCaller += excluded
			if covered {
				total.invoiced = total.invoiced.Add(*invoicedAmount)
				// The capture rate compares only spend the invoice could
				// have recorded: attributed cost on covered days.
				total.attributedCovered = total.attributedCovered.Add(attributed)
				total.invoicedDays++
			}
		}
	}
	// Providers whose range holds no usage and no invoice rows still answer a
	// totals entry so the caller sees the import was reconciled to zero.
	for provider := range byProvider {
		if totals[provider] == nil {
			totals[provider] = &providerTotal{attributed: decimal.Zero, invoiced: decimal.Zero}
		}
	}
	totalsList := make([]string, 0, len(totals))
	for provider := range totals {
		totalsList = append(totalsList, provider)
	}
	sort.Strings(totalsList)
	totalsItems := make([]map[string]any, 0, len(totalsList))
	for _, provider := range totalsList {
		total := totals[provider]
		var rate *string
		var reason *string
		switch {
		case total.invoicedDays == 0:
			r := "not_imported"
			reason = &r
		case total.currencyMismatch:
			r := "currency_mismatch"
			reason = &r
		case total.invoiced.IsZero():
			r := "zero_invoice"
			reason = &r
		default:
			v := total.attributedCovered.Div(total.invoiced).Round(6).String()
			rate = &v
		}
		var invoiced *string
		if total.invoicedDays > 0 {
			v := total.invoiced.String()
			invoiced = &v
		}
		totalsItems = append(totalsItems, map[string]any{
			"provider_id":                     provider,
			"attributed_cost":                 total.attributed.String(),
			"attributed_cost_covered":         total.attributedCovered.String(),
			"invoiced_cost":                   invoiced,
			"capture_rate":                    rate,
			"capture_rate_unavailable_reason": reason,
			"unpriced_share":                  unpricedShare(total.priced, total.unpriced),
		})
	}
	return access.Reply{Status: http.StatusOK, Body: map[string]any{
		"start":    start.Format(dayFormat),
		"end":      end.Format(dayFormat),
		"currency": pricingCurrency,
		"items":    items,
		"totals":   totalsItems,
	}}, nil
}

// invoiceDay is one provider-day's invoiced total and the currencies the
// line items named; a comma-joined list tells the report when the provider
// billed more than one currency for the same day.
type invoiceDay struct {
	amount     decimal.Decimal
	currencies string
}

func nullableDecimal(v *decimal.Decimal) any {
	if v == nil {
		return nil
	}
	return v.String()
}

// unpriced_share = unpriced/(priced+unpriced), null when nothing was billed.
func unpricedShare(priced, unpriced int64) any {
	if priced+unpriced == 0 {
		return nil
	}
	return decimal.NewFromInt(unpriced).Div(decimal.NewFromInt(priced + unpriced)).Round(6).String()
}
