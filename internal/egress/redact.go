package egress

import (
	"cmp"
	"encoding/json"
	"net/textproto"
	"slices"
	"strings"
)

// Sensitive collects the credential values applied to outbound requests, so
// any text derived from a provider's reply can be scrubbed before a client or
// log sees it. Providers echo credentials in diagnostics, sometimes inside a
// JSON serialization of the request headers. A Sensitive is not safe for
// concurrent use.
type Sensitive struct {
	values []string
}

// Add records credential values as sent. Surrounding spaces and tabs are
// dropped because HTTP strips them from header values before sending.
func (s *Sensitive) Add(values ...string) {
	for _, value := range values {
		if value = textproto.TrimString(value); value != "" {
			s.values = append(s.values, value)
		}
	}
}

// Include records every value another collection holds.
func (s *Sensitive) Include(other Sensitive) {
	s.values = append(s.values, other.values...)
}

// Values returns a copy of the recorded values for a plugin runtime to redact
// its own output before returning it to the host.
func (s Sensitive) Values() []string { return slices.Clone(s.values) }

// Redact replaces each recorded value, including its JSON-escaped forms, with
// [REDACTED]. Longer values are replaced first so a credential that contains
// another cannot leave part of itself exposed.
func (s Sensitive) Redact(text string) string {
	if len(s.values) == 0 || text == "" {
		return text
	}
	var forms []string
	for _, value := range s.values {
		forms = append(forms, value)
		quoted, _ := json.Marshal(value)
		forms = append(forms, string(quoted[1:len(quoted)-1]))
		// Upstream JSON encoders may leave HTML characters unescaped.
		var unescaped strings.Builder
		encoder := json.NewEncoder(&unescaped)
		encoder.SetEscapeHTML(false)
		encoder.Encode(value)
		encoded := unescaped.String()
		forms = append(forms, encoded[1:len(encoded)-2]) // quotes and trailing newline
	}
	slices.SortStableFunc(forms, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	pairs := make([]string, 0, 2*len(forms))
	for _, form := range forms {
		pairs = append(pairs, form, "[REDACTED]")
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// Contains reports whether text holds any recorded value.
func (s Sensitive) Contains(text string) bool { return s.Redact(text) != text }
