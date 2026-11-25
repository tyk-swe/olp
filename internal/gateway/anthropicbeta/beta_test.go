package anthropicbeta

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func TestCheckAndForwardMergesPublishedAndCallerBetas(t *testing.T) {
	t.Parallel()
	var profile connectors.Profile
	for _, candidate := range connectors.Profiles() {
		if candidate.ID == "anthropic-messages" {
			profile = candidate
			break
		}
	}
	if profile.ID == "" {
		t.Fatal("anthropic-messages built-in profile not found")
	}
	cfg := connectors.Config{
		Kind:            connectors.KindAnthropic,
		ProfileID:       profile.ID,
		ProfileRevision: profile.Revision,
		SemanticHeaders: map[string]string{"Anthropic-Beta": "prompt-caching-2024-07-31, shared-beta"},
	}
	caller := http.Header{"Anthropic-Beta": {"shared-beta", "token-counting-2024-11-01"}}
	if err := Check(false, wire.Anthropic, caller, []connectors.Config{cfg}); err != nil {
		t.Fatalf("Check returned unexpected error: %v", err)
	}
	outHeader := http.Header{}
	updated := Forward(outHeader, wire.Anthropic, wire.Anthropic, caller, cfg)
	got, ok := updated.SemanticHeader("Anthropic-Beta")
	if !ok || got != "prompt-caching-2024-07-31,shared-beta,token-counting-2024-11-01" {
		t.Fatalf("merged beta = %q (%v)", got, ok)
	}

	tooLong := http.Header{"Anthropic-Beta": {strings.Repeat("a", maxBetaBytes+1)}}
	if err := Check(false, wire.Anthropic, tooLong, []connectors.Config{cfg}); err == nil {
		t.Fatal("expected Check to reject oversized Anthropic-Beta header")
	}
}
