package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
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
