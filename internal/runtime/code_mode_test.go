package runtime

import (
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
)

func TestCodeAdapterDerivesFromTheRevisionsFrozenConnections(t *testing.T) {
	grant := func(profile string) Configuration {
		return Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile}
	}
	s := &Snapshot{CodeConnections: map[string]Configuration{
		"codex:p1": grant(codexauth.ProfileID), "codex:p2": grant("reference-grant-chat"),
		"zai:p3": grant(codeplans.ZAIProfile), "zai:p4": grant(codeplans.BigModelProfile),
		"mixed:p5": grant(codeplans.OpenCodeGoProfile), "mixed:p6": grant(codexauth.ProfileID),
		"other:p7": grant("reference-grant-chat"),
	}}
	for revision, want := range map[string]codemode.Adapter{"codex": codemode.AdapterCodex, "zai": codemode.AdapterZAICoding, "mixed": "", "other": "", "absent": ""} {
		if got := s.CodeAdapter(codemode.Route{RevisionID: revision}); got != want {
			t.Fatalf("%s: %q, want %q", revision, got, want)
		}
	}
}
