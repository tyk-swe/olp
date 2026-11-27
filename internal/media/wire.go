package media

import (
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

const (
	DefaultImageUploadLimit    int64 = 50 * 1024 * 1024
	DefaultAudioUploadLimit    int64 = 25 * 1024 * 1024
	DefaultVideoReferenceLimit int64 = 20 * 1024 * 1024
	MaxVideoPromptLength             = 32000
)

var nowFunc = time.Now

func nowUnix() int64 {
	return nowFunc().Unix()
}

type AudioResult struct {
	Audio       []byte
	ContentType string
	Tokens      *ImageUsage
}

func parseRawObject(body []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("JSON value is not an object")
	}
	return fields, nil
}

func decodeField[T any](fields map[string]json.RawMessage, name string, target *T) error {
	raw, ok := fields[name]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, target)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func diffRawFields(doc map[string]json.RawMessage, known ...string) map[string]json.RawMessage {
	var extra map[string]json.RawMessage
	for name, value := range doc {
		if slices.Contains(known, name) {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[name] = value
	}
	return extra
}

func mergeRawFields(base, extra map[string]json.RawMessage) map[string]json.RawMessage {
	merged := map[string]json.RawMessage{}
	maps.Copy(merged, base)
	for name, value := range extra {
		if _, exists := merged[name]; !exists {
			merged[name] = value
		}
	}
	return merged
}

func encodeRequestJSON[T any](payload T, extra map[string]json.RawMessage) ([]byte, *Error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, invalidMedia("The media request could not be encoded.")
	}
	if len(extra) == 0 {
		return raw, nil
	}
	fields, err := parseRawObject(raw)
	if err != nil {
		return nil, invalidMedia("The media request could not be encoded.")
	}
	merged := map[string]json.RawMessage{}
	maps.Copy(merged, fields)
	for name, value := range extra {
		if _, exists := merged[name]; exists {
			return nil, invalidMedia("The extension field " + strconv.Quote(name) + " collides with a request field.")
		}
		merged[name] = value
	}
	body, err := json.Marshal(merged)
	if err != nil {
		return nil, invalidMedia("The media request could not be encoded.")
	}
	return body, nil
}

func marshalWithExtra[T any](payload T, extra map[string]json.RawMessage) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil || len(extra) == 0 {
		return raw, err
	}
	fields, err := parseRawObject(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(mergeRawFields(fields, extra))
}

func parseDimensions(size string) (int, int, bool) {
	widthText, heightText, ok := strings.Cut(size, "x")
	if !ok {
		return 0, 0, false
	}
	width, wErr := strconv.Atoi(widthText)
	height, hErr := strconv.Atoi(heightText)
	if wErr != nil || hErr != nil {
		return 0, 0, false
	}
	return width, height, true
}

func textField(fields []staging.Field, name string, value *string) []staging.Field {
	if value == nil {
		return fields
	}
	return append(fields, staging.Field{Name: name, Value: staging.Text(*value)})
}

func textValue(fields []staging.Field, name, value string) []staging.Field {
	return append(fields, staging.Field{Name: name, Value: staging.Text(value)})
}

func extraFields(fields []staging.Field, extra map[string]string) []staging.Field {
	if len(extra) == 0 {
		return fields
	}
	for _, name := range slices.Sorted(maps.Keys(extra)) {
		fields = append(fields, staging.Field{Name: name, Value: staging.Text(extra[name])})
	}
	return fields
}

type Order int

const (
	OrderDescending Order = iota
	OrderAscending
)

const MaxNativeVideoSourceBytes = 1 << 20

type ListQuery struct {
	After *uuid.UUID
	Order Order
	Limit int
}

func ValidateVideoListQuery(query url.Values) (*ListQuery, *Error) {
	for name, values := range query {
		if name != "after" && name != "limit" && name != "order" || len(values) != 1 {
			return nil, invalidMedia("Video list contains unsupported query parameters.")
		}
	}
	out := &ListQuery{Order: OrderDescending, Limit: 20}
	if values := query["limit"]; len(values) == 1 {
		limit, err := strconv.ParseInt(values[0], 10, 32)
		if err != nil || limit < 1 || limit > 100 {
			return nil, invalidMedia("Video list limit must be between 1 and 100.")
		}
		out.Limit = int(limit)
	}
	switch order := query.Get("order"); order {
	case "":
	case "asc":
		out.Order = OrderAscending
	case "desc":
	default:
		return nil, invalidMedia("Video list order must be asc or desc.")
	}
	if after := query.Get("after"); after != "" {
		cursor, err := uuid.Parse(after)
		if err != nil {
			return nil, invalidMedia("The video cursor is invalid.")
		}
		out.After = &cursor
	}
	return out, nil
}

func ValidateVideoContentQuery(query url.Values) (string, *Error) {
	for name, values := range query {
		if name != "variant" || len(values) != 1 {
			return "", invalidMedia("Video content contains unsupported query parameters.")
		}
	}
	variant := query.Get("variant")
	switch variant {
	case "", "video", "thumbnail", "spritesheet":
		return variant, nil
	}
	return "", invalidMedia("The video content variant must be video, thumbnail, or spritesheet.")
}

func ValidUpstreamJobID(value string) bool {
	return value != "" && len(value) <= 1024 && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00") && validJobIDChars(value)
}

func validJobIDChars(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func VideoJobPath(jobID, suffix string) (string, *Error) {
	if jobID == "" || len(jobID) > 256 {
		return "", invalidMedia("The upstream video job ID is invalid.")
	}
	for i := 0; i < len(jobID); i++ {
		c := jobID[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return "", invalidMedia("The upstream video job ID is invalid.")
		}
	}
	if suffix == "" {
		return "videos/" + jobID, nil
	}
	return "videos/" + jobID + "/" + suffix, nil
}

func int64Text(value *int64) *string {
	if value == nil {
		return nil
	}
	text := strconv.FormatInt(*value, 10)
	return &text
}

func float64Text(value *float64) *string {
	if value == nil {
		return nil
	}
	text := strconv.FormatFloat(*value, 'g', -1, 64)
	return &text
}

func int64Ptr(value *int) *int64 {
	if value == nil {
		return nil
	}
	v := int64(*value)
	return &v
}
