package codexauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var errToken = errors.New("Codex authorization lacks usable account identity or expiry")

type Identity struct {
	Account   string
	User      string
	ExpiresAt time.Time
}

func (i Identity) Principal() string {
	sum := sha256.Sum256([]byte(i.Account + "\x00" + i.User))
	return "codex:" + hex.EncodeToString(sum[:])
}

func (i Identity) Facts() map[string]string {
	return map[string]string{"account_id": i.Account, "user_id": i.User}
}

type claims struct {
	Expiry int64 `json:"exp"`
	Auth   struct {
		Account string `json:"chatgpt_account_id"`
		User    string `json:"chatgpt_user_id"`
		UserID  string `json:"user_id"`
		FedRAMP bool   `json:"chatgpt_account_is_fedramp"`
	} `json:"https://api.openai.com/auth"`
}

// TokenIdentity observes tokens obtained directly from the fixed TLS issuer,
// or their encrypted stored versions. It does not authenticate client JWTs.
func TokenIdentity(token string, now time.Time) (Identity, error) {
	var zero Identity
	if len(token) > 16<<10 || strings.ContainsAny(token, "\r\n\x00") {
		return zero, errToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return zero, errToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return zero, errToken
	}
	var c claims
	if err = json.Unmarshal(payload, &c); err != nil {
		return zero, errToken
	}
	if c.Auth.User == "" {
		c.Auth.User = c.Auth.UserID
	} else if c.Auth.UserID != "" && c.Auth.UserID != c.Auth.User {
		return zero, errToken
	}
	if !identifier(c.Auth.Account) || !identifier(c.Auth.User) || c.Auth.FedRAMP || c.Expiry <= now.Unix() || c.Expiry > now.AddDate(10, 0, 0).Unix() {
		return zero, errToken
	}
	return Identity{Account: c.Auth.Account, User: c.Auth.User, ExpiresAt: time.Unix(c.Expiry, 0)}, nil
}

func identifier(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '|') {
			return false
		}
	}
	return true
}
