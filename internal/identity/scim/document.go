package scim

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
)

const (
	UserSchema     = "urn:ietf:params:scim:schemas:core:2.0:User"
	GroupSchema    = "urn:ietf:params:scim:schemas:core:2.0:Group"
	UserExtension  = "urn:openllmproxy:params:scim:schemas:extension:2.0:User"
	GroupExtension = "urn:openllmproxy:params:scim:schemas:extension:2.0:Group"
	PatchSchema    = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	ListSchema     = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SearchSchema   = "urn:ietf:params:scim:api:messages:2.0:SearchRequest"
	ErrorSchema    = "urn:ietf:params:scim:api:messages:2.0:Error"
	MaxBody        = 128 << 10
)

type Error struct {
	Status       int
	Type, Detail string
}

func (e *Error) Error() string { return e.Type }

func Fail(status int, typ, detail string) error { return &Error{status, typ, detail} }

func Invalid(detail string) error { return Fail(400, "invalidValue", detail) }

type Document map[string]any

func NameSubattributes() []string {
	return []string{"formatted", "familyName", "givenName", "middleName", "honorificPrefix", "honorificSuffix"}
}

var names = maps.Collect(func(yield func(string, string) bool) {
	for _, n := range slices.Concat([]string{"schemas", "id", "externalId", "userName", "displayName", "display", "active", "name", "emails", "value", "type", "primary", "roles", "groups", "members", "$ref", "meta", "resourceType", "created", "lastModified", "location", "version", "password", "role", "accessScope", "projects", "operations", "op", "path", "filter", "startIndex", "count", "attributes", "excludedAttributes", "sortBy", "sortOrder", UserExtension, GroupExtension}, NameSubattributes()) {
		if !yield(strings.ToLower(n), n) {
			return
		}
	}
})

func Canonical(name string) string {
	if n, ok := names[strings.ToLower(name)]; ok {
		return n
	}
	return name
}

func Decode(r io.Reader) (Document, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBody+1))
	if err != nil || len(data) > MaxBody {
		return nil, Fail(413, "tooLarge", "SCIM documents are limited to 128 KiB.")
	}
	if _, err = oif.ParseJSON(data, oif.Limits{MaxBytes: MaxBody, MaxDepth: 24, MaxNodes: 16384}); err != nil {
		return nil, Fail(400, "invalidSyntax", "Provide one unambiguous JSON object.")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var raw map[string]any
	if d.Decode(&raw) != nil || raw == nil {
		return nil, Fail(400, "invalidSyntax", "Provide one JSON object.")
	}
	normalized, err := normalizeObject(raw)
	if err != nil {
		return nil, err
	}
	return Document(normalized), nil
}

func normalizeObject(v map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(v))
	seen := make(map[string]bool, len(v))
	for k, x := range v {
		key := strings.ToLower(k)
		if seen[key] {
			return nil, Fail(400, "invalidSyntax", "Attribute names must be unambiguous ignoring case.")
		}
		seen[key] = true
		n, err := normalize(x)
		if err != nil {
			return nil, err
		}
		out[Canonical(k)] = n
	}
	return out, nil
}

func normalize(value any) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		return normalizeObject(v)
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			n, err := normalize(x)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	default:
		return value, nil
	}
}

func String(d map[string]any, k string) string { s, _ := d[k].(string); return s }

func Schema(d Document, required string) error {
	values, ok := d["schemas"].([]any)
	if !ok || len(values) > 8 {
		return Invalid("Provide the resource schemas.")
	}
	for _, v := range values {
		if v == required {
			return nil
		}
	}
	return Invalid("The resource schema is missing.")
}

func Clone(d Document) Document {
	if d == nil {
		return nil
	}
	return Document(cloneObject(map[string]any(d)))
}

func cloneObject(x map[string]any) map[string]any {
	out := make(map[string]any, len(x))
	for k, v := range x {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneObject(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}
