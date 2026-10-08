package gateway

import (
	"net/http"
	"strings"
)

// semanticHeaders is the request's header set as the semantic binders read it.
// Field lines of one name are one comma-separated list (RFC 9110 section 5.3),
// and the official Go SDK for Anthropic sends each beta as a line of its own,
// so the lines of Anthropic-Beta are folded into one value. The binders refuse
// a repeated semantic header as ambiguous, which stays true of the others: a
// second Anthropic-Version still has no meaning.
func semanticHeaders(header http.Header) http.Header {
	out := header.Clone()
	// Canonical names keep absent private-header removal allocation-free.
	out.Del("X-Olp-End-User")
	out.Del("X-Olp-Attribution")
	out.Del(callerCredentialHeader)
	if values := out.Values("Anthropic-Beta"); len(values) > 1 {
		out.Set("Anthropic-Beta", strings.Join(values, ","))
	}
	return out
}
