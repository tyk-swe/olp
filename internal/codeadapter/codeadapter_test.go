package codeadapter

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
)

func TestEveryProfileBelongsToOneAdapterAtItsManifestAddress(t *testing.T) {
	want := map[string]codemode.Adapter{codexauth.ProfileID: codemode.AdapterCodex, codeplans.OpenCodeGoProfile: codemode.AdapterOpenCodeGo, codeplans.ZAIProfile: codemode.AdapterZAICoding, codeplans.BigModelProfile: codemode.AdapterZAICoding}
	for profile, adapter := range want {
		v, ok := ForConnection(connectors.KindPlugin, connectors.AuthGrant, profile)
		if !ok || v.Adapter != adapter || len(v.Clients()) == 0 || len(v.Endpoints()) == 0 {
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
	if v, _ := Lookup(codemode.AdapterZAICoding); !v.KeyGrant || v.Serves(codemode.ProtocolResponses) || v.Clients()[0] != ClientClaudeCode {
		t.Fatal("Z.ai row must serve messages and chat, defaulting to Claude Code")
	}
	if v, _ := Lookup(codemode.AdapterOpenCodeGo); v.Clients()[0] != ClientOpenCode || !v.Supports(ClientClaudeCode) || v.Supports(ClientCodex) {
		t.Fatal("OpenCode Go row must default to OpenCode")
	}
}

func TestEndpointCapabilitiesAcrossProfiles(t *testing.T) {
	for _, test := range []struct {
		profile, path, upstream, header string
		protocol                        codemode.Protocol
		identity                        IdentityPolicy
		observation                     ObservationPolicy
		websocket                       bool
	}{
		{codexauth.ProfileID, "responses", "responses", "Authorization", codemode.ProtocolResponses, IdentityCodex, ObserveCodex, true},
		{codexauth.ProfileID, "responses/compact", "responses/compact", "Authorization", codemode.ProtocolResponses, IdentityCodex, ObserveCodex, false},
		{codeplans.OpenCodeGoProfile, "v1/responses", "responses", "Authorization", codemode.ProtocolResponses, IdentityClient, ObserveUsage, false},
		{codeplans.OpenCodeGoProfile, "v1/messages", "messages", "X-Api-Key", codemode.ProtocolMessages, IdentityClient, ObserveUsage, false},
		{codeplans.OpenCodeGoProfile, "v1/chat/completions", "chat/completions", "Authorization", codemode.ProtocolChat, IdentityClient, ObserveUsage, false},
		{codeplans.ZAIProfile, "v1/messages", "anthropic/v1/messages", "Authorization", codemode.ProtocolMessages, IdentityClient, ObserveUsage, false},
		{codeplans.ZAIProfile, "v1/chat/completions", "coding/paas/v4/chat/completions", "Authorization", codemode.ProtocolChat, IdentityClient, ObserveUsage, false},
		{codeplans.BigModelProfile, "v1/messages", "anthropic/v1/messages", "Authorization", codemode.ProtocolMessages, IdentityClient, ObserveUsage, false},
		{codeplans.BigModelProfile, "v1/chat/completions", "coding/paas/v4/chat/completions", "Authorization", codemode.ProtocolChat, IdentityClient, ObserveUsage, false},
	} {
		t.Run(test.profile+"/"+test.path, func(t *testing.T) {
			v, _ := ForConnection(connectors.KindPlugin, connectors.AuthGrant, test.profile)
			e, ok := v.Endpoint(test.path)
			if !ok || e.UpstreamPath != test.upstream || e.CredentialHeader != test.header || e.Protocol != test.protocol || e.Identity != test.identity || e.Observation != test.observation || !e.Supports(HTTP) || e.Supports(WebSocket) != test.websocket || e.Supports(0) {
				t.Fatalf("endpoint: %+v", e)
			}
		})
	}
}

func TestEndpointConsistencyRejectsAmbiguousRequests(t *testing.T) {
	if err := validate(vendors); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"protocol", "identity", "operation"} {
		t.Run(change, func(t *testing.T) {
			registry := slices.Clone(vendors)
			registry[1].endpoints = slices.Clone(registry[1].endpoints)
			e := &registry[1].endpoints[2]
			e.ClientPath = "responses"
			e.Identity = IdentityCodex
			switch change {
			case "protocol":
				e.Protocol, e.Operation, e.Identity = codemode.ProtocolChat, "chat/completions", IdentityClient
			case "identity":
				e.Identity = IdentityClient
			case "operation":
				e.Operation = "responses/compact"
			}
			if err := validate(registry); err == nil {
				t.Fatal("ambiguous client path accepted")
			}
		})
	}
}

func TestModelProtocolRestrictions(t *testing.T) {
	v, _ := Lookup(codemode.AdapterOpenCodeGo)
	for _, test := range []struct {
		model    string
		protocol codemode.Protocol
	}{
		{"MiniMax-M3", codemode.ProtocolMessages}, {"qwen-4", codemode.ProtocolMessages},
		{"GPT-5.5", codemode.ProtocolResponses}, {"grok-5", codemode.ProtocolResponses},
		{"glm-5.3", codemode.ProtocolChat},
	} {
		for _, protocol := range []codemode.Protocol{codemode.ProtocolResponses, codemode.ProtocolMessages, codemode.ProtocolChat, "unknown"} {
			if v.ServesModel(protocol, test.model) != (protocol == test.protocol) {
				t.Fatalf("%s on %s", test.model, protocol)
			}
		}
		if protocol, ok := v.Protocol(ClientOpenCode, test.model); !ok || protocol != test.protocol {
			t.Fatalf("OpenCode %s: %s %v", test.model, protocol, ok)
		}
		if v.Reaches(ClientClaudeCode, test.model) != (test.protocol == codemode.ProtocolMessages) {
			t.Fatalf("Claude Code reaches %s", test.model)
		}
	}
}

func TestCredentialConstructionUsesDeclaredEndpoint(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, profile := range []string{codexauth.ProfileID, codeplans.OpenCodeGoProfile, codeplans.ZAIProfile, codeplans.BigModelProfile} {
		t.Run(profile, func(t *testing.T) {
			v, _ := ForConnection(connectors.KindPlugin, connectors.AuthGrant, profile)
			key := "fixture-coding-plan-key"
			principal := codeplans.Principal(profile, key)
			credential := connectors.GrantCredential{AccessToken: key}
			if !v.KeyGrant {
				payload := fmt.Sprintf(`{"exp":%d,"https://api.openai.com/auth":{"chatgpt_account_id":"account","chatgpt_user_id":"user"}}`, now.Add(time.Hour).Unix())
				credential.AccessToken = "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
				identity, err := codexauth.TokenIdentity(credential.AccessToken, now)
				if err != nil {
					t.Fatal(err)
				}
				principal, credential.Facts = identity.Principal(), identity.Facts()
			}
			secret, _ := json.Marshal(credential)
			for _, e := range v.Endpoints() {
				dispatch := codemode.Dispatch{Adapter: v.Adapter, ClientPath: e.ClientPath, Protocol: e.Protocol}
				auth, err := v.Authorize(secret, profile, principal, dispatch, now)
				if err != nil {
					t.Fatal(err)
				}
				want := credential.AccessToken
				if e.CredentialHeader == "Authorization" {
					want = "Bearer " + want
				}
				if auth.Headers.Get(e.CredentialHeader) != want {
					t.Fatalf("%s: %v", e.ClientPath, auth.Headers)
				}
				if err := v.ValidateAuthorization(e, auth, principal); err != nil {
					t.Fatal(err)
				}
				if _, err := v.Authorize(secret, profile, "different-principal", dispatch, now); err == nil {
					t.Fatal("principal mismatch accepted")
				}
				dispatch.Protocol = "unknown"
				if _, err := v.Authorize(secret, profile, principal, dispatch, now); err == nil {
					t.Fatal("wrong endpoint protocol accepted")
				}
				dispatch.Protocol, dispatch.ClientPath = e.Protocol, "unserved"
				if _, err := v.Authorize(secret, profile, principal, dispatch, now); err == nil {
					t.Fatal("unknown client path accepted")
				}
				if _, err := v.Authorize([]byte(`{}`), profile, principal, codemode.Dispatch{Adapter: v.Adapter, ClientPath: e.ClientPath, Protocol: e.Protocol}, now); err == nil {
					t.Fatal("missing credential accepted")
				}
			}
			if !v.KeyGrant {
				e := v.Endpoints()[0]
				_, err := v.Authorize(secret, profile, principal, codemode.Dispatch{Adapter: v.Adapter, ClientPath: e.ClientPath, Protocol: e.Protocol}, now.Add(2*time.Hour))
				if refusal, ok := errors.AsType[*codemode.Refusal](err); !ok || refusal.Code != "code_principal_mismatch" {
					t.Fatalf("expired token: %v", err)
				}
			}
		})
	}
}
