package codeadapter

import (
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
)

func TestEveryProfileBelongsToOneAdapterAtItsManifestAddress(t *testing.T) {
	want := map[string]codemode.Adapter{codexauth.ProfileID: codemode.AdapterCodex, codeplans.OpenCodeGoProfile: codemode.AdapterOpenCodeGo, codeplans.ZAIProfile: codemode.AdapterZAICoding, codeplans.BigModelProfile: codemode.AdapterZAICoding}
	for profile, adapter := range want {
		v, ok := ForConnection(connectors.KindPlugin, connectors.AuthGrant, profile)
		if !ok || v.Adapter != adapter || len(v.Clients) == 0 || len(v.Protocols) == 0 {
			t.Fatalf("%s: %+v", profile, v)
		}
		if address, ok := v.Address(profile); !ok || address == "" {
			t.Fatalf("%s has no hosting address", profile)
		}
		if _, ok := ForConnection(connectors.KindPlugin, "static_credential", profile); ok {
			t.Fatalf("%s served without a grant", profile)
		}
	}
	if v, _ := Lookup(codemode.AdapterCodex); v.KeyGrant || !v.Serves(codemode.ProtocolResponses) || v.Serves(codemode.ProtocolMessages) {
		t.Fatal("Codex row changed")
	}
	if v, _ := Lookup(codemode.AdapterZAICoding); !v.KeyGrant || v.Serves(codemode.ProtocolResponses) || v.Clients[0] != ClientClaudeCode {
		t.Fatal("Z.ai row must serve messages and chat, defaulting to Claude Code")
	}
	if v, _ := Lookup(codemode.AdapterOpenCodeGo); v.Clients[0] != ClientOpenCode || !v.Supports(ClientClaudeCode) || v.Supports(ClientCodex) {
		t.Fatal("OpenCode Go row must default to OpenCode")
	}
}

// Codex's paths are the route's root, which no other family serves.
