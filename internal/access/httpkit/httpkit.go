package httpkit

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access/authority"
)

type Problem struct {
	Status              int
	Code, Detail, Field string
}

func (p *Problem) Error() string { return p.Code }
func Fail(status int, code, detail string) error {
	return &Problem{Status: status, Code: code, Detail: detail}
}
func Invalid(field, detail string) error {
	return &Problem{Status: 422, Code: "validation_failed", Detail: detail, Field: field}
}
func Forbidden() error {
	return Fail(403, "permission_denied", "The current role cannot perform this operation.")
}

var NotFound = Fail(404, "not_found", "The resource was not found.")

func NotFoundOr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return NotFound
	}
	return err
}

func ProblemFor(err error) (*Problem, bool) {
	if known, ok := errors.AsType[*Problem](err); ok {
		return known, true
	}
	if validation, ok := errors.AsType[*authority.ValidationError](err); ok {
		return &Problem{Status: 422, Code: "validation_failed", Detail: validation.Detail, Field: validation.Field}, true
	}
	if contract := ContractProblem(err); contract != nil {
		return contract, true
	}
	if refusal, ok := errors.AsType[interface {
		error
		RoutingRefusal() (string, string)
	}](err); ok {
		code, message := refusal.RoutingRefusal()
		return &Problem{Status: 422, Code: code, Detail: message}, true
	}
	if validation, ok := errors.AsType[interface {
		error
		RoutingValidation() (string, string)
	}](err); ok {
		field, message := validation.RoutingValidation()
		return &Problem{Status: 422, Code: "validation_failed", Detail: message, Field: field}, true
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return &Problem{Status: 404, Code: "not_found", Detail: "The resource was not found."}, true
	}
	if pg, ok := errors.AsType[*pgconn.PgError](err); ok && pg.Code == "23505" {
		return &Problem{Status: 409, Code: "already_exists", Detail: "This record already exists."}, true
	}
	return &Problem{Status: 503, Code: "service_unavailable", Detail: "The operation could not be completed. Retry shortly."}, false
}

func WriteProblem(w http.ResponseWriter, err error) {
	p, _ := ProblemFor(err)
	RenderProblem(w, p)
}

func RenderProblem(w http.ResponseWriter, p *Problem) {
	body := map[string]any{"type": "https://openllmproxy.dev/problems/" + p.Code, "title": http.StatusText(p.Status), "status": p.Status, "detail": p.Detail}
	if p.Field != "" {
		body["errors"] = map[string]any{p.Field: []any{map[string]string{"code": p.Code, "message": p.Detail}}}
	}
	w.Header().Set("Content-Type", "application/problem+json")
	if p.Status == 429 {
		w.Header().Set("Retry-After", "60")
	}
	w.WriteHeader(p.Status)
	json.NewEncoder(w).Encode(body)
}

type Reply struct {
	Status              int             `json:"status"`
	Body                any             `json:"body"`
	ETag                string          `json:"etag,omitempty"`
	Location            string          `json:"location,omitempty"`
	Cookies             []*http.Cookie  `json:"-"`
	CSRF                string          `json:"-"`
	DisableHTMLEscaping bool            `json:"-"`
	ParentMutation      *ParentMutation `json:"parent_mutation,omitempty"`
}

// ParentMutation records the atomic parent transition caused by a child edit.
// Clients can account for their own writes without adopting an unrelated edit.
type ParentMutation struct {
	Previous string `json:"previous"`
	Current  string `json:"current"`
}

func OK(body any) Reply                  { return Reply{Status: 200, Body: body} }
func Detail(body any, etag string) Reply { return Reply{Status: 200, Body: body, ETag: etag} }

func requireJSONContentType(r *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return Fail(415, "unsupported_media_type", "Send an application/json request.")
	}
	return nil
}

func Decode(r *http.Request, v any) error {
	if err := requireJSONContentType(r); err != nil {
		return err
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return Fail(400, "invalid_json", "The request body is not valid for this operation.")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return Fail(400, "invalid_json", "Send one JSON document.")
	}
	return nil
}

func Match(r *http.Request, etag string) error {
	value := r.Header.Get("If-Match")
	if value == "" {
		return Fail(428, "precondition_required", "Reload the record and send its ETag in If-Match.")
	}
	if value != `"`+etag+`"` {
		return Fail(412, "etag_mismatch", "This record changed. Reload it before saving.")
	}
	return nil
}

func IDParam(r *http.Request, name string) (string, error) {
	v := r.PathValue(name)
	id, err := uuid.Parse(v)
	if err != nil {
		return "", Fail(400, "invalid_identifier", "Use a valid UUID.")
	}
	return id.String(), nil
}

func ValidText(field, value string, max int) error {
	if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > max {
		return Invalid(field, "Use 1–"+strconv.Itoa(max)+" characters.")
	}
	return nil
}

func NewID() string { return uuid.Must(uuid.NewV7()).String() }

func ParseUUID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return "", Invalid("id", "Use a valid UUID.")
	}
	return id.String(), nil
}

func ParseUUIDs(values []string) ([]string, error) {
	type parsedUUID struct {
		value string
		err   error
	}
	parsed := slices.Collect(func(yield func(parsedUUID) bool) {
		for _, raw := range values {
			value, err := ParseUUID(raw)
			if !yield(parsedUUID{value, err}) {
				return
			}
		}
	})
	for _, value := range parsed {
		if value.err != nil {
			return nil, value.err
		}
	}
	if values == nil {
		return nil, nil
	}
	return append([]string{}, slices.Collect(func(yield func(string) bool) {
		for _, value := range parsed {
			if !yield(value.value) {
				return
			}
		}
	})...), nil
}

func ParseOptionalUUID(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	canonical, err := ParseUUID(*value)
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

type Pagination struct {
	Limit  int
	Before string
}

func Page(r *http.Request) (Pagination, error) {
	return PageBy(r, "ffffffff-ffff-ffff-ffff-ffffffffffff", func(raw string) (string, error) {
		v, err := uuid.Parse(raw)
		return v.String(), err
	})
}

func PageBy(r *http.Request, first string, cursor func(string) (string, error)) (Pagination, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			return Pagination{}, Invalid("limit", "Use a page size from 1 to 200.")
		}
		limit = n
	}
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return Pagination{Limit: limit, Before: first}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Pagination{}, Invalid("cursor", "Invalid pagination cursor.")
	}
	before, err := cursor(string(b))
	if err != nil {
		return Pagination{}, Invalid("cursor", "Invalid pagination cursor.")
	}
	return Pagination{Limit: limit, Before: before}, nil
}

func ListReply(items []map[string]any, p Pagination) Reply {
	return ListReplyBy(items, p, func(item map[string]any) string { return item["id"].(string) })
}

// ListReplyBy paginates records using the identifier ordered by their query.
func ListReplyBy[T any](items []T, p Pagination, id func(T) string) Reply {
	return OK(ListPage(items, p, id))
}

type ListBody[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func ListPage[T any](items []T, p Pagination, id func(T) string) ListBody[T] {
	switch {
	case len(items) > p.Limit:
		cursor := base64.RawURLEncoding.EncodeToString([]byte(id(items[p.Limit-1])))
		return ListBody[T]{Items: items[:p.Limit], NextCursor: &cursor}
	case items == nil:
		return ListBody[T]{Items: []T{}}
	}
	return ListBody[T]{Items: items}
}

func JSONRows(rows pgx.Rows) ([]map[string]any, error) {
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var b []byte
		var item map[string]any
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(b))
		decoder.UseNumber()
		if err := decoder.Decode(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
