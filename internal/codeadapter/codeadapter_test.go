package codeadapter

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/connectors"
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

func TestCodingPlanAdaptersPlaceTheKeyInEachEndpointHeader(t *testing.T) {
	key := "vendor-key-0123456789abcdef0"
	secret, err := json.Marshal(connectors.GrantCredential{AccessToken: key})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		adapter codemode.Adapter
		profile string
		headers map[string]string
	}{
		{codemode.AdapterMiniMaxPlan, codeplans.MiniMaxProfile, map[string]string{"v1/messages": "Authorization", "v1/chat/completions": "Authorization"}},
		{codemode.AdapterKimiCode, codeplans.KimiProfile, map[string]string{"v1/messages": "X-Api-Key", "v1/chat/completions": "Authorization"}},
		{codemode.AdapterQwenTokenPlan, codeplans.QwenTokenPlanProfile, map[string]string{"v1/messages": "Authorization", "v1/chat/completions": "Authorization"}},
	} {
		vendor, ok := Lookup(test.adapter)
		if !ok {
			t.Fatalf("adapter %s missing", test.adapter)
		}
		for path, header := range test.headers {
			endpoint, _ := vendor.Endpoint(path)
			auth, err := vendor.Authorize(secret, test.profile, codeplans.Principal(test.profile, key),
				codemode.Dispatch{ClientPath: path, Adapter: test.adapter, Protocol: endpoint.Protocol}, time.Now())
			if err != nil {
				t.Fatalf("%s %s: %v", test.adapter, path, err)
			}
			want := key
			if header == "Authorization" {
				want = "Bearer " + key
			}
			if got := auth.Headers.Get(header); got != want || len(auth.Headers) != 1 {
				t.Fatalf("%s %s placed %v", test.adapter, path, auth.Headers)
			}
			if err := vendor.ValidateAuthorization(endpoint, auth, auth.Principal); err != nil {
				t.Fatalf("%s %s authorization invalid: %v", test.adapter, path, err)
			}
		}
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

func TestCodingPlanClientsUseOpenCodeCatalogProviders(t *testing.T) {
	for _, test := range []struct {
		adapter  codemode.Adapter
		provider string
	}{
		{codemode.AdapterMiniMaxPlan, "minimax-coding-plan"},
		{codemode.AdapterKimiCode, "kimi-code-plan-global"},
		{codemode.AdapterQwenTokenPlan, "alibaba-token-plan"},
	} {
		t.Run(string(test.adapter), func(t *testing.T) {
			vendor, ok := Lookup(test.adapter)
			if !ok {
				t.Fatalf("adapter %s missing", test.adapter)
			}
			client, ok := vendor.Client(ClientOpenCode)
			if !ok {
				t.Fatal("OpenCode client missing")
			}
			if client.Provider != test.provider {
				t.Fatalf("OpenCode provider %q cannot resolve models from %q (https://models.dev/api.json, reviewed 2026-10-27)", client.Provider, test.provider)
			}
		})
	}
}

func TestMiniMaxOpenCodeClientSendsMessages(t *testing.T) {
	vendor, ok := Lookup(codemode.AdapterMiniMaxPlan)
	if !ok {
		t.Fatal("MiniMax adapter missing")
	}
	if p, ok := vendor.Protocol(ClientOpenCode, "MiniMax-M3"); !ok || p != codemode.ProtocolMessages {
		t.Fatalf("OpenCode predicted to send %q, but minimax-coding-plan uses @ai-sdk/anthropic (https://models.dev/api.json, reviewed 2026-10-27)", p)
	}
	if !vendor.ServesModel(codemode.ProtocolChat, "MiniMax-M3") {
		t.Fatal("raw Chat Completions forwarding lost")
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
