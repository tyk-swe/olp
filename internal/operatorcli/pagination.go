package operatorcli

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"

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

func collectPages(ctx context.Context, client *management.Client, op management.Operation, input management.Arguments) (management.Response, *commandError) {
	pageInput := input
	pageInput.Query = maps.Clone(input.Query)
	start, _ := input.Query["cursor"].(string)
	seen := map[string]struct{}{start: {}}
	items := []json.RawMessage{}
	totalBytes := 0
	for range maxListPages {
		result, failure := client.CallClassified(ctx, op, pageInput)
		if failure != nil {
			return management.Response{}, callFailure(failure)
		}
		totalBytes += len(result.Body)
		if totalBytes > maxListBytes {
			return management.Response{}, commandFailure(management.InvalidResponse, errors.New("management pagination exceeds 16 MiB"))
		}
		page, err := decodeListPage(result.Body)
		if err != nil {
			return management.Response{}, commandFailure(management.InvalidResponse, err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == nil {
			body, err := json.Marshal(listPage{Items: items})
			if err != nil {
				return management.Response{}, commandFailure(management.InvalidResponse, err)
			}
			return management.Response{Body: body}, nil
		}
		cursor := *page.NextCursor
		if _, repeated := seen[cursor]; repeated {
			return management.Response{}, commandFailure(management.InvalidResponse, errors.New("management pagination cursor repeated"))
		}
		seen[cursor] = struct{}{}
		pageInput.Query["cursor"] = cursor
	}
	return management.Response{}, commandFailure(management.InvalidResponse, errors.New("management pagination exceeds 100 pages"))
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
