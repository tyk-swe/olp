package generation

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

// These helpers are the operation's shared JSON/contract vocabulary. Dialect
// registrations and qualified mappings use them; they carry no provider
// knowledge of their own.

func Member(value oif.Value, name string) oif.Value { out, _ := value.Lookup(name); return out }

func Text(value oif.Value) string { text, _ := value.Text(); return text }

// Field returns a top-level document member verbatim, or nil when absent.
func Field(document oif.Document, name string) json.RawMessage {
	return Member(document.Root(), name).Bytes()
}

func OnlyMembers(value oif.Value, allowed string) bool {
	if value.Kind() != oif.Object {
		return false
	}
	names := strings.Fields(allowed)
	for _, field := range value.Members() {
		if !slices.Contains(names, field.Name) {
			return false
		}
	}
	return true
}

func NonnegativeInteger(value oif.Value) bool {
	if value.Kind() != oif.Number {
		return false
	}
	n, err := strconv.ParseInt(value.Raw(), 10, 64)
	return err == nil && n >= 0
}

func EmptyOptional(value oif.Value) bool {
	return value.Kind() == oif.Absent || value.Kind() == oif.Null || value.Kind() == oif.Array && len(value.Elements()) == 0
}

// Incompatible is an interaction-contract rejection: a caller-visible
// admission failure carrying code, field and requirement.
func Incompatible(code, field, requirement, message string) *oif.Incompatibility {
	return &oif.Incompatibility{Code: code, Field: field, Requirement: requirement, Message: message}
}

// GuardFailure is a fidelity violation: the provider result or event failed
// its admitted grammar.
func GuardFailure(field, requirement string) error {
	return Incompatible("fidelity_protocol_violation", field, requirement, "The provider result does not satisfy the admitted interaction contract.")
}

// ContinuationFailure rejects a negotiated client continuation contract.
func ContinuationFailure(field, requirement string) error {
	return Incompatible("state_carrier", field, requirement, "The selected client continuation contract cannot preserve this interaction.")
}

// SameValue compares JSON structure without float conversion. Member order is
// immaterial for SDK serialization; array order and numeric lexemes remain
// authoritative, including signed zero and precision outside IEEE754.
func SameValue(a, b oif.Value) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case oif.Object:
		if len(a.Members()) != len(b.Members()) {
			return false
		}
		for _, m := range a.Members() {
			v, present := b.Lookup(m.Name)
			if !present || !SameValue(m.Value, v) {
				return false
			}
		}
		return true
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
	case oif.String:
		return Text(a) == Text(b)
	default:
		return a.Raw() == b.Raw()
	}
}

// SameSource verifies an invocation for delivery replay with exact numeric
// lexemes and ordered arrays, allowing only insignificant JSON object order.
func SameSource(a, b []byte) bool {
	first, err := oif.ParseJSON(a, oif.Limits{})
	if err != nil {
		return false
	}
	second, err := oif.ParseJSON(b, oif.Limits{})
	return err == nil && SameValue(first.Root(), second.Root())
}

// ContinuationFrame wraps projected client JSON as one SSE data frame without
// decoding native numbers.
func ContinuationFrame(raw []byte) []byte {
	return bytes.Join([][]byte{[]byte("data: "), raw, []byte("\n\n")}, nil)
}

// ContinuationActionable marks a projected client frame carrying an actionable
// tool-call delta.
func ContinuationActionable(frame []byte) bool {
	document, err := oif.ParseJSON(frame, oif.Limits{})
	if err != nil {
		return false
	}
	for _, choice := range Member(document.Root(), "choices").Elements() {
		if len(Member(Member(choice, "delta"), "tool_calls").Elements()) > 0 {
			return true
		}
	}
	return false
}

var unsafeField = []string{"authorization", "proxy-authorization", "x-api-key", "cookie", "set-cookie"}

// SafeField renders a member name safe for error fields: reserved headers are
// generalized rather than echoed.
func SafeField(name string) string {
	if slices.Contains(unsafeField, strings.ToLower(strings.TrimPrefix(name, "/"))) {
		return "/headers"
	}
	if strings.HasPrefix(name, "/") {
		return name
	}
	return "/" + name
}
