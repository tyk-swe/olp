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
	value = strings.ToLower(strings.TrimSpace(value))
	a, err := mail.ParseAddress(value)
	if err != nil || a.Address != value || len(value) > 254 {
		return "", httpkit.Invalid("email", "Enter a valid email address.")
	}
	return value, nil
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
