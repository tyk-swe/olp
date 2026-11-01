package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func testToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestTokenIdentityBindsAccountAndUser(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	token := testToken(t, map[string]any{
		"exp": now.Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-1", "chatgpt_user_id": "user-1",
		},
	})
	identity, err := TokenIdentity(token, now)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Principal() != (Identity{Account: "acct-1", User: "user-1"}).Principal() {
		t.Fatal("principal does not follow account and user")
	}
	if identity.Principal() == (Identity{Account: "acct-1", User: "user-2"}).Principal() {
		t.Fatal("principal ignores user")
	}
}

func TestTokenIdentityRejectsExpiredAndFedRAMP(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for name, claims := range map[string]map[string]any{
		"expired": {
			"exp":                         now.Unix(),
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1", "chatgpt_user_id": "user-1"},
		},
		"fedramp": {
			"exp":                         now.Add(time.Hour).Unix(),
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1", "chatgpt_user_id": "user-1", "chatgpt_account_is_fedramp": true},
		},
		"mismatched user": {
			"exp":                         now.Add(time.Hour).Unix(),
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-1", "chatgpt_user_id": "user-1", "user_id": "user-2"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := TokenIdentity(testToken(t, claims), now); err == nil {
				t.Fatal("token accepted")
			}
		})
	}
}

func testImportDocument(t *testing.T, accessToken, idToken, refreshToken, account string) string {
	t.Helper()
	document, err := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "last_refresh": "2026-01-02T03:04:05Z",
		"tokens": map[string]string{"access_token": accessToken, "id_token": idToken, "refresh_token": refreshToken, "account_id": account}})
	if err != nil {
		t.Fatal(err)
	}
	return string(document)
}

func TestCredentialImportRequiresUnambiguousMatchingSubscriptionTokens(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	token := testToken(t, map[string]any{"exp": now.Add(-time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "fixture-account", "chatgpt_user_id": "fixture-user"}})
	other := testToken(t, map[string]any{"exp": now.Add(-time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "fixture-account", "chatgpt_user_id": "another-user"}})
	valid := testImportDocument(t, token, token, "fixture-refresh", "fixture-account")
	parsed, err := ParseCredentialImport(valid, now)
	if err != nil {
		t.Fatal(err)
	}
	request := parsed.RefreshRequest()
	if request.RefreshToken != "fixture-refresh" || request.Facts["account_id"] != "fixture-account" || request.Facts["user_id"] != "fixture-user" {
		t.Fatal("import did not constrain refresh to the pasted account and user")
	}
	for name, source := range map[string]string{
		"malformed JSON":            "{",
		"nonobject":                 "[]",
		"duplicate mode":            strings.Replace(valid, `"auth_mode":"chatgpt"`, `"auth_mode":"chatgpt","auth_mode":"chatgpt"`, 1),
		"API key conflict":          strings.Replace(valid, `"OPENAI_API_KEY":null`, `"OPENAI_API_KEY":"fixture-key"`, 1),
		"unsupported mode":          strings.Replace(valid, `"auth_mode":"chatgpt"`, `"auth_mode":"apikey"`, 1),
		"invalid refresh timestamp": strings.Replace(valid, "2026-01-02T03:04:05Z", "invalid-time", 1),
		"unexpected token field":    strings.Replace(valid, `"tokens":{`, `"tokens":{"unexpected":"fixture",`, 1),
		"account conflict":          testImportDocument(t, token, token, "fixture-refresh", "other-account"),
		"user conflict":             testImportDocument(t, token, other, "fixture-refresh", "fixture-account"),
		"missing refresh":           testImportDocument(t, token, token, "", "fixture-account"),
		"unsafe refresh":            testImportDocument(t, token, token, "fixture\nrefresh", "fixture-account"),
		"oversized refresh":         testImportDocument(t, token, token, strings.Repeat("r", (16<<10)+1), "fixture-account"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCredentialImport(source, now); err == nil {
				t.Fatal("unsupported or conflicting credentials were accepted")
			} else if strings.Contains(err.Error(), "fixture-refresh") || strings.Contains(err.Error(), token) {
				t.Fatal("import failure exposed credential material")
			}
		})
	}
}

func TestCredentialImportMatchesVerifiedNonrotatingRefreshAndRejectsChangedIdentity(t *testing.T) {
	now := time.Now()
	identity := Identity{Account: "fixture-account", User: "fixture-user"}
	local := testToken(t, map[string]any{"exp": now.Add(-time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": identity.Account, "chatgpt_user_id": identity.User}})
	imported, err := ParseCredentialImport(testImportDocument(t, local, local, "fixture-retained-refresh", identity.Account), now)
	if err != nil {
		t.Fatal(err)
	}
	fresh := testToken(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": identity.Account, "chatgpt_user_id": identity.User}})
	adapter := Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
		var input struct {
			GrantType    string `json:"grant_type"`
			RefreshToken string `json:"refresh_token"`
			ClientID     string `json:"client_id"`
		}
		if request.Method != "POST" || request.URL != Issuer+"/oauth/token" || json.Unmarshal(request.Body, &input) != nil || input.GrantType != "refresh_token" || input.RefreshToken != "fixture-retained-refresh" || input.ClientID != ClientID {
			t.Fatal("import refresh left the fixed issuer or changed the supplied credential")
		}
		body, err := json.Marshal(map[string]string{"access_token": fresh, "id_token": fresh})
		return abi.HTTPResponse{Status: 200, Body: body}, err
	}}
	grant, err := adapter.RefreshGrant(t.Context(), imported.RefreshRequest())
	if err != nil || grant.AccessToken != fresh || grant.RefreshToken != "" || !imported.Matches(grant) {
		t.Fatal("valid issuer refresh without token rotation could not complete import")
	}
	for name, changed := range map[string]abi.Grant{
		"principal": {Principal: "changed", Facts: identity.Facts(), ExpiresIn: grant.ExpiresIn},
		"facts":     {Principal: identity.Principal(), Facts: (Identity{Account: identity.Account, User: "another-user"}).Facts(), ExpiresIn: grant.ExpiresIn},
		"expiry":    {Principal: identity.Principal(), Facts: identity.Facts()},
	} {
		t.Run(name, func(t *testing.T) {
			if imported.Matches(changed) {
				t.Fatal("import accepted a changed identity or unusable expiry")
			}
		})
	}
}

func TestImportedRefreshRejectsUnsafeIssuerTokensAndChangedIdentity(t *testing.T) {
	now := time.Now()
	identity := Identity{Account: "fixture-account", User: "fixture-user"}
	valid := testToken(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": identity.Account, "chatgpt_user_id": identity.User}})
	changed := testToken(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "changed-account", "chatgpt_user_id": identity.User}})
	excessive := testToken(t, map[string]any{"exp": now.AddDate(11, 0, 0).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": identity.Account, "chatgpt_user_id": identity.User}})
	for name, response := range map[string]map[string]string{
		"empty access":         {"access_token": "", "refresh_token": "fixture-secret"},
		"unsafe access":        {"access_token": valid + "\r\n", "refresh_token": "fixture-secret"},
		"oversized refresh":    {"access_token": valid, "refresh_token": strings.Repeat("r", (16<<10)+1)},
		"excessive expiry":     {"access_token": excessive, "refresh_token": "fixture-secret"},
		"changed account":      {"access_token": changed, "refresh_token": "fixture-secret"},
		"conflicting ID token": {"access_token": valid, "id_token": changed, "refresh_token": "fixture-secret"},
	} {
		t.Run(name, func(t *testing.T) {
			adapter := Adapter{Fetch: func(context.Context, abi.HTTPRequest) (abi.HTTPResponse, error) {
				body, err := json.Marshal(response)
				return abi.HTTPResponse{Status: 200, Body: body}, err
			}}
			if _, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: ProfileID, RefreshToken: "fixture-imported-refresh", Facts: identity.Facts()}); err == nil {
				t.Fatal("issuer response with invalid token material or identity was accepted")
			} else if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), valid) {
				t.Fatal("issuer rejection exposed tokens")
			}
		})
	}
}
