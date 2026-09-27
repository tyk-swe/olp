package providers

import "testing"

func TestProbeStatusErrorsUseLocalDiagnostics(t *testing.T) {
	for status, want := range map[int]probeError{
		400: {Code: "upstream_rejected", Detail: "The upstream answered HTTP 400."},
		401: {Code: "upstream_authentication_failed", Detail: "The upstream rejected the credential (HTTP 401)."},
		403: {Code: "upstream_permission_denied", Detail: "The upstream denied access (HTTP 403)."},
		404: {Code: "upstream_not_found", Detail: "The upstream has no such resource (HTTP 404)."},
		409: {Code: "upstream_rejected", Detail: "The upstream answered HTTP 409."},
		429: {Code: "upstream_rate_limit", Detail: "The upstream is rate limiting (HTTP 429)."},
		500: {Code: "upstream_unavailable", Detail: "The upstream failed (HTTP 500)."},
		503: {Code: "upstream_unavailable", Detail: "The upstream failed (HTTP 503)."},
	} {
		if got := statusError(status); *got != want {
			t.Errorf("HTTP %d: %+v, want %+v", status, *got, want)
		}
	}
}
