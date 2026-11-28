package identity

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/problem"
)

func TestValidatePurpose(t *testing.T) {
	t.Parallel()
	const validUUID = "01900000-0000-7000-8000-000000000001"

	noResource := []string{"password_enrollment", "oidc_link", "saml_link", "plugin_permit", "mfa_manage"}
	for _, purpose := range noResource {
		if err := validatePurpose(purpose, ""); err != nil {
			t.Errorf("validatePurpose(%q, \"\") = %v, want nil", purpose, err)
		}
		if err := validatePurpose(purpose, validUUID); err == nil {
			t.Errorf("validatePurpose(%q, uuid) = nil, want error", purpose)
		}
	}

	withResource := []string{"oidc_unlink", "saml_unlink", "agent_delegation"}
	for _, purpose := range withResource {
		if err := validatePurpose(purpose, validUUID); err != nil {
			t.Errorf("validatePurpose(%q, uuid) = %v, want nil", purpose, err)
		}
		if err := validatePurpose(purpose, ""); err == nil {
			t.Errorf("validatePurpose(%q, \"\") = nil, want error", purpose)
		}
		if err := validatePurpose(purpose, "not-a-uuid"); err == nil {
			t.Errorf("validatePurpose(%q, invalid) = nil, want error", purpose)
		}
	}

	if err := validatePurpose("unknown", ""); err == nil {
		t.Error("validatePurpose(\"unknown\", \"\") = nil, want error")
	}
	if err := validatePurpose("unknown", validUUID); err == nil {
		t.Error("validatePurpose(\"unknown\", uuid) = nil, want error")
	}
}

func TestBrowserHint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		agent string
		want  string
	}{
		{"", "Unknown browser"},
		{"CustomAgent/1.0", "Unknown browser"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0", "Edge on Windows"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36", "Chrome on macOS"},
		{"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/120.0.0.0 Mobile Safari/537.36", "Chrome on Android"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Safari/604.1", "Safari on iPhone"},
		{"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Safari/604.1", "Safari on iPad"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0", "Firefox on Linux"},
		{"curl/8.5.0", "curl"},
		{"CustomBot/1.0 (Linux)", "Unknown browser on Linux"},
	}
	for _, tc := range cases {
		if got := browserHint(tc.agent); got != tc.want {
			t.Errorf("browserHint(%q) = %q, want %q", tc.agent, got, tc.want)
		}
	}
}

func TestProvisioningID(t *testing.T) {
	t.Parallel()
	reqWith := func(name, value string) *http.Request {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/provisioning/x/users/y", nil)
		r.SetPathValue(name, value)
		return r
	}

	if got, err := provisioningID(reqWith("source", "hr-directory"), "source", "source", 100); err != nil || got != "hr-directory" {
		t.Fatalf("valid source: got=(%q, %v)", got, err)
	}
	if got, err := provisioningID(reqWith("external_id", "scim"), "external_id", "external_id", 255); err != nil || got != "scim" {
		t.Fatalf("external_id named scim: got=(%q, %v)", got, err)
	}
	exact := strings.Repeat("x", 10)
	if got, err := provisioningID(reqWith("source", exact), "source", "source", 10); err != nil || got != exact {
		t.Fatalf("exact max length: got=(%q, %v)", got, err)
	}

	_, scimErr := provisioningID(reqWith("source", "scim"), "source", "source", 100)
	var problem *problem.Problem
	if !errors.As(scimErr, &problem) || problem.Status != http.StatusConflict || problem.Code != "managed_provisioning_source" {
		t.Fatalf("scim source err = %v, want 409 managed_provisioning_source", scimErr)
	}

	invalid := []string{
		"",
		strings.Repeat("x", 11),
		"bad/slash",
		"bad\rline",
		"bad\nline",
		"bad\x00null",
		"bad\ttab",
	}
	for _, val := range invalid {
		if got, err := provisioningID(reqWith("source", val), "source", "source", 10); err == nil {
			t.Errorf("provisioningID(%q) = %q, want error", val, got)
		}
	}
}
