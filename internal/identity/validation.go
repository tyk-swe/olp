package identity

import (
	"net/mail"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/access/httpkit"
)

func email(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	a, err := mail.ParseAddress(normalized)
	if err != nil || a.Address != normalized || len(normalized) > 254 {
		return "", httpkit.Invalid("email", "Enter a valid email address.")
	}
	return normalized, nil
}

func password(value string) error {
	n := utf8.RuneCountInString(value)
	if n < 12 || n > 1024 {
		return httpkit.Invalid("password", "Use 12–1024 characters.")
	}
	return nil
}

func safeReturn(value string) bool {
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "/") {
		return false
	}
	// Check escaped input for controls, backslashes, and authority prefixes,
	// while preserving safe query escapes in the original redirect destination.
	decoded, err := url.PathUnescape(value)
	if err != nil || !utf8.ValidString(decoded) || strings.HasPrefix(decoded, "//") || strings.Contains(decoded, "\\") || strings.IndexFunc(decoded, unicode.IsControl) >= 0 {
		return false
	}
	path, err := url.Parse(value)
	if err != nil {
		return false
	}
	base := url.URL{Scheme: "https", Host: "console.invalid", Path: "/"}
	destination := base.ResolveReference(path)
	return destination.Scheme == base.Scheme && destination.Host == base.Host && destination.User == nil
}

func validatePurpose(purpose, resource string) error {
	switch purpose {
	case "password_enrollment", "oidc_link", "saml_link", "plugin_permit", "mfa_manage":
		if resource == "" {
			return nil
		}
	case "oidc_unlink", "saml_unlink", "agent_delegation":
		_, err := httpkit.ParseUUID(resource)
		return err
	}
	return httpkit.Invalid("purpose", "Use a supported security purpose and its required resource ID.")
}
