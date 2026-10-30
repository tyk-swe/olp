package codeadapter

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/antigravityauth"
	"github.com/tyk-swe/olp/internal/claudeauth"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/connectors"
)

func TestValidateAcceptsShippedTable(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexIdentityRequiresSeparateAdapterPolicy(t *testing.T) {
	for _, adapter := range []codemode.Adapter{codemode.AdapterCodex, codemode.AdapterClaude, codemode.AdapterAntigravity, codemode.AdapterOpenCodeGo, codemode.AdapterZAICoding, codemode.AdapterMiniMaxPlan, codemode.AdapterKimiCode, codemode.AdapterQwenTokenPlan} {
		vendor, ok := Lookup(adapter)
		if !ok {
			t.Fatalf("adapter %s unavailable", adapter)
		}
		for _, enabled := range []bool{false, true} {
			route := codemode.Route{MessagesToCodex: true, ChatToAntigravity: true, CodexCLIIdentity: enabled}
			originator := vendor.CompatibilityOriginator(route)
			if vendor.Adapter == codemode.AdapterCodex && enabled {
				if originator != "codex_cli_rs" {
					t.Fatalf("opted-in Codex originator = %q", originator)
				}
				continue
			}
			if originator != "" {
				t.Fatalf("identity enabled for %s with policy %t", vendor.Adapter, enabled)
			}
		}
	}
}

func TestValidateRejectsInconsistentSharedPath(t *testing.T) {
	registry := slices.Clone(vendors)
	index := slices.IndexFunc(registry, func(vendor Vendor) bool { return vendor.Adapter == codemode.AdapterOpenCodeGo })
	other := registry[index]
	other.Adapter = "other"
	other.profiles = nil
	other.endpoints = slices.Clone(other.endpoints)
	other.endpoints[0].Operation = "messages"
	other.endpoints[0].Protocol = codemode.ProtocolMessages
	registry[index] = other
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

func TestRoutePolicySeparatesMessagesClientFromCodexTarget(t *testing.T) {
	adapter, ok := Lookup(codemode.AdapterCodex)
	if !ok {
		t.Fatal("Codex adapter missing")
	}
	native, ok := adapter.Endpoint("responses")
	if !ok {
		t.Fatal("native Responses endpoint missing")
	}
	for _, enabled := range []bool{false, true} {
		route := codemode.Route{MessagesToCodex: enabled}
		endpoint, reachable := RouteEndpoint(route, adapter, "v1/messages")
		if reachable != enabled {
			t.Fatalf("policy %t, Messages target reachable %t", enabled, reachable)
		}
		if enabled && (endpoint.Protocol != native.Protocol || endpoint.UpstreamPath != native.UpstreamPath || endpoint.CredentialHeader != native.CredentialHeader || endpoint.Observation != native.Observation || endpoint.Identity != native.Identity) {
			t.Fatalf("translated target changed native account contract, got %+v, native %+v", endpoint, native)
		}
		protocol, reachable := RouteClientProtocol(route, adapter, ClientClaudeCode, "native-fixture-model")
		if reachable != enabled || enabled && protocol != codemode.ProtocolMessages {
			t.Fatalf("policy %t, Claude client protocol %q, reachable %t", enabled, protocol, reachable)
		}
		if enabled && protocol == endpoint.Protocol {
			t.Fatal("Messages client protocol was replaced with upstream target protocol")
		}
		clients := RouteClients(route, adapter)
		if slices.Contains(clients, ClientClaudeCode) != enabled {
			t.Fatalf("policy %t, generated clients %v", enabled, clients)
		}
		for _, client := range adapter.Clients() {
			if !slices.Contains(clients, client) {
				t.Fatalf("policy %t removed native client %s", enabled, client)
			}
			want, offered := adapter.Protocol(client, "native-fixture-model")
			got, reachable := RouteClientProtocol(route, adapter, client, "native-fixture-model")
			if reachable != offered || got != want {
				t.Fatalf("policy %t changed native %s protocol from %q to %q", enabled, client, want, got)
			}
		}
		if _, ok := RouteClientProtocol(route, adapter, "unsupported-client", "native-fixture-model"); ok {
			t.Fatal("translation exposed an unsupported client")
		}
		if _, ok := RouteEndpoint(route, adapter, "unsupported-operation"); ok {
			t.Fatal("translation exposed an unsupported operation")
		}
	}
}

func TestMessagesTranslationPolicyPreservesOtherAdaptersNativeReachability(t *testing.T) {
	adapter, ok := Lookup(codemode.AdapterOpenCodeGo)
	if !ok {
		t.Fatal("account adapter missing")
	}
	route := codemode.Route{MessagesToCodex: true}
	if !slices.Equal(RouteClients(route, adapter), adapter.Clients()) {
		t.Fatal("Codex policy changed another adapter's generated clients")
	}
	for _, model := range []string{"minimax-fixture-model", "gpt-fixture-model"} {
		want, offered := adapter.Protocol(ClientClaudeCode, model)
		got, reachable := RouteClientProtocol(route, adapter, ClientClaudeCode, model)
		if reachable != offered || got != want {
			t.Fatalf("Codex policy changed another account's native reachability for %s, got %q %t, native %q %t", model, got, reachable, want, offered)
		}
	}
	native, ok := adapter.Endpoint("v1/messages")
	endpoint, reachable := RouteEndpoint(route, adapter, "v1/messages")
	if !ok || !reachable || endpoint.Protocol != native.Protocol || endpoint.UpstreamPath != native.UpstreamPath || endpoint.CredentialHeader != native.CredentialHeader {
		t.Fatalf("Codex policy replaced native Messages target, got %+v, native %+v", endpoint, native)
	}
}

func TestClaudeSubscriptionUsesVerifiedNativeMessagesAuthorization(t *testing.T) {
	vendor, ok := ForConnection("plugin", "grant", "claude-subscription")
	if !ok {
		t.Fatal("Claude subscription enrollment is unavailable")
	}
	secret := []byte(`{"access_token":"fixture-access","facts":{"account_uuid":"11111111-1111-4111-8111-111111111111","organization_uuid":"22222222-2222-4222-8222-222222222222"}}`)
	principal := "claude:46bfea7713358f59899a7e3efd31117f654890abb9752ca9063761118779aebe"
	dispatch := codemode.Dispatch{Adapter: "claude", ClientPath: "v1/messages", Protocol: codemode.ProtocolMessages}
	authorization, err := vendor.Authorize(secret, "claude-subscription", principal, dispatch, time.Now())
	if err != nil || authorization.Headers.Get("Authorization") != "Bearer fixture-access" || authorization.Headers.Get("Anthropic-Beta") != claudeauth.OAuthBeta || authorization.Principal != principal {
		t.Fatalf("verified authorization refused: %v", err)
	}
	endpoint, _ := vendor.Endpoint(dispatch.ClientPath)
	if err := vendor.ValidateAuthorization(endpoint, authorization, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := vendor.Authorize(secret, "claude-subscription", "another-principal", dispatch, time.Now()); err == nil {
		t.Fatal("another identity inherited the credential")
	}
}

func TestAntigravityRequiresPublishedChatTranslationPolicy(t *testing.T) {
	vendor, ok := Lookup("antigravity")
	if !ok {
		t.Fatal("Antigravity adapter is unavailable")
	}
	for _, enabled := range []bool{false, true} {
		route := codemode.Route{ChatToAntigravity: enabled}
		endpoint, served := RouteEndpoint(route, vendor, "v1/chat/completions")
		protocol, client := RouteClientProtocol(route, vendor, ClientOpenCode, "gemini-fixture")
		if served != enabled || client != enabled {
			t.Fatalf("translation policy %t, endpoint %t client %t", enabled, served, client)
		}
		if enabled && (endpoint.Protocol != codemode.ProtocolChat || protocol != codemode.ProtocolChat || endpoint.Supports(WebSocket)) {
			t.Fatal("Antigravity advertised an unsupported protocol or transport")
		}
		if _, served := RouteEndpoint(route, vendor, "v1/messages"); served {
			t.Fatal("Antigravity advertised Messages")
		}
		if vendor.ServesModel(codemode.ProtocolChat, "claude-fixture") {
			t.Fatal("Antigravity advertised an unqualified model family")
		}
	}
}

func TestAntigravityAuthorizationUsesVerifiedSubjectAndProject(t *testing.T) {
	vendor, ok := ForConnection("plugin", "grant", antigravityauth.ProfileID)
	if !ok {
		t.Fatal("Antigravity enrollment is unavailable")
	}
	secret := []byte(`{"access_token":"fixture-access","facts":{"subject":"verified-subject","project_id":"verified-project"}}`)
	principal := antigravityauth.Principal("verified-subject", "verified-project")
	dispatch := codemode.Dispatch{Adapter: codemode.AdapterAntigravity, ClientPath: "v1/chat/completions", Protocol: codemode.ProtocolChat}
	authorization, err := vendor.Authorize(secret, antigravityauth.ProfileID, principal, dispatch, time.Now())
	if err != nil || authorization.Principal != principal || authorization.ProjectID != "verified-project" || authorization.Headers.Get("Authorization") != "Bearer fixture-access" {
		t.Fatalf("verified authorization refused %v", err)
	}
	endpoint, _ := vendor.Endpoint(dispatch.ClientPath)
	if err := vendor.ValidateAuthorization(endpoint, authorization, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := vendor.Authorize(secret, antigravityauth.ProfileID, antigravityauth.Principal("another-subject", "verified-project"), dispatch, time.Now()); err == nil {
		t.Fatal("another subject inherited the credential")
	}
	if _, err := vendor.Authorize([]byte(`{"access_token":"fixture-access","facts":{"subject":"verified-subject"}}`), antigravityauth.ProfileID, principal, dispatch, time.Now()); err == nil {
		t.Fatal("unverified project became dispatch authority")
	}
}
