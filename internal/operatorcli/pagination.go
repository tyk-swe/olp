package operatorcli

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/management"
)

const (
	maxListPages = 100
	maxListBytes = 16 << 20
)

func supportsAllPages(op management.Operation) bool {
	return op.Method == http.MethodGet && (op.Name == "list_providers" || op.Name == "list_api_keys")
}

func allPagesOption(op management.Operation, opts options) (bool, error) {
	if _, present := opts["all-pages"]; !present {
		return false, nil
	}
	if !supportsAllPages(op) {
		return false, errors.New("--all-pages is only supported for list_providers and list_api_keys")
	}
	switch opts.one("all-pages") {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("--all-pages requires true or false")
	}
}

type listPage struct {
	Items      []json.RawMessage `json:"items"`
	NextCursor *string           `json:"next_cursor,omitempty"`
}

type pageWalk struct {
	client *management.Client
	op     management.Operation
	input  management.Arguments
}

func collectPages(ctx context.Context, client *management.Client, op management.Operation, input management.Arguments) (management.Response, *commandError) {
	start, _ := input.Query["cursor"].(string)
	items, failure := pageWalk{client: client, op: op, input: input}.collect(ctx, input, []string{start}, nil, 0)
	if failure != nil {
		return management.Response{}, failure
	}
	body, err := json.Marshal(listPage{Items: items})
	if err != nil {
		return management.Response{}, commandFailure(management.InvalidResponse, err)
	}
	return management.Response{Body: body}, nil
}

func (w pageWalk) collect(ctx context.Context, args management.Arguments, seen []string, items []json.RawMessage, size int) ([]json.RawMessage, *commandError) {
	if len(seen) > maxListPages {
		return nil, commandFailure(management.InvalidResponse, errors.New("management pagination exceeds 100 pages"))
	}
	result, failure := w.client.CallClassified(ctx, w.op, args)
	if failure != nil {
		return nil, callFailure(failure)
	}
	total := size + len(result.Body)
	if total > maxListBytes {
		return nil, commandFailure(management.InvalidResponse, errors.New("management pagination exceeds 16 MiB"))
	}
	page, err := decodeListPage(result.Body)
	if err != nil {
		return nil, commandFailure(management.InvalidResponse, err)
	}
	all := slices.Concat(items, page.Items)
	if page.NextCursor == nil {
		return all, nil
	}
	cursor := *page.NextCursor
	if slices.Contains(seen, cursor) {
		return nil, commandFailure(management.InvalidResponse, errors.New("management pagination cursor repeated"))
	}
	return w.collect(ctx, w.at(cursor), slices.Concat(seen, []string{cursor}), all, total)
}

func (w pageWalk) at(cursor string) management.Arguments {
	query := maps.Collect(func(yield func(string, any) bool) {
		for key, value := range w.input.Query {
			if key != "cursor" && !yield(key, value) {
				return
			}
		}
		yield("cursor", cursor)
	})
	return management.Arguments{Path: w.input.Path, Query: query, Body: w.input.Body, IfMatch: w.input.IfMatch, IdempotencyKey: w.input.IdempotencyKey}
}

func decodeListPage(body json.RawMessage) (listPage, error) {
	var page listPage
	if err := json.Unmarshal(body, &page); err != nil || page.Items == nil {
		return listPage{}, errors.New("management list response must contain an items array and an optional cursor")
	}
	for _, item := range page.Items {
		if item[0] != '{' {
			return listPage{}, errors.New("management list items must be objects")
		}
	}
	if page.NextCursor != nil && *page.NextCursor == "" {
		return listPage{}, errors.New("management pagination cursor must not be empty")
	}
	return page, nil
}
