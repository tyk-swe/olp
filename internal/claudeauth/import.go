package claudeauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type CredentialImport struct {
	accessToken  string
	refreshToken string
}

func ParseCredentialImport(input string, now time.Time) (CredentialImport, error) {
	document, err := oif.ParseJSON([]byte(input), oif.Limits{MaxBytes: 64 << 10, MaxDepth: 4, MaxNodes: 64})
	if err != nil || document.Root().Kind() != oif.Object || len(document.Root().Members()) != 1 {
		return CredentialImport{}, invalid()
	}
	entry, ok := document.Root().Lookup("claudeAiOauth")
	if !ok || entry.Kind() != oif.Object {
		return CredentialImport{}, invalid()
	}
	for _, field := range entry.Members() {
		if !slices.Contains([]string{"accessToken", "refreshToken", "expiresAt", "refreshTokenExpiresAt", "scopes", "subscriptionType", "subscriptionTypeSource", "subscriptionTypeVerifiedAt", "rateLimitTier", "clientId"}, field.Name) {
			return CredentialImport{}, invalid()
		}
	}
	var stored struct {
		AccessToken                string   `json:"accessToken"`
		RefreshToken               string   `json:"refreshToken"`
		ExpiresAt                  int64    `json:"expiresAt"`
		RefreshTokenExpiresAt      *int64   `json:"refreshTokenExpiresAt"`
		Scopes                     []string `json:"scopes"`
		SubscriptionType           *string  `json:"subscriptionType"`
		SubscriptionTypeSource     string   `json:"subscriptionTypeSource"`
		SubscriptionTypeVerifiedAt *int64   `json:"subscriptionTypeVerifiedAt"`
		RateLimitTier              *string  `json:"rateLimitTier"`
		ClientID                   *string  `json:"clientId"`
	}
	if json.Unmarshal(entry.Bytes(), &stored) != nil || !validToken(stored.AccessToken) || !validToken(stored.RefreshToken) {
		return CredentialImport{}, invalid()
	}
	if stored.ExpiresAt <= 0 || stored.ExpiresAt > now.AddDate(10, 0, 0).UnixMilli() || !slices.Contains(stored.Scopes, "user:profile") || !slices.Contains(stored.Scopes, "user:inference") {
		return CredentialImport{}, invalid()
	}
	if stored.ClientID != nil && *stored.ClientID != ClientID || stored.SubscriptionTypeSource != "" && stored.SubscriptionTypeSource != "sign_in" && stored.SubscriptionTypeSource != "profile" {
		return CredentialImport{}, invalid()
	}
	if stored.RefreshTokenExpiresAt != nil && *stored.RefreshTokenExpiresAt <= 0 || stored.SubscriptionTypeVerifiedAt != nil && *stored.SubscriptionTypeVerifiedAt <= 0 {
		return CredentialImport{}, invalid()
	}
	return CredentialImport{accessToken: stored.AccessToken, refreshToken: stored.RefreshToken}, nil
}

func (c CredentialImport) Verify(ctx context.Context, client *http.Client) (abi.GrantRefresh, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ProfileURL, nil)
	if err != nil || !validToken(c.accessToken) || !validToken(c.refreshToken) {
		return abi.GrantRefresh{}, invalid()
	}
	request.Header.Set("Authorization", "Bearer "+c.accessToken)
	bounded := &http.Client{Transport: client.Transport, Timeout: client.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := bounded.Do(request)
	if err != nil {
		return abi.GrantRefresh{}, invalid()
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || response.StatusCode != http.StatusOK {
		return abi.GrantRefresh{}, invalid()
	}
	if _, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10, MaxDepth: 16, MaxNodes: 1024}); err != nil {
		return abi.GrantRefresh{}, invalid()
	}
	var identity profile
	if json.Unmarshal(body, &identity) != nil {
		return abi.GrantRefresh{}, invalid()
	}
	facts, err := profileFacts(identity)
	if err != nil {
		return abi.GrantRefresh{}, err
	}
	return abi.GrantRefresh{Profile: ProfileID, RefreshToken: c.refreshToken, Facts: facts}, nil
}

func (c CredentialImport) Secrets() []string {
	return []string{c.accessToken, c.refreshToken}
}
