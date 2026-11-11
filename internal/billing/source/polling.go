package source

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

const (
	pollingBucketLimit = 31
	maxPages           = 20
)

type apiKey string

func parseAPIKey(key string) (apiKey, error) {
	if n := len(key); n < 20 || n > 512 {
		return "", access.Invalid("credential", "credential must be 20 to 512 bytes")
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return "", access.Invalid("credential", "credential must be printable ASCII without whitespace")
		}
	}
	return apiKey(key), nil
}

func parseIDScope[S any](raw json.RawMessage) (S, error) {
	var scope S
	if len(raw) == 0 || string(raw) == "null" {
		return scope, nil
	}
	if err := decodeStrict(raw, &scope); errors.Is(err, errTrailingData) {
		return scope, access.Invalid("scope", "scope must be a single JSON document")
	} else if err != nil {
		return scope, access.Invalid("scope", "scope must be a JSON object with only the source's keys")
	}
	return scope, nil
}

func validIDs(field string, ids []string, pattern *regexp.Regexp, message string) error {
	if len(ids) > 100 {
		return access.Invalid("scope."+field, "at most 100 "+field)
	}
	if slices.ContainsFunc(ids, func(id string) bool { return !pattern.MatchString(id) }) {
		return access.Invalid("scope."+field, message)
	}
	return nil
}

func window(req Polling) (time.Time, time.Time) {
	if req.ImportedThrough == nil {
		return req.Today.AddDate(0, 0, -30), req.Today
	}
	return slices.MinFunc([]time.Time{*req.ImportedThrough, req.Today.AddDate(0, 0, -7)}, time.Time.Compare), req.Today
}

type costReport func(ctx context.Context, client *http.Client, endpoint string, from, to time.Time) (Days, error)

func readPolling(ctx context.Context, req Polling, emit func(Period) error, report costReport) (*Resume, error) {
	client, err := req.Client(req.Endpoint)
	if err != nil {
		return nil, fail(CodeEndpoint, err)
	}
	start, end := window(req)
	for from := start; from.Before(end); from = from.AddDate(0, 0, pollingBucketLimit) {
		days, err := report(ctx, client, req.Endpoint, from, slices.MinFunc([]time.Time{from.AddDate(0, 0, pollingBucketLimit), end}, time.Time.Compare))
		if err != nil {
			return nil, err
		}
		if err := emit(Period{Days: days}); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

type pagination struct {
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

func (p pagination) cursor() pagination { return p }

type pages[P interface{ cursor() pagination }] struct {
	client  *http.Client
	base    string
	header  http.Header
	query   func(page string) url.Values
	collect func(P) error
}

func (p pages[P]) read(ctx context.Context, page string, count int) error {
	if count == maxPages {
		return fail(CodeResponse, errors.New("cost report exceeded 20 pages"))
	}
	var doc P
	if err := getJSON(ctx, p.client, p.base+"?"+p.query(page).Encode(), p.header, &doc); err != nil {
		return err
	}
	if err := p.collect(doc); err != nil {
		return err
	}
	next := doc.cursor()
	if !next.HasMore {
		return nil
	}
	if next.NextPage == "" {
		return fail(CodeResponse, errors.New("cost report has no continuation cursor"))
	}
	return p.read(ctx, next.NextPage, count+1)
}
