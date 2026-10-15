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

// fetchOpenAI reads the OpenAI organization costs report for a day window.
// Query array encoding follows openai-go v3.74.0
// AdminOrganizationUsageCostsParams (adminorganizationusage.go), which uses
// apiquery.ArrayQueryFormatBrackets: group_by[]=…&project_ids[]=….
func fetchOpenAI(ctx context.Context, client *http.Client, endpoint *string, key []byte, scope Scope, start, end time.Time) (map[string]map[string]lineSum, error) {
	if endpoint == nil {
		return nil, failFetch("endpoint", errors.New("the provider has no endpoint"))
	}
	base, err := endpointURL(*endpoint, "/organization/costs")
	if err != nil {
		return nil, err
	}
	// 1d buckets cap the window's bucket count at 31, well under the 180
	// per page, so one page per window covers the whole window in practice;
	// has_more paging still iterates for larger ranges.
	q := url.Values{}
	q.Set("start_time", strconv.FormatInt(start.Unix(), 10))
	q.Set("end_time", strconv.FormatInt(end.Unix(), 10))
	q.Set("bucket_width", "1d")
	q.Set("limit", "180")
	for _, item := range []string{"line_item"} {
		q.Add("group_by[]", item)
	}
	for _, id := range scope.ProjectIDs {
		q.Add("project_ids[]", id)
	}
	days := reportAccumulator{}
	for pages := 0; pages < maxPages; pages++ {
		var doc openAICostsPage
		err := get(ctx, client, base+"?"+q.Encode(), func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+string(key))
		}, &doc)
		if err != nil {
			return nil, err
		}
		for _, bucket := range doc.Data {
			day := dayOf(bucket.StartTime)
			days.ensureDay(day)
			for _, result := range bucket.Results {
				amount, err := decimalValue(result.Amount.Value)
				if err != nil {
					return nil, failFetch("response", err)
				}
				days.add(day, amount, result.Amount.Currency, result.LineItem)
			}
		}
		if !doc.HasMore || doc.NextPage == "" {
			return days, nil
		}
		q.Set("page", doc.NextPage)
	}
	return nil, failFetch("response", errors.New("cost report exceeded 20 pages"))
}

// openAICostsPage is the documented shape of one organization/costs page:
// {object:"list",data:[{start_time,results:[{amount:{value,currency},
// line_item,project_id,…}]}],has_more,next_page}.
type openAICostsPage struct {
	Data []struct {
		StartTime int64 `json:"start_time"`
		Results   []struct {
			Amount struct {
				Value    json.RawMessage `json:"value"`
				Currency string          `json:"currency"`
			} `json:"amount"`
			LineItem  *string `json:"line_item"`
			ProjectID *string `json:"project_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}
