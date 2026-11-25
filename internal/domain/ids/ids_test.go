package ids_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/ids"
)

func TestCanonicalAcceptsOnlyTheLowerCaseHyphenatedForm(t *testing.T) {
	t.Parallel()
	minted := ids.New()
	cases := []struct {
		name                 string
		raw                  string
		canonical, orNilForm bool
	}{
		{"minted", minted, true, true},
		{"fixed", "123e4567-e89b-12d3-a456-426614174000", true, true},
		{"nil", uuid.Nil.String(), false, true},
		{"upper case", "123E4567-E89B-12D3-A456-426614174000", false, false},
		{"no hyphens", "123e4567e89b12d3a456426614174000", false, false},
		{"braced", "{" + minted + "}", false, false},
		{"urn", "urn:uuid:" + minted, false, false},
		{"padded", " " + minted, false, false},
		{"upper case minted", strings.ToUpper(minted), false, false},
		{"empty", "", false, false},
		{"not a uuid", "not-a-uuid", false, false},
	}
	for _, c := range cases {
		if got := ids.Canonical(c.raw); got != c.canonical {
			t.Errorf("%s: Canonical(%q) = %v, want %v", c.name, c.raw, got, c.canonical)
		}
		if got := ids.CanonicalOrNil(c.raw); got != c.orNilForm {
			t.Errorf("%s: CanonicalOrNil(%q) = %v, want %v", c.name, c.raw, got, c.orNilForm)
		}
	}
}

func TestNewMintsDistinctVersionSevenIDs(t *testing.T) {
	t.Parallel()
	first, second := ids.New(), ids.New()
	if first == second {
		t.Fatalf("New returned %q twice", first)
	}
	for _, raw := range []string{first, second} {
		parsed, err := uuid.Parse(raw)
		if err != nil || parsed.Version() != 7 {
			t.Fatalf("New returned %q, want a version 7 UUID", raw)
		}
	}
}
