package anthropicstate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/kernel"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
)

func (s surface) listResources(w http.ResponseWriter, r *http.Request, kind string, family wire.Format) {
	x, authority, done := s.begin(w, r, family)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	limit, before, cursor, err := listCursor(r.URL.Query(), kind)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	ctx := r.Context()
	if kind == resources.KindAnthropicBatch {
		scanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ctx = scanCtx
	}
	backwards := before != ""
	param := "after_id"
	if backwards {
		param = "before_id"
	}
	fetch := func(anchor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
		batch, err := s.kernel.Deps.Resources.ListKindsPage(ctx, []string{kind}, authority.ID, count, anchor, backwards)
		if errors.Is(err, resources.ErrNotFound) {
			return nil, fault.InvalidRequest("invalid_value", "The cursor must identify a resource owned by this key.", &param)
		}
		if err != nil {
			return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The resource list could not be read.")
		}
		return batch, nil
	}
	authorize := func(row *resources.Resource) *fault.Error {
		return s.kernel.AuthorizeListedResource(ctx, authority, row)
	}
	var rows []*resources.Resource
	var continuation string
	var listErr *fault.Error
	for token := cursor; ; token = continuation {
		rows, continuation, listErr = s.kernel.ListedResourcePage(authority.ID, param, limit, token, fetch, authorize)
		if kind != resources.KindAnthropicBatch || len(rows) != 0 || continuation == "" || listErr != nil || ctx.Err() != nil {
			break
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusGatewayTimeout, "gateway_timeout", "The resource list could not be read within the deadline."))
		return
	}
	if listErr != nil {
		s.kernel.Refuse(x, w, listErr)
		return
	}
	if backwards {
		slices.Reverse(rows)
	}
	items, ids := make([]json.RawMessage, 0, len(rows)), make([]string, 0, len(rows))
	for _, row := range rows {
		object, err := s.resourceObject(row, kind, r)
		if err != nil {
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A resource could not be projected."))
			return
		}
		items, ids = append(items, object), append(ids, row.ID)
	}
	s.kernel.WriteStateJSON(w, x, listBody(items, ids, continuation, backwards))
}

func listBody(items []json.RawMessage, ids []string, continuation string, backwards bool) []byte {
	body := map[string]any{"data": items, "has_more": continuation != ""}
	if len(ids) > 0 {
		body["first_id"], body["last_id"] = ids[0], ids[len(ids)-1]
	}
	if continuation != "" {
		if backwards {
			body["first_id"] = continuation
		} else {
			body["last_id"] = continuation
		}
	}
	return kernel.MustJSON(body)
}

func (s surface) resourceObject(res *resources.Resource, kind string, r *http.Request) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(res.Metadata, &fields); err != nil || fields == nil {
		return nil, errors.New("stored Anthropic resource metadata is not an object")
	}
	delete(fields, "upstream_model")
	fields["id"] = kernel.MustJSON(res.ID)
	if kind == resources.KindAnthropicBatch {
		if raw, ok := fields["results_url"]; ok && string(raw) != "null" {
			fields["results_url"] = kernel.MustJSON(s.kernel.GatewayOrigin(r) + "/anthropic/v1/messages/batches/" + url.PathEscape(res.ID) + "/results")
		}
		return json.Marshal(fields)
	}
	if _, ok := fields["type"]; !ok {
		fields["type"] = json.RawMessage(`"file"`)
	}
	return json.Marshal(fields)
}

func listCursor(query url.Values, kind string) (int, string, string, *fault.Error) {
	before, after := query.Get("before_id"), query.Get("after_id")
	if before != "" && after != "" {
		return 0, "", "", fault.InvalidRequest("invalid_request", "before_id and after_id cannot be used together.", nil)
	}
	limit := 20
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			param := "limit"
			return 0, "", "", fault.InvalidRequest("invalid_value", "limit must be an integer between 1 and 1000.", &param)
		}
		limit = parsed
	}
	cursor := before
	if cursor == "" {
		cursor = after
	}
	if cursor != "" && !strings.HasPrefix(cursor, kernel.RetainedListCursorPrefix) && !validCursor(kind, cursor) {
		param := "after_id"
		if before != "" {
			param = "before_id"
		}
		return 0, "", "", fault.InvalidRequest("invalid_value", param+" must be a local resource identifier.", &param)
	}
	return limit, before, cursor, nil
}

func validCursor(kind, value string) bool {
	prefix := kind + "_"
	raw := strings.TrimPrefix(value, prefix)
	if raw == value || len(raw) != 32 {
		return false
	}
	id, err := uuid.Parse(raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:])
	return err == nil && resources.LocalID(kind, id) == value
}
