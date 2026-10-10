package gateway

import (
	"bytes"

	"github.com/tyk-swe/olp/internal/oif"
)

// parseObjectRequest rejects an invalid envelope before allocating its source
// index. Accepted native source, including whitespace, remains unchanged.
func parseObjectRequest(body []byte, bounds oif.Limits) (oif.Document, error) {
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return oif.Document{}, &oif.SourceError{Code: "expected_object"}
	}
	return oif.ParseJSON(body, bounds)
}
