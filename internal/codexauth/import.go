package codexauth

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type CredentialImport struct {
	tokens   tokenResponse
	identity Identity
}

func ParseCredentialImport(input string, now time.Time) (CredentialImport, error) {
	document, err := oif.ParseJSON([]byte(input), oif.Limits{MaxBytes: 64 << 10, MaxDepth: 4, MaxNodes: 32})
	if err != nil || document.Root().Kind() != oif.Object {
		return CredentialImport{}, invalid()
	}
	for _, field := range document.Root().Members() {
		if !slices.Contains([]string{"auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh"}, field.Name) {
			return CredentialImport{}, invalid()
		}
		switch field.Name {
		case "OPENAI_API_KEY":
			if field.Value.Kind() != oif.Null {
				return CredentialImport{}, invalid()
			}
		case "auth_mode":
			if mode, ok := field.Value.Text(); !ok || mode != "chatgpt" {
				return CredentialImport{}, invalid()
			}
		case "last_refresh":
			value, ok := field.Value.Text()
			if _, err := time.Parse(time.RFC3339Nano, value); !ok || err != nil {
				return CredentialImport{}, invalid()
			}
		}
	}
	tokens, ok := document.Root().Lookup("tokens")
	if !ok || tokens.Kind() != oif.Object {
		return CredentialImport{}, invalid()
	}
	for _, field := range tokens.Members() {
		if !slices.Contains([]string{"access_token", "refresh_token", "id_token", "account_id"}, field.Name) || field.Value.Kind() != oif.String {
			return CredentialImport{}, invalid()
		}
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	}
	if json.Unmarshal(tokens.Bytes(), &raw) != nil || raw.RefreshToken == "" || len(raw.RefreshToken) > 16<<10 || strings.ContainsAny(raw.RefreshToken, "\r\n\x00") {
		return CredentialImport{}, invalid()
	}
	access, accessErr := tokenIdentity(raw.AccessToken)
	id, idErr := tokenIdentity(raw.IDToken)
	if accessErr != nil || idErr != nil || access.Principal() != id.Principal() || raw.AccountID != access.Account || access.ExpiresAt.After(now.AddDate(10, 0, 0)) || id.ExpiresAt.After(now.AddDate(10, 0, 0)) {
		return CredentialImport{}, invalid()
	}
	return CredentialImport{tokens: tokenResponse{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken, IDToken: raw.IDToken}, identity: access}, nil
}

func (c CredentialImport) RefreshRequest() abi.GrantRefresh {
	return abi.GrantRefresh{Profile: ProfileID, RefreshToken: c.tokens.RefreshToken, Facts: c.identity.Facts()}
}

func (c CredentialImport) Secrets() []string {
	return []string{c.tokens.AccessToken, c.tokens.RefreshToken, c.tokens.IDToken}
}

func (c CredentialImport) Matches(grant abi.Grant) bool {
	return grant.Principal == c.identity.Principal() && maps.Equal(grant.Facts, c.identity.Facts()) && grant.ExpiresIn > 0
}
