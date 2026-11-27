package capture

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
	"github.com/tyk-swe/olp/internal/domain/ids"
)

func strPtr(s string) *string { return &s }

func validSettings() Settings {
	return Settings{
		ProjectID:   strPtr(ids.New()),
		SampleRatio: "0.25",
		Include:     []Include{Input, Output},
		Redact:      []string{"pii_email"},
		KeyIDs:      []string{ids.New()},
		EndUsers:    []string{strings.Repeat("a", 64)},
		MaxBytes:    4096,
		Enabled:     true,
	}
}

func TestSettingsValidateBoundaries(t *testing.T) {
	t.Parallel()
	if err := validSettings().validate(); err != nil {
		t.Fatalf("validSettings rejected: %v", err)
	}

	routeOnly := validSettings()
	routeOnly.ProjectID = nil
	routeOnly.Route = strPtr("team-chat")
	if err := routeOnly.validate(); err != nil {
		t.Fatalf("route-scoped settings rejected: %v", err)
	}

	tooManyDetectors := make([]string, guardrailpolicy.MaxEntries+1)
	for i := range tooManyDetectors {
		tooManyDetectors[i] = "detector-" + strconv.Itoa(i)
	}
	tooManyKeys := make([]string, maxSelectors+1)
	for i := range tooManyKeys {
		tooManyKeys[i] = ids.New()
	}

	invalidCases := map[string]func(*Settings){
		"neither project nor route":     func(s *Settings) { s.ProjectID, s.Route = nil, nil },
		"sample ratio above 1":          func(s *Settings) { s.SampleRatio = "1.000000001" },
		"sample ratio negative":         func(s *Settings) { s.SampleRatio = "-0.1" },
		"sample ratio over 9 decimals":  func(s *Settings) { s.SampleRatio = "0.1234567890" },
		"empty include":                 func(s *Settings) { s.Include = nil },
		"duplicate include":             func(s *Settings) { s.Include = []Include{Input, Input} },
		"unknown include value":         func(s *Settings) { s.Include = []Include{Include("headers")} },
		"too many redactors":            func(s *Settings) { s.Redact = tooManyDetectors },
		"case-insensitive dup redactor": func(s *Settings) { s.Redact = []string{"PII", "pii"} },
		"whitespace-padded redactor":    func(s *Settings) { s.Redact = []string{" pii "} },
		"control char in redactor":      func(s *Settings) { s.Redact = []string{"pii\n"} },
		"redactor over 100 runes":       func(s *Settings) { s.Redact = []string{strings.Repeat("x", 101)} },
		"too many key selectors":        func(s *Settings) { s.KeyIDs = tooManyKeys },
		"non-canonical key uuid":        func(s *Settings) { s.KeyIDs = []string{"not-a-uuid"} },
		"short end user digest":         func(s *Settings) { s.EndUsers = []string{strings.Repeat("a", 63)} },
		"uppercase end user digest":     func(s *Settings) { s.EndUsers = []string{strings.Repeat("A", 64)} },
		"maxBytes below minimum":        func(s *Settings) { s.MaxBytes = minPolicySize - 1 },
		"maxBytes above maximum":        func(s *Settings) { s.MaxBytes = maxRecord + 1 },
	}

	for name, mutate := range invalidCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := validSettings()
			mutate(&s)
			if err := s.validate(); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

func TestSettingsMatchesSelectors(t *testing.T) {
	t.Parallel()
	s := Settings{
		ProjectID: strPtr("proj-a"),
		Route:     strPtr("route-a"),
		KeyIDs:    []string{"key-1", "key-2"},
		EndUsers:  []string{"user-1"},
	}
	if !s.matches("proj-a", "route-a", "key-2", "user-1") {
		t.Fatal("exact match rejected")
	}
	if s.matches("proj-b", "route-a", "key-1", "user-1") {
		t.Error("mismatched project accepted")
	}
	if s.matches("proj-a", "route-b", "key-1", "user-1") {
		t.Error("mismatched route accepted")
	}
	if s.matches("proj-a", "route-a", "key-3", "user-1") {
		t.Error("unlisted key accepted")
	}
	if s.matches("proj-a", "route-a", "key-1", "user-2") {
		t.Error("unlisted end user accepted")
	}
	wildcard := Settings{ProjectID: strPtr("proj-a")}
	if !wildcard.matches("proj-a", "any-route", "any-key", "any-user") {
		t.Error("unrestricted selectors did not match")
	}
}

func TestSampledIsDeterministicBoundedAndMonotonic(t *testing.T) {
	t.Parallel()
	zero := big.NewRat(0, 1)
	one := big.NewRat(1, 1)
	quarter := big.NewRat(1, 4)
	half := big.NewRat(1, 2)

	quarterHits, halfHits := 0, 0
	for i := range 500 {
		reqID := "req-" + strconv.Itoa(i)
		if sampled(zero, "policy-1", reqID) {
			t.Fatalf("ratio 0 sampled %s", reqID)
		}
		if !sampled(one, "policy-1", reqID) {
			t.Fatalf("ratio 1 skipped %s", reqID)
		}
		q1 := sampled(quarter, "policy-1", reqID)
		q2 := sampled(quarter, "policy-1", reqID)
		if q1 != q2 {
			t.Fatalf("sampled is not deterministic for %s", reqID)
		}
		h := sampled(half, "policy-1", reqID)
		if q1 && !h {
			t.Fatalf("sampling is not monotonic for %s: sampled at 0.25 but not 0.5", reqID)
		}
		if q1 {
			quarterHits++
		}
		if h {
			halfHits++
		}
	}
	if quarterHits == 0 || halfHits <= quarterHits || halfHits == 500 {
		t.Fatalf("unexpected sample counts: quarter=%d half=%d out of 500", quarterHits, halfHits)
	}
}

func TestInputOfFiltersAndClonesKnownInputFields(t *testing.T) {
	t.Parallel()
	orig := json.RawMessage(`[{"role":"user","content":"hi"}]`)
	doc := map[string]json.RawMessage{
		"messages": orig,
		"model":    json.RawMessage(`"route-a"`),
		"prompt":   nil,
	}
	got := inputOf(doc)
	if len(got) != 1 || string(got["messages"]) != string(orig) {
		t.Fatalf("inputOf = %v, want only messages", got)
	}
	orig[0] = 'X'
	if got["messages"][0] == 'X' {
		t.Fatal("inputOf aliased caller backing slice")
	}
	if key := objectKey("0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"); key != "captures/0192cf87d4ab7f2ea8b1c2d3e4f50607.jsonl" {
		t.Fatalf("objectKey = %q", key)
	}
}
