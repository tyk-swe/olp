package codeadapter

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestValidateAcceptsShippedTable(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsInconsistentSharedPath(t *testing.T) {
	registry := slices.Clone(vendors)
	other := registry[1]
	other.Adapter = "other"
	other.profiles = nil
	other.endpoints = slices.Clone(other.endpoints)
	other.endpoints[0].Operation = "messages"
	other.endpoints[0].Protocol = codemode.ProtocolMessages
	registry[1] = other
	if err := validate(registry); err == nil {
		t.Fatal("shared client path with different semantics accepted")
	}
}

func TestValidateRejectsDuplicateAdapter(t *testing.T) {
	registry := slices.Clone(vendors)
	registry = append(registry, registry[0])
	if err := validate(registry); err == nil {
		t.Fatal("duplicate adapter accepted")
	}
}

func TestForConnectionRequiresGrantPluginProfile(t *testing.T) {
	vendor, ok := Lookup(codemode.AdapterZAICoding)
	if !ok || len(vendor.profiles) == 0 {
		t.Fatal("GLM adapter missing")
	}
	if _, ok := ForConnection(kindPlugin, authGrant, vendor.profiles[0]); !ok {
		t.Fatal("grant plugin profile not resolved")
	}
	if _, ok := ForConnection("api_key", authGrant, vendor.profiles[0]); ok {
		t.Fatal("non-plugin connection resolved to a code adapter")
	}
	if _, ok := ForConnection(kindPlugin, "api_key", vendor.profiles[0]); ok {
		t.Fatal("non-grant connection resolved to a code adapter")
	}
}

func TestOpenCodeGoServesEachModelOnOneProtocol(t *testing.T) {
	vendor, ok := Lookup(codemode.AdapterOpenCodeGo)
	if !ok {
		t.Fatal("OpenCode Go adapter missing")
	}
	for model, protocol := range map[string]codemode.Protocol{
		"MiniMax-M3":  codemode.ProtocolMessages,
		"qwen3-coder": codemode.ProtocolMessages,
		"gpt-5.5":     codemode.ProtocolResponses,
		"grok-4":      codemode.ProtocolResponses,
		"glm-5.3":     codemode.ProtocolChat,
	} {
		if !vendor.ServesModel(protocol, model) {
			t.Fatalf("%s not served on %s", model, protocol)
		}
		for _, other := range []codemode.Protocol{codemode.ProtocolChat, codemode.ProtocolMessages, codemode.ProtocolResponses} {
			if other != protocol && vendor.ServesModel(other, model) {
				t.Fatalf("%s also served on %s", model, other)
			}
		}
	}
	if vendor.Reaches(ClientClaudeCode, "gpt-5.5") {
		t.Fatal("Claude Code reached a Responses-only model")
	}
	if !vendor.Reaches(ClientClaudeCode, "minimax-m3") {
		t.Fatal("Claude Code did not reach a Messages model")
	}
}
