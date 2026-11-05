package codeadapter_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
)

func TestAllowanceSignalMarksMiniMaxProfilesUnqualified(t *testing.T) {
	for _, profile := range []string{codeplans.MiniMaxProfile, codeplans.MiniMaxCNProfile} {
		vendor, ok := codeadapter.ForConnection("plugin", "grant", profile)
		if !ok || vendor.Adapter != codemode.AdapterMiniMaxPlan {
			t.Fatalf("profile %q must resolve to the MiniMax adapter", profile)
		}
		signal, ok := codeadapter.AllowanceSignalForProfile("plugin", "grant", profile)
		if !ok || signal.Signal != codeadapter.AllowanceSignalRemainsUnqualified {
			t.Fatalf("profile %q signal = %+v ok=%v, want unqualified", profile, signal, ok)
		}
		if signal.QualifiedForActiveRead() {
			t.Fatalf("profile %q must not qualify for an active allowance read", profile)
		}
	}
}

func TestAllowanceSignalNamesCodexProvenAndOthersUnproven(t *testing.T) {
	codex, ok := codeadapter.AllowanceSignalForProfile("plugin", "grant", codexauth.ProfileID)
	if !ok || codex.Signal != codeadapter.AllowanceSignalProven || codex.QualifiedForActiveRead() {
		t.Fatalf("codex signal = %+v ok=%v, want passive proven and active unqualified", codex, ok)
	}
	glm, ok := codeadapter.AllowanceSignalForProfile("plugin", "grant", codeplans.ZAIProfile)
	if !ok || glm.Signal != codeadapter.AllowanceSignalNone || glm.QualifiedForActiveRead() {
		t.Fatalf("glm signal = %+v ok=%v, want none and unqualified", glm, ok)
	}
	if _, ok := codeadapter.AllowanceSignalForProfile("plugin", "grant", "unknown-profile"); ok {
		t.Fatal("unknown profile must have no signal entry")
	}
}
