package codemode

import (
	"strings"
	"testing"
	"time"
)

func TestOutcomeRejectsUnboundedAndContradictoryMetadata(t *testing.T) {
	now := time.Now().UTC()
	status := func(n int) *int { return &n }
	for _, o := range []Outcome{
		{},
		{Origin: "upstream", Kind: "completed", UpstreamStatus: status(500), ObservedAt: now},
		{Origin: "upstream", Kind: "rejected", UpstreamStatus: status(200), ObservedAt: now},
		{Origin: strings.Repeat("x", 1000), Kind: "failed", ObservedAt: now},
	} {
		if err := o.Validate(); err == nil {
			t.Fatalf("invalid outcome accepted: %+v", o)
		}
	}
	for _, o := range []Outcome{
		{Origin: "upstream", Kind: "headers", UpstreamStatus: status(200), ObservedAt: now},
		{Origin: "upstream", Kind: "failed", UpstreamStatus: status(429), ObservedAt: now},
		{Origin: "gateway", Kind: "interrupted", ObservedAt: now},
		{Origin: "client", Kind: "canceled", ObservedAt: now},
	} {
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
