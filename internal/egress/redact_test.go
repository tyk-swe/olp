package egress

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSensitiveRedactsEveryEncodingOfAppliedCredentials(t *testing.T) {
	var s Sensitive
	s.Add(" \t"+`secret-"\<&`+"\t ", `secret-"\<&-long`, "", " \t ")
	escaped, _ := json.Marshal(`secret-"\<&-long`)
	var unescaped strings.Builder
	encoder := json.NewEncoder(&unescaped)
	encoder.SetEscapeHTML(false)
	encoder.Encode(`secret-"\<&`)
	for _, text := range []string{
		`raw secret-"\<&-long and secret-"\<&`,
		"json " + string(escaped),
		"unescaped " + unescaped.String(),
	} {
		redacted := s.Redact(text)
		if strings.Contains(redacted, "secret-") || strings.Contains(redacted, "-long") || !strings.Contains(redacted, "[REDACTED]") {
			t.Fatalf("credential survived redaction: %q -> %q", text, redacted)
		}
		if !s.Contains(text) {
			t.Fatalf("Contains missed a credential in %q", text)
		}
	}
	if got := s.Redact("nothing sensitive"); got != "nothing sensitive" || s.Contains(got) {
		t.Fatal("redaction changed text without credentials", got)
	}
}

func TestSensitiveIncludesAnotherCollection(t *testing.T) {
	var first, second Sensitive
	first.Add("alpha-key")
	second.Add("beta-key")
	first.Include(second)
	if got := first.Redact("alpha-key beta-key"); got != "[REDACTED] [REDACTED]" {
		t.Fatal("included credentials were not redacted", got)
	}
	var empty Sensitive
	if got := empty.Redact("alpha-key"); got != "alpha-key" {
		t.Fatal("an empty collection must not change text", got)
	}
}
