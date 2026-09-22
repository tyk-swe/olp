package operations

import (
	"encoding/json"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
)

func Error(code, field, requirement, message string) error {
	return &oif.Incompatibility{Code: code, Field: field, Requirement: requirement, Message: message}
}
func Invalid(field, message string) error {
	return Error("unsupported_parameter", field, "native_request", message)
}
func Violation(field, requirement string) error {
	return Error("fidelity_protocol_violation", field, requirement, "The native result does not satisfy the admitted operation contract.")
}
func Member(value oif.Value, name string) oif.Value { out, _ := value.Lookup(name); return out }
func String(value oif.Value) string                 { out, _ := value.Text(); return out }
func Int(value oif.Value) (int64, bool) {
	if value.Kind() != oif.Number {
		return 0, false
	}
	n, err := strconv.ParseInt(value.Raw(), 10, 64)
	return n, err == nil
}
func Uint(value oif.Value) (uint64, bool) {
	if value.Kind() != oif.Number {
		return 0, false
	}
	n, err := strconv.ParseUint(value.Raw(), 10, 64)
	return n, err == nil
}
func Number(value oif.Value) bool {
	// The source parser already validates JSON number syntax. A score or vector
	// coordinate may exceed float64's range, and casting it here would impose a
	// representation limit on an otherwise valid native result.
	return value.Kind() == oif.Number && len(value.Raw()) <= 256
}
func Raw(value any) json.RawMessage    { out, _ := json.Marshal(value); return out }
func Pointer(path, name string) string { return oif.Pointer(path, name) }
func Optional(value oif.Value) bool    { return value.Kind() == oif.Absent || value.Kind() == oif.Null }

func ModelChanges(doc oif.Document, model string) ([]oif.Change, error) {
	if _, present := doc.Root().Lookup("model"); !present {
		return nil, nil
	}
	encoded, _ := json.Marshal(model)
	return []oif.Change{{Pointer: "/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "published model binding"}}, nil
}

func ObjectSchema(properties map[string]any, required ...string) json.RawMessage {
	return Raw(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": true})
}
func FieldSchema(schema map[string]any, validate func(oif.Value) error) Field {
	return Field{Schema: Raw(schema), Validate: validate}
}
func NullableBool(value oif.Value) error {
	if !Optional(value) && value.Kind() != oif.Boolean {
		return Invalid("default", "Use a boolean or native null.")
	}
	return nil
}
func PositiveInt(value oif.Value) error {
	if value.Kind() == oif.Null {
		return nil
	}
	n, ok := Int(value)
	if !ok || n <= 0 {
		return Invalid("default", "Use a positive integer or native null.")
	}
	return nil
}

func Strings(value oif.Value, allowTokens bool) (int, []Text, error) {
	if value.Kind() == oif.String {
		return 1, []Text{{Value: String(value)}}, nil
	}
	if value.Kind() != oif.Array || len(value.Elements()) == 0 {
		return 0, nil, Invalid("input", "Use a non-empty native input collection.")
	}
	items := value.Elements()
	if allowTokens && items[0].Kind() == oif.Number {
		for _, v := range items {
			if _, ok := Uint(v); !ok {
				return 0, nil, Invalid("input", "Token inputs require non-negative integer IDs.")
			}
		}
		return 1, nil, nil
	}
	texts := []Text{}
	for index, v := range items {
		if v.Kind() == oif.String {
			texts = append(texts, Text{Pointer: "/" + strconv.Itoa(index), Value: String(v)})
			continue
		}
		if allowTokens && v.Kind() == oif.Array && len(v.Elements()) > 0 {
			for _, token := range v.Elements() {
				if _, ok := Uint(token); !ok {
					return 0, nil, Invalid("input", "Token inputs require non-negative integer IDs.")
				}
			}
			continue
		}
		return 0, nil, Invalid("input", "Input elements have no representation in this dialect.")
	}
	return len(items), texts, nil
}

// SameValue checks document identity without serializing it or reordering the
// returned source. Native strings may use equivalent JSON escape spellings.
func SameValue(a, b oif.Value) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case oif.String:
		return String(a) == String(b)
	case oif.Array:
		aa, bb := a.Elements(), b.Elements()
		if len(aa) != len(bb) {
			return false
		}
		for i := range aa {
			if !SameValue(aa[i], bb[i]) {
				return false
			}
		}
		return true
	case oif.Object:
		aa, bb := a.Members(), b.Members()
		if len(aa) != len(bb) {
			return false
		}
		for _, m := range aa {
			v, ok := b.Lookup(m.Name)
			if !ok || !SameValue(m.Value, v) {
				return false
			}
		}
		return true
	default:
		return a.Raw() == b.Raw()
	}
}
