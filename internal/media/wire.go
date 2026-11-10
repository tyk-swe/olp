package media

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/staging"
)

const (
	DefaultImageUploadLimit    int64 = 50 * 1024 * 1024
	DefaultAudioUploadLimit    int64 = 25 * 1024 * 1024
	DefaultVideoReferenceLimit int64 = 20 * 1024 * 1024
	MaxVideoPromptLength             = 32000
)

type AudioResult struct {
	Audio       []byte
	ContentType string
	Tokens      *ImageUsage
}

func jsonDoc(fields map[string]any, extra map[string]any) ([]byte, *Error) {
	doc := make(map[string]any, len(fields)+len(extra))
	for name, value := range fields {
		if value == nil {
			continue
		}
		if v := reflect.ValueOf(value); v.Kind() == reflect.Pointer && v.IsNil() {
			continue
		}
		doc[name] = value
	}
	for name, value := range extra {
		if _, exists := doc[name]; exists {
			return nil, invalidMedia("The extension field " + strconv.Quote(name) + " collides with a request field.")
		}
		doc[name] = value
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, invalidMedia("The media request could not be encoded.")
	}
	return body, nil
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

func extraFields(fields []staging.Field, extra map[string]any) []staging.Field {
	if len(extra) == 0 {
		return fields
	}
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		value := extra[name]
		if values, ok := value.([]any); ok {
			field := name
			if !strings.HasSuffix(field, "[]") {
				field += "[]"
			}
			for _, item := range values {
				text := extraText(item)
				fields = append(fields, staging.Field{Name: field, Value: staging.Text(text)})
			}
			continue
		}
		text := extraText(value)
		fields = append(fields, staging.Field{Name: name, Value: staging.Text(text)})
	}
	return fields
}

func extraText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(data)
	}
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
