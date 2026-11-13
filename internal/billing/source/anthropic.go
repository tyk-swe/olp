package source

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
)

var anthropicWorkspace = regexp.MustCompile(`^(?:wrkspc_[A-Za-z0-9]{1,64}|default)$`)

type anthropicScope struct {
	WorkspaceIDs []string `json:"workspace_ids,omitempty"`
}

var anthropic = vendor[anthropicScope, apiKey, Polling]{
	family:       "api_key",
	providerKind: connectors.KindAnthropic,
	scope: func(raw json.RawMessage) (anthropicScope, error) {
		scope, err := parseIDScope[anthropicScope](raw)
		if err != nil {
			return scope, err
		}
		return scope, validIDs("workspace_ids", scope.WorkspaceIDs, anthropicWorkspace, "workspace_ids must look like wrkspc_ followed by 1 to 64 letters or digits, or be default")
	},
	input:      jsonString,
	credential: parseAPIKey,
	request:    Inputs.Polling,
	read: func(ctx context.Context, req Polling, scope anthropicScope, key apiKey, emit func(Period) error) (*Resume, error) {
		return readPolling(ctx, req, emit, func(ctx context.Context, client *http.Client, endpoint string, from, to time.Time) (Days, error) {
			return readAnthropicCosts(ctx, client, endpoint, scope, key, from, to)
		})
	},
}

type anthropicCostPage struct {
	pagination
	Data []struct {
		StartingAt string `json:"starting_at"`
		Results    []struct {
			Amount      json.RawMessage `json:"amount"`
			Currency    string          `json:"currency"`
			CostType    string          `json:"cost_type"`
			Description string          `json:"description"`
			WorkspaceID string          `json:"workspace_id"`
		} `json:"results"`
	} `json:"data"`
}

func readAnthropicCosts(ctx context.Context, client *http.Client, endpoint string, scope anthropicScope, key apiKey, from, to time.Time) (Days, error) {
	base, err := endpointURL(endpoint, "/v1/organizations/cost_report")
	if err != nil {
		return Days{}, err
	}
	days := newDays()
	report := pages[anthropicCostPage]{
		client: client,
		base:   base,
		header: http.Header{"X-Api-Key": {string(key)}, "Anthropic-Version": {"2023-06-01"}},
		query: func(page string) url.Values {
			query := url.Values{
				"starting_at":  {from.Format(time.RFC3339)},
				"ending_at":    {to.Format(time.RFC3339)},
				"bucket_width": {"1d"},
				"group_by[]":   {"description", "workspace_id"},
				"limit":        {strconv.Itoa(pollingBucketLimit)},
			}
			if page != "" {
				query.Set("page", page)
			}
			return query
		},
		collect: func(page anthropicCostPage) error {
			for _, bucket := range page.Data {
				day, err := time.Parse(time.DateOnly, bucket.StartingAt[:min(len(bucket.StartingAt), len(time.DateOnly))])
				if err != nil {
					return fail(CodeResponse, errors.New("bucket has no starting_at day"))
				}
				days.include(day)
				for _, result := range bucket.Results {
					if len(scope.WorkspaceIDs) > 0 && !slices.Contains(scope.WorkspaceIDs, cmp.Or(result.WorkspaceID, "default")) {
						continue
					}
					cents, err := decimalValue(result.Amount)
					if err != nil {
						return fail(CodeResponse, err)
					}
					if err := days.add(day, cmp.Or(result.Description, result.CostType), result.Currency, cents.Shift(-2)); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	return days, report.read(ctx, "", 0)
}
