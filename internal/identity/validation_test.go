package identity

import (
	"strings"
	"testing"
)

func TestEmailValidation(t *testing.T) {
	t.Parallel()
	valid := []struct {
		input string
		want  string
	}{
		{"Alice@Example.COM", "alice@example.com"},
		{"  bob+tag@sub.example.org \t", "bob+tag@sub.example.org"},
		{strings.Repeat("a", 64) + "@" + strings.Repeat("b", 185) + ".com", strings.Repeat("a", 64) + "@" + strings.Repeat("b", 185) + ".com"},
	}
	for _, tc := range valid {
		got, err := email(tc.input)
		if err != nil {
			t.Errorf("email(%q) returned error %v, want %q", tc.input, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("email(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"not-an-email",
		"Alice <alice@example.com>",
		"<alice@example.com>",
		"alice @example.com",
		"alice\n@example.com",
		"alice\x00@example.com",
		"alice@",
		"@example.com",
		strings.Repeat("a", 64) + "@" + strings.Repeat("b", 186) + ".com",
	}
	for _, raw := range invalid {
		if got, err := email(raw); err == nil {
			t.Errorf("email(%q) = %q, want error", raw, got)
		}
	}
}

func TestPasswordValidation(t *testing.T) {
	t.Parallel()
	valid := []string{
		strings.Repeat("a", 12),
		strings.Repeat("a", 1024),
		strings.Repeat("é", 12),
		strings.Repeat("🔑", 12),
	}
	for _, candidate := range valid {
		if err := password(candidate); err != nil {
			t.Errorf("password(len=%d runes) returned error %v, want nil", len([]rune(candidate)), err)
		}
	}

	invalid := []string{
		"",
		strings.Repeat("a", 11),
		strings.Repeat("🔑", 11),
		strings.Repeat("a", 1025),
	}
	for _, candidate := range invalid {
		if err := password(candidate); err == nil {
			t.Errorf("password(len=%d runes) accepted, want error", len([]rune(candidate)))
		}
	}
}

func TestSafeReturn(t *testing.T) {
	t.Parallel()
	allowed := []string{
		"",
		"/",
		"/settings/profile",
		"/projects/123?tab=keys&next=%2Fhome#section",
		"/search?q=hello%20world",
	}
	for _, target := range allowed {
		if !safeReturn(target) {
			t.Errorf("safeReturn(%q) = false, want true", target)
		}
	}

	rejected := []string{
		"settings/profile",
		"https://evil.example/login",
		"http://evil.example/",
		"javascript:alert(1)",
		"//evil.example/path",
		"//user@evil.example/path",
		"/%2f/evil.example",
		"/%2Fevil.example",
		"/\\evil.example",
		"/%5cevil.example",
		"/%5Cevil.example",
		"/path\\sub",
		"/path\r\nLocation: https://evil.example",
		"/path%0d%0aheader",
		"/path%00null",
		"/path\x7fdel",
		"/%ZZ",
		"/%ff",
	}
	for _, target := range rejected {
		if safeReturn(target) {
			t.Errorf("safeReturn(%q) = true, want false", target)
		}
	}
}
