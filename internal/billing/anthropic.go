package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// fetchAnthropic reads the Anthropic organization cost report for a day
// window. Amounts arrive as decimal strings in the currency's lowest unit —
// cents — and convert to the stored amount with an exact decimal shift.
func fetchAnthropic(ctx context.Context, client *http.Client, endpoint *string, key []byte, scope Scope, start, end time.Time) (map[string]map[string]lineSum, error) {
	if endpoint == nil {
		return nil, failFetch("endpoint", errors.New("the provider has no endpoint"))
	}
	base, err := endpointURL(*endpoint, "/v1/organizations/cost_report")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("starting_at", start.Format(time.RFC3339))
	q.Set("ending_at", end.Format(time.RFC3339))
	q.Set("bucket_width", "1d")
	q.Add("group_by[]", "description")
	q.Add("group_by[]", "workspace_id")
	q.Set("limit", strconv.Itoa(31))
	workspaces := map[string]bool{}
	for _, id := range scope.WorkspaceIDs {
		workspaces[id] = true
	}
	days := reportAccumulator{}
	for pages := 0; pages < maxPages; pages++ {
		var doc anthropicCostPage
		err := get(ctx, client, base+"?"+q.Encode(), func(r *http.Request) {
			r.Header.Set("x-api-key", string(key))
			r.Header.Set("anthropic-version", "2023-06-01")
		}, &doc)
		if err != nil {
			return nil, err
		}
		for _, bucket := range doc.Data {
			var day string
			if len(bucket.StartingAt) >= 10 {
				day = bucket.StartingAt[:10]
			} else {
				return nil, failFetch("response", errors.New("bucket has no starting_at day"))
			}
			days.ensureDay(day)
			for _, result := range bucket.Results {
				workspace := "default"
				if result.WorkspaceID != nil && *result.WorkspaceID != "" {
					workspace = *result.WorkspaceID
				}
				if len(workspaces) > 0 && !workspaces[workspace] {
					continue
				}
				cents, err := decimalValue(result.Amount)
				if err != nil {
					return nil, failFetch("response", err)
				}
				days.add(day, cents.Shift(-2), result.Currency, result.Description, result.CostType)
			}
		}
		if !doc.HasMore || doc.NextPage == "" {
			return days, nil
		}
		q.Set("page", doc.NextPage)
	}
	return nil, failFetch("response", errors.New("cost report exceeded 20 pages"))
}

// anthropicCostPage is the documented shape of one cost_report page:
// {data:[{starting_at,ending_at,results:[{amount,currency,cost_type,
// description,workspace_id,model,service_tier,context_window}]}],has_more,
// next_page}.
type anthropicCostPage struct {
	Data []struct {
		StartingAt string `json:"starting_at"`
		Results    []struct {
			Amount      json.RawMessage `json:"amount"`
			Currency    string          `json:"currency"`
			CostType    *string         `json:"cost_type"`
			Description *string         `json:"description"`
			WorkspaceID *string         `json:"workspace_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}
