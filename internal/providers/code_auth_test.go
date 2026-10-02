package providers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

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
	cfg := runtime.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codexauth.ProfileID, ProfileRevision: "digest"}
	if !codeConfiguration(cfg) {
		t.Fatal("Codex grant profile refused")
	}
	cfg.Options.SemanticHeaders = map[string]string{"User-Agent": "invented"}
	if codeConfiguration(cfg) {
		t.Fatal("semantic header injection admitted")
	}
	cfg.Options.SemanticHeaders = nil
	cfg.Endpoint = "https://other.example"
	if codeConfiguration(cfg) {
		t.Fatal("unknown upstream admitted")
	}
	cfg.Endpoint = ""
	cfg.AuthMode = "api_key"
	if codeConfiguration(cfg) {
		t.Fatal("static credential admitted")
	}
}
