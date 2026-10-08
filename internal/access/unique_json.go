package access

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

// DecodeUnique is the bounded configuration boundary for model-significant
// documents. Validate original members before typed maps could turn ambiguous
// duplicate keys into last-writer-wins configuration. Source errors never echo
// values or names that might contain secrets.
func DecodeUnique(r *http.Request, destination any, limit int64) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return Fail(415, "unsupported_media_type", "Send an application/json request.")
	}
	original := r.Body
	defer original.Close()
	data, err := io.ReadAll(io.LimitReader(original, limit+1))
	if err != nil || int64(len(data)) > limit {
		return Fail(400, "invalid_json", "The request body exceeds this operation's bounded JSON contract.")
	}
	document, err := oif.ParseJSON(data, oif.Limits{MaxBytes: int(limit), MaxDepth: 128, MaxNodes: 1 << 20})
	if err != nil {
		return Fail(400, "invalid_json", "Configuration must contain one valid, unambiguous JSON document within its structural limits.")
	}
	if err := canonicalMembers(document.Root(), reflect.TypeOf(destination), ""); err != nil {
		return Fail(400, "invalid_json", "Configuration members must use their declared names and types; reset a default by removing it, not by replacing its definition with null.")
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return Decode(r, destination)
}

// Go's JSON decoder accepts case-insensitive field aliases and scalar null as a
// zero value. Those coercions are unsafe for explicit model configuration. Raw
// native JSON remains opaque here, so distinct schema properties foo/Foo and
// native null retain their meaning instead of being conflated by reflection.
func canonicalMembers(value oif.Value, shape reflect.Type, fieldName string) error {
	if shape == nil || shape == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	for shape.Kind() == reflect.Pointer {
		if value.Kind() == oif.Null {
			return nil
		}
		shape = shape.Elem()
	}
	if shape == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	switch shape.Kind() {
	case reflect.Interface:
		return nil
	case reflect.Struct:
		if value.Kind() != oif.Object {
			return errors.New("object required")
		}
		fields := canonicalFields(shape)
		for _, member := range value.Members() {
			field, ok := fields[member.Name]
			if !ok {
				return errors.New("noncanonical field")
			}
			if err := canonicalMembers(member.Value, field, member.Name); err != nil {
				return err
			}
		}
	case reflect.Map:
		if value.Kind() == oif.Null {
			switch fieldName {
			case "semantic_headers", "query_settings", "operation_defaults", "bindings", "values", "native_options":
				return errors.New("configuration definition cannot be null")
			}
			return nil
		}
		if value.Kind() != oif.Object {
			return errors.New("object required")
		}
		for _, member := range value.Members() {
			if err := canonicalMembers(member.Value, shape.Elem(), member.Name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Kind() == oif.Null {
			return nil
		}
		if value.Kind() != oif.Array {
			return errors.New("array required")
		}
		for _, item := range value.Elements() {
			if err := canonicalMembers(item, shape.Elem(), fieldName); err != nil {
				return err
			}
		}
	default:
		if value.Kind() == oif.Null {
			return errors.New("scalar cannot be null")
		}
	}
	return nil
}

// canonicalFields follows encoding/json's embedded-field selection: shallower
// fields win, explicit JSON names win at equal depth, and equally dominant
// names are ambiguous. Anonymous struct carriers are not wire members.
func canonicalFields(root reflect.Type) map[string]reflect.Type {
	type level struct {
		shape reflect.Type
		depth int
	}
	type candidate struct {
		shape             reflect.Type
		depth             int
		tagged, ambiguous bool
	}
	queue := []level{{shape: root}}
	seen := map[reflect.Type]int{root: 0}
	candidates := map[string]candidate{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for i := 0; i < current.shape.NumField(); i++ {
			field := current.shape.Field(i)
			nested := field.Type
			if nested.Kind() == reflect.Pointer {
				nested = nested.Elem()
			}
			if field.PkgPath != "" && (!field.Anonymous || nested.Kind() != reflect.Struct) {
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")[0]
			if tag == "-" {
				continue
			}
			if field.Anonymous && tag == "" && nested.Kind() == reflect.Struct {
				depth := current.depth + 1
				if previous, ok := seen[nested]; !ok || previous >= depth {
					seen[nested] = depth
					queue = append(queue, level{nested, depth})
				}
				continue
			}
			name := tag
			if name == "" {
				name = field.Name
			}
			next := candidate{shape: field.Type, depth: current.depth, tagged: tag != ""}
			previous, exists := candidates[name]
			switch {
			case !exists || next.depth < previous.depth || next.depth == previous.depth && next.tagged && !previous.tagged:
				candidates[name] = next
			case next.depth == previous.depth && next.tagged == previous.tagged:
				previous.ambiguous = true
				candidates[name] = previous
			}
		}
	}
	fields := make(map[string]reflect.Type, len(candidates))
	for name, field := range candidates {
		if !field.ambiguous {
			fields[name] = field.shape
		}
	}
	return fields
}
