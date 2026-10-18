package export

import (
	"testing"
)

func TestCaptureValidatesRedactorNamesAndBounds(t *testing.T) {
	p := CapturePolicy{Enabled: true, SampleRatio: "0.05", Include: []string{"input", "output"}, MaxBytes: 262144}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Redact = []string{"builtin.secrets"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Redact = []string{"PII", "pii"}
	if err := p.Validate(); err == nil {
		t.Fatal("duplicate redactors accepted")
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
