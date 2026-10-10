package export

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCaptureSamplingUsesExactDeterministicRatio(t *testing.T) {
	p := CapturePolicy{ID: "policy", Enabled: true, Include: []string{"input"}, MaxBytes: 262144}
	for i := range 1000 {
		id := fmt.Sprintf("request-%d", i)
		p.SampleRatio = "0"
		if p.Sample(id) {
			t.Fatal("zero ratio sampled")
		}
		p.SampleRatio = "1"
		if !p.Sample(id) {
			t.Fatal("one ratio did not sample")
		}
		p.SampleRatio = "0.5"
		digest := sha256.Sum256([]byte("policy|" + id))
		if p.Sample(id) != (digest[0] < 128) || p.Sample(id) != p.Sample(id) {
			t.Fatal("half-ratio comparison or determinism failed")
		}
	}
	for _, ratio := range []string{"-1", "2", "NaN", "1e-2", "0.1234567890", ".5"} {
		p.SampleRatio = ratio
		if p.Sample("request") || p.Validate() == nil {
			t.Fatalf("accepted invalid ratio %q", ratio)
		}
	}
	p.SampleRatio, p.Enabled = "1", false
	if p.Sample("request") {
		t.Fatal("disabled capture sampled")
	}
}

func TestCaptureRejectsUnavailableRedactionAndUnboundedPolicies(t *testing.T) {
	p := CapturePolicy{Enabled: true, SampleRatio: "0.05", Include: []string{"input", "output"}, MaxBytes: 262144}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Redact = []string{"builtin.secrets"}
	if err := p.Validate(); err == nil {
		t.Fatal("unavailable redaction silently accepted")
	}
	p.Redact, p.MaxBytes = nil, 1048577
	if err := p.Validate(); err == nil {
		t.Fatal("unbounded capture accepted")
	}
	p.MaxBytes, p.Include = 262144, []string{"input", "input"}
	if err := p.Validate(); err == nil {
		t.Fatal("duplicate include accepted")
	}
}

func TestCaptureInputCopiesOnlyContentFields(t *testing.T) {
	source := map[string]json.RawMessage{
		"messages": json.RawMessage(`[{"role":"user","content":"sentinel"}]`),
		"api_key":  json.RawMessage(`"credential"`),
		"model":    json.RawMessage(`"route"`),
	}
	input := CaptureInput(source)
	data, err := json.Marshal(input)
	if err != nil || strings.Contains(string(data), "credential") || strings.Contains(string(data), "route") {
		t.Fatalf("input=%s error=%v", data, err)
	}
	source["messages"][0] = 'x'
	if input["messages"][0] != '[' {
		t.Fatal("capture input aliased request memory")
	}
}

func TestCapturePolicyMatchesScopedSamplingDimensions(t *testing.T) {
	project, route := "project", "route"
	p := CapturePolicy{ProjectID: &project, Route: &route, KeyIDs: []string{"key"}, EndUsers: []string{"digest"}}
	for _, test := range []struct {
		project, route, key, digest string
		want                        bool
	}{
		{"project", "route", "key", "digest", true},
		{"other", "route", "key", "digest", false},
		{"project", "other", "key", "digest", false},
		{"project", "route", "other", "digest", false},
		{"project", "route", "key", "other", false},
	} {
		if got := p.Matches(test.project, test.route, test.key, test.digest); got != test.want {
			t.Fatalf("match %+v = %t", test, got)
		}
	}
}
