package codeadapter

import (
	"errors"
	"strings"
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

func TestConnectionKindAndAuthModeAreThePluginGrantConstants(t *testing.T) {
	if kindPlugin != connectors.KindPlugin || authGrant != connectors.AuthGrant {
		t.Fatal("code accounts no longer name the plugin grant connection")
	}
	for _, v := range vendors {
		for _, profile := range v.Profiles {
			if !strings.Contains(SQL("c"), "WHEN '"+profile+"' THEN '"+string(v.Adapter)+"'") {
				t.Fatalf("SQL omits %s", profile)
			}
		}
	}
}

func TestDeriveIgnoresUnknownConnectionsAndRefusesMixedAdapters(t *testing.T) {
	codex := Connection{connectors.KindPlugin, connectors.AuthGrant, codexauth.ProfileID}
	zai := Connection{connectors.KindPlugin, connectors.AuthGrant, codeplans.ZAIProfile}
	bigmodel := Connection{connectors.KindPlugin, connectors.AuthGrant, codeplans.BigModelProfile}
	other := Connection{connectors.KindPlugin, connectors.AuthGrant, "reference-grant-chat"}
	for _, test := range []struct {
		connections []Connection
		want        codemode.Adapter
		err         error
	}{
		{nil, "", nil},
		{[]Connection{other}, "", nil},
		{[]Connection{codex, other}, codemode.AdapterCodex, nil},
		{[]Connection{zai, bigmodel}, codemode.AdapterZAICoding, nil},
		{[]Connection{zai, codex}, "", ErrMixed},
	} {
		got, err := Derive(test.connections)
		if got != test.want || !errors.Is(err, test.err) {
			t.Fatalf("%v: %q %v", test.connections, got, err)
		}
	}
}

func TestCredentialHeaderFollowsTheNativeClient(t *testing.T) {
	for _, test := range []struct {
		adapter  codemode.Adapter
		protocol codemode.Protocol
		want     string
	}{
		{codemode.AdapterCodex, codemode.ProtocolResponses, "Authorization"},
		{codemode.AdapterOpenCodeGo, codemode.ProtocolChat, "Authorization"},
		{codemode.AdapterOpenCodeGo, codemode.ProtocolResponses, "Authorization"},
		{codemode.AdapterOpenCodeGo, codemode.ProtocolMessages, "X-Api-Key"},
		{codemode.AdapterZAICoding, codemode.ProtocolMessages, "Authorization"},
		{codemode.AdapterZAICoding, codemode.ProtocolChat, "Authorization"},
	} {
		if got := CredentialHeader(test.adapter, test.protocol); got != test.want {
			t.Fatalf("%s %s: %s", test.adapter, test.protocol, got)
		}
	}
}
