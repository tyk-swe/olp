// Package scim implements bounded SCIM 2.0 protocol documents and queries.
package scim

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
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

var names = map[string]string{}

func init() {
	for _, n := range []string{"schemas", "id", "externalId", "userName", "displayName", "display", "active", "name", "formatted", "familyName", "givenName", "middleName", "honorificPrefix", "honorificSuffix", "emails", "value", "type", "primary", "roles", "groups", "members", "$ref", "meta", "resourceType", "created", "lastModified", "location", "version", "password", "role", "accessScope", "projects", "operations", "op", "path", "filter", "startIndex", "count", "attributes", "excludedAttributes", "sortBy", "sortOrder", UserExtension, GroupExtension} {
		names[strings.ToLower(n)] = n
	}
}

func Canonical(name string) string {
	if n, ok := names[strings.ToLower(name)]; ok {
		return n
	}
	return name
}

func Decode(r io.Reader) (Document, error) {
	data, e := io.ReadAll(io.LimitReader(r, MaxBody+1))
	if e != nil || len(data) > MaxBody {
		return nil, Fail(413, "tooLarge", "SCIM documents are limited to 128 KiB.")
	}
	if _, e = oif.ParseJSON(data, oif.Limits{MaxBytes: MaxBody, MaxDepth: 24, MaxNodes: 16384}); e != nil {
		return nil, Fail(400, "invalidSyntax", "Provide one unambiguous JSON object.")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var raw map[string]any
	if d.Decode(&raw) != nil || raw == nil {
		return nil, Fail(400, "invalidSyntax", "Provide one JSON object.")
	}
	normalized, e := normalize(raw)
	if e != nil {
		return nil, e
	}
	return Document(normalized.(map[string]any)), nil
}

func normalize(value any) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		seen := map[string]bool{}
		for k, x := range v {
			key := strings.ToLower(k)
			if seen[key] {
				return nil, Fail(400, "invalidSyntax", "Attribute names must be unambiguous ignoring case.")
			}
			seen[key] = true
			n, e := normalize(x)
			if e != nil {
				return nil, e
			}
			out[Canonical(k)] = n
		}
		return out, nil
	case []any:
		for i, x := range v {
			n, e := normalize(x)
			if e != nil {
				return nil, e
			}
			v[i] = n
		}
		return v, nil
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

func Clone(d Document) Document { return Document(cloneValue(map[string]any(d)).(map[string]any)) }

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = cloneValue(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = cloneValue(v)
		}
		return out
	default:
		return v
	}
}
