package providers

import (
	"errors"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
)

// A grant's credential versions come from grant enrollment rather
// than a pasted secret, but it authorizes with one all the same.

// An operation in a native dialect is served through the
// vendor's preset profile.

func TestKindCapabilityOptionsIncludeProfileTuples(t *testing.T) {
	for _, mode := range []string{ModeUnary, ModeStreaming} {
		tuple := CapabilityInput{OperationGeneration, "native", mode}
		if !slices.Contains(capabilityOptionsForKind(KindOpenAICompatible), tuple) {
			t.Fatalf("compatible profiles' tuple is not published: %+v", tuple)
		}
		if slices.Contains(capabilityOptionsForKind(KindOpenAI), tuple) {
			t.Fatalf("OpenAI has no native generation profile but publishes %+v", tuple)
		}
	}
}

func TestKindCapabilityOptionsIncludeDecisions(t *testing.T) {
	for _, kind := range []string{KindOpenAI, KindOpenAICompatible} {
		for _, surface := range []string{SurfaceOpenAI, "native"} {
			tuple := CapabilityInput{"decision", surface, ModeUnary}
			if !slices.Contains(capabilityOptionsForKind(kind), tuple) {
				t.Fatalf("%s does not publish %+v", kind, tuple)
			}
			if slices.Contains(capabilityOptionsForKind(kind), CapabilityInput{"decision", surface, ModeStreaming}) {
				t.Fatalf("%s publishes streaming Decisions", kind)
			}
		}
	}
}

// A commitment is a positive decimal amount on a UTC calendar period; it is
// part of the stored provider configuration, so it flows through revisions and
// configuration export unchanged.
func TestCommitmentValidation(t *testing.T) {
	cfg := Configuration{Kind: KindOpenAI, AuthMode: "api_key"}
	cfg.Normalize()
	cfg.Options.Commitment = &routing.Commitment{Amount: "300.50", Period: "day"}
	if err := cfg.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, field, amount, period string
	}{
		{"zero", "configuration.options.commitment.amount", "0", "day"},
		{"negative", "configuration.options.commitment.amount", "-1", "day"},
		{"malformed", "configuration.options.commitment.amount", "1.2.3", "day"},
		{"empty amount", "configuration.options.commitment.amount", "", "day"},
		{"long", "configuration.options.commitment.amount", "1234567890123", "day"},
		{"unknown period", "configuration.options.commitment.period", "100", "week"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := cfg
			invalid.Options.Commitment = &routing.Commitment{Amount: tc.amount, Period: tc.period}
			if problem, ok := errors.AsType[*access.Problem](invalid.Validate(&egress.Policy{})); !ok || problem.Field != tc.field {
				t.Fatalf("%+v of %q/%q passed validation", problem, tc.amount, tc.period)
			}
		})
	}
}
