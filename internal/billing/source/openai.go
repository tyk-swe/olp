package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
)

var openAIProject = regexp.MustCompile(`^proj_[A-Za-z0-9]{1,64}$`)

type openAIScope struct {
	ProjectIDs []string `json:"project_ids,omitempty"`
}

var openAI = vendor[openAIScope, apiKey, Polling]{
	family:       "api_key",
	providerKind: connectors.KindOpenAI,
	scope: func(raw json.RawMessage) (openAIScope, error) {
		scope, err := parseIDScope[openAIScope](raw)
		if err != nil {
			return scope, err
		}
		return scope, validIDs("project_ids", scope.ProjectIDs, openAIProject, "project_ids must look like proj_ followed by 1 to 64 letters or digits")
	},
	input:      jsonString,
	credential: parseAPIKey,
	request:    Inputs.Polling,
	read: func(ctx context.Context, req Polling, scope openAIScope, key apiKey, emit func(Period) error) (*Resume, error) {
		return readPolling(ctx, req, emit, func(ctx context.Context, client *http.Client, endpoint string, from, to time.Time) (Days, error) {
			return readOpenAICosts(ctx, client, endpoint, scope, key, from, to)
		})
	},
}

type openAICostsPage struct {
	pagination
	Data []struct {
		StartTime int64 `json:"start_time"`
		Results   []struct {
			Amount struct {
				Value    json.RawMessage `json:"value"`
				Currency string          `json:"currency"`
			} `json:"amount"`
			LineItem string `json:"line_item"`
		} `json:"results"`
	} `json:"data"`
}

func readOpenAICosts(ctx context.Context, client *http.Client, endpoint string, scope openAIScope, key apiKey, from, to time.Time) (Days, error) {
	base, err := endpointURL(endpoint, "/organization/costs")
	if err != nil {
		return Days{}, err
	}
	days := newDays()
	report := pages[openAICostsPage]{
		client: client,
		base:   base,
		header: http.Header{"Authorization": {"Bearer " + string(key)}},
		query: func(page string) url.Values {
			query := url.Values{
				"start_time":    {strconv.FormatInt(from.Unix(), 10)},
				"end_time":      {strconv.FormatInt(to.Unix(), 10)},
				"bucket_width":  {"1d"},
				"limit":         {strconv.Itoa(pollingBucketLimit)},
				"group_by[]":    {"line_item"},
				"project_ids[]": scope.ProjectIDs,
			}
			if page != "" {
				query.Set("page", page)
			}
			return query
		},
		collect: func(page openAICostsPage) error {
			for _, bucket := range page.Data {
				day := time.Unix(bucket.StartTime, 0)
				days.include(day)
				for _, result := range bucket.Results {
					amount, err := decimalValue(result.Amount.Value)
					if err != nil {
						return fail(CodeResponse, err)
					}
					if err := days.add(day, result.LineItem, result.Amount.Currency, amount); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	return days, report.read(ctx, "", 0)
}
