package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func buildToken(t *testing.T, payload any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestTokenIdentityExtractsAccountUserAndPrincipal(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)

	token := buildToken(t, map[string]any{
		"exp": exp.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_123",
			"chatgpt_user_id":    "user_456",
		},
	})
	id, err := TokenIdentity(token, now)
	if err != nil {
		t.Fatalf("TokenIdentity returned error: %v", err)
	}
	if id.Account != "acct_123" || id.User != "user_456" || !id.ExpiresAt.Equal(exp) {
		t.Fatalf("unexpected identity: %+v", id)
	}
	facts := id.Facts()
	if facts["account_id"] != "acct_123" || facts["user_id"] != "user_456" {
		t.Fatalf("unexpected facts: %v", facts)
	}

	fallbackToken := buildToken(t, map[string]any{
		"exp": exp.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_123",
			"user_id":            "user_fallback",
		},
	})
	fallbackID, err := TokenIdentity(fallbackToken, now)
	if err != nil || fallbackID.User != "user_fallback" {
		t.Fatalf("fallback user_id = %+v err=%v", fallbackID, err)
	}

	p1 := (Identity{Account: "ab", User: "c"}).Principal()
	p2 := (Identity{Account: "a", User: "bc"}).Principal()
	if !strings.HasPrefix(p1, "codex:") || p1 == p2 {
		t.Fatalf("principals did not separate account and user boundaries: %q vs %q", p1, p2)
	}
}

func TestTokenIdentityRejectsInvalidExpiredAndFedRAMPTokens(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	validExp := now.Add(time.Hour).Unix()

	cases := map[string]string{
		"expired at now": buildToken(t, map[string]any{
			"exp":                         now.Unix(),
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct", "chatgpt_user_id": "user"},
		}),
		"expiry over 10 years away": buildToken(t, map[string]any{
			"exp":                         now.AddDate(10, 0, 1).Unix(),
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct", "chatgpt_user_id": "user"},
		}),
		"fedramp account": buildToken(t, map[string]any{
			"exp": validExp,
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id":         "acct",
				"chatgpt_user_id":            "user",
				"chatgpt_account_is_fedramp": true,
			},
		}),
		"conflicting user ids": buildToken(t, map[string]any{
			"exp": validExp,
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "acct",
				"chatgpt_user_id":    "user-a",
				"user_id":            "user-b",
			},
		}),
		"invalid account characters": buildToken(t, map[string]any{
			"exp":                         validExp,
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct/bad", "chatgpt_user_id": "user"},
		}),
		"overlong user identifier": buildToken(t, map[string]any{
			"exp":                         validExp,
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct", "chatgpt_user_id": strings.Repeat("u", 257)},
		}),
		"missing signature segment": "header.payload.",
		"two segments only":         "header.payload",
		"contains newline":          "a.b.c\n",
		"oversized token":           "a." + strings.Repeat("A", 16<<10) + ".c",
		"invalid base64 payload":    "a.!!!.c",
		"non-json payload":          "a." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".c",
	}

	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := TokenIdentity(token, now); !errors.Is(err, errToken) {
				t.Fatalf("TokenIdentity = %v, want %v", err, errToken)
			}
		})
	}
}
