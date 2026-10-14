package usage

import (
	"context"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

// ProviderDaySpend is the operator spend one provider accrued on one UTC day:
// estimated cost plus the day's share of commitment amounts, the exact
// formula the reconciliation report compares with the provider's invoice.
type ProviderDaySpend struct {
	ProviderID             string `json:"provider_id"`
	Day                    string `json:"day"`
	AttributedCost         string `json:"attributed_cost"`
	PricedAttempts         int64  `json:"priced_attempts"`
	UnpricedAttempts       int64  `json:"unpriced_attempts"`
	IncompleteAttempts     int64  `json:"incomplete_attempts"`
	ExcludedCallerAttempts int64  `json:"excluded_caller_attempts"`
}

// ReconciliationSpend groups attributed spend per provider and UTC day for
// the reconciliation report. It shares the report_rows machinery so
// commitment allocation matches every other report.
//
// Facts rows are excluded when the caller paid (caller_credential) or a
// budget exemption covers them. Rolled-up hourly rows carry no caller flag,
// so caller-paid usage surviving only as an hourly aggregate still
// contributes; the report's excluded_caller_attempts then undercounts the
// same way. Hourly rows also keep only request-grain priced and incomplete
// counts, which the response documents as an approximation.
func ReconciliationSpend(ctx context.Context, q access.Queryer, start, end time.Time, providers []string) ([]ProviderDaySpend, error) {
	if len(providers) == 0 {
		return nil, nil
	}
	// The report is provider-scoped, not consumer-project scoped, so the
	// project filters stay open and provider selection happens in the outer
	// query.
	f := Filters{Start: start, End: end, AllProjects: true}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	var query filterQuery
	f.usageRows(&query, scopeProvider)
	query.push(" SELECT provider_id::text, to_char(observed_at AT TIME ZONE 'UTC','YYYY-MM-DD')," +
		" (COALESCE(SUM(estimated_cost) FILTER (WHERE NOT budget_exempt AND NOT caller_credential),0) +" +
		"  COALESCE(SUM(allocated_commitment) FILTER (WHERE NOT budget_exempt AND NOT caller_credential),0))::text," +
		" COALESCE(SUM(reconcil_priced_attempts) FILTER (WHERE NOT budget_exempt AND NOT caller_credential),0)::bigint," +
		" COALESCE(SUM(reconcil_unpriced_attempts) FILTER (WHERE NOT budget_exempt AND NOT caller_credential),0)::bigint," +
		" COALESCE(SUM(reconcil_incomplete_attempts) FILTER (WHERE NOT budget_exempt AND NOT caller_credential),0)::bigint," +
		" COALESCE(SUM(reconcil_caller_attempts),0)::bigint" +
		" FROM report_rows WHERE provider_id = ANY(" + query.bind(providers) + "::uuid[])" +
		" GROUP BY provider_id, to_char(observed_at AT TIME ZONE 'UTC','YYYY-MM-DD')" +
		" ORDER BY provider_id, 2")
	rows, err := q.Query(ctx, query.sql(), query.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []ProviderDaySpend
	for rows.Next() {
		var item ProviderDaySpend
		if err := rows.Scan(&item.ProviderID, &item.Day, &item.AttributedCost,
			&item.PricedAttempts, &item.UnpricedAttempts, &item.IncompleteAttempts,
			&item.ExcludedCallerAttempts); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
