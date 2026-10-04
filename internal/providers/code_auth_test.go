package providers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestCodeAuthorizationUsesCurrentTokenAndObservedAccountOnly(t *testing.T) {
	now := time.Now()
	principal := (codexauth.Identity{Account: "account-a", User: "user-a"}).Principal()
	for _, account := range []string{"account-a", "account-b"} {
		payload := fmt.Sprintf(`{"exp":%d,"https://api.openai.com/auth":{"chatgpt_account_id":%q,"user_id":"user-a"}}`, now.Add(time.Hour).Unix(), account)
		token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".fixture-signature"
		secret, _ := json.Marshal(connectors.GrantCredential{AccessToken: token, Facts: map[string]string{"account_id": account, "user_id": "user-a"}})
		auth, err := authorizeCodex(secret, principal, now)
		if account != "account-a" {
			if err == nil || len(auth.Headers) != 0 {
				t.Fatal("authorization switched principal")
			}
			continue
		}
		if err != nil || auth.Principal != principal || len(auth.Headers) != 2 || auth.Headers.Get("Authorization") != "Bearer "+token || auth.Headers.Get("ChatGPT-Account-ID") != account {
			t.Fatalf("authorization did not use issued account credentials: %v", err)
		}
		if _, err := authorizeCodex(secret, principal, now.Add(2*time.Hour)); err == nil {
			t.Fatal("expired access token was authorized")
		}
		secret, _ = json.Marshal(connectors.GrantCredential{AccessToken: token, Facts: map[string]string{"account_id": "wrong-account", "user_id": "user-a"}})
		if _, err := authorizeCodex(secret, principal, now); err == nil {
			t.Fatal("credential facts overrode observed identity")
		}
	}
}

func TestCodeAuthorizerRefusesOrdinaryAndTransformingConfigurations(t *testing.T) {
	codex, _ := codeadapter.Lookup(codemode.AdapterCodex)
	cfg := runtime.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codexauth.ProfileID, ProfileRevision: "digest"}
	if !codeConfiguration(cfg, codex) {
		t.Fatal("Codex grant profile refused")
	}
	cfg.Options.SemanticHeaders = map[string]string{"User-Agent": "invented"}
	if codeConfiguration(cfg, codex) {
		t.Fatal("semantic header injection admitted")
	}
	cfg.Options.SemanticHeaders = nil
	cfg.Endpoint = "https://other.example"
	if codeConfiguration(cfg, codex) {
		t.Fatal("unknown upstream admitted")
	}
	cfg.Endpoint = ""
	cfg.AuthMode = "api_key"
	if codeConfiguration(cfg, codex) {
		t.Fatal("static credential admitted")
	}
}

func TestCodeAuthorizerPinsEachCodingPlanProfileToItsOwnUpstream(t *testing.T) {
	zai, _ := codeadapter.Lookup(codemode.AdapterZAICoding)
	for profile, endpoint := range map[string]string{codeplans.ZAIProfile: codeplans.ZAIUpstream, codeplans.BigModelProfile: codeplans.BigModelUpstream} {
		cfg := runtime.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile, ProfileRevision: "digest", Endpoint: endpoint}
		if !codeConfiguration(cfg, zai) {
			t.Fatalf("%s refused at its own upstream", profile)
		}
		cfg.Endpoint = codeplans.OpenCodeGoUpstream
		if codeConfiguration(cfg, zai) {
			t.Fatalf("%s admitted at another upstream", profile)
		}
	}
	codex, _ := codeadapter.Lookup(codemode.AdapterCodex)
	if codeConfiguration(runtime.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codeplans.ZAIProfile, ProfileRevision: "digest"}, codex) {
		t.Fatal("a coding-plan profile passed as Codex")
	}
}

func TestKeyAuthorizationPlacesOnlyTheEnrolledKey(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef.AbCdEfGhIjKlMnOp"
	principal := codeplans.Principal(codeplans.OpenCodeGoProfile, key)
	secret, _ := json.Marshal(connectors.GrantCredential{AccessToken: key})
	auth, err := authorizeKey(secret, codeplans.OpenCodeGoProfile, principal, "Authorization")
	if err != nil || auth.Principal != principal || len(auth.Headers) != 1 || auth.Headers.Get("Authorization") != "Bearer "+key {
		t.Fatalf("bearer authorization: %+v %v", auth, err)
	}
	auth, err = authorizeKey(secret, codeplans.OpenCodeGoProfile, principal, "X-Api-Key")
	if err != nil || len(auth.Headers) != 1 || auth.Headers.Get("X-Api-Key") != key {
		t.Fatalf("x-api-key authorization: %+v %v", auth, err)
	}
	if _, err := authorizeKey(secret, codeplans.ZAIProfile, principal, "Authorization"); err == nil {
		t.Fatal("key authorized under another profile's principal")
	}
	for _, credential := range []connectors.GrantCredential{{AccessToken: key + "x"}, {AccessToken: key, Facts: map[string]string{"account": "x"}}, {AccessToken: "short"}, {AccessToken: key[:20] + "\r\n" + key[20:]}} {
		secret, _ := json.Marshal(credential)
		if auth, err := authorizeKey(secret, codeplans.OpenCodeGoProfile, principal, "Authorization"); err == nil || len(auth.Headers) != 0 {
			t.Fatalf("credential %+v authorized", credential)
		}
	}
}
