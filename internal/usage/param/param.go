package param

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

// list is the shape every paged accounting response shares.
type Page struct {
	Items      any     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// textParam treats a blank parameter as absent: a console that clears a
// filter sends an empty value, and filtering on the empty string would return
// an empty page that looks like "no traffic".
func Text(query url.Values, name string) *string {
	value := strings.TrimSpace(query.Get(name))
	if value == "" {
		return nil
	}
	return &value
}

func UUID(query url.Values, name string) (*string, error) {
	raw := Text(query, name)
	if raw == nil {
		return nil, nil
	}
	id, err := access.ParseUUID(*raw)
	if err != nil {
		return nil, access.Fail(400, "invalid_filter", "Use a valid identifier for "+name+".")
	}
	return &id, nil
}

func RequiredTime(query url.Values, name string) (time.Time, error) {
	value, err := Time(query, name)
	if err != nil {
		return time.Time{}, err
	}
	if value == nil {
		return time.Time{}, access.Fail(400, "invalid_range",
			"Give an RFC 3339 "+name+" for the reporting range.")
	}
	return *value, nil
}

func Time(query url.Values, name string) (*time.Time, error) {
	raw := Text(query, name)
	if raw == nil {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return nil, access.Fail(400, "invalid_range", "Use an RFC 3339 timestamp for "+name+".")
	}
	utc := value.UTC()
	return &utc, nil
}

// statusParam bounds the status filter to the codes a request can actually
// carry, so a typo returns an error instead of an empty page.
func Status(query url.Values) (*int, error) {
	raw := Text(query, "status_code")
	if raw == nil {
		return nil, nil
	}
	status, err := strconv.Atoi(*raw)
	if err != nil || status < 100 || status > 599 {
		return nil, access.Fail(400, "invalid_filter", "Use an HTTP status code from 100 to 599.")
	}
	return &status, nil
}

func Limit(query url.Values) (int, error) {
	raw := Text(query, "limit")
	if raw == nil {
		return 50, nil
	}
	limit, err := strconv.Atoi(*raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, access.Fail(400, "invalid_limit", "Use a page size from 1 to 200.")
	}
	return limit, nil
}

func Cursor(query url.Values) (*usage.Cursor, error) {
	raw := Text(query, "cursor")
	if raw == nil {
		return nil, nil
	}
	at, id, err := usage.DecodeCursor(*raw)
	if err != nil {
		return nil, access.Fail(400, "invalid_cursor", "The cursor is invalid.")
	}
	return &usage.Cursor{At: at, ID: id}, nil
}
