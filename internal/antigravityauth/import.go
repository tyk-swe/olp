package antigravityauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type CredentialImport struct {
	accessToken   string
	authorization refreshAuthorization
	project       string
}

func ParseCredentialImport(input, configuredClientID string, now time.Time) (CredentialImport, error) {
	document, err := oif.ParseJSON([]byte(input), oif.Limits{MaxBytes: 64 << 10, MaxDepth: 2, MaxNodes: 32})
	if err != nil || document.Root().Kind() != oif.Object {
		return CredentialImport{}, invalid()
	}
	for _, field := range document.Root().Members() {
		switch field.Name {
		case "type", "access_token", "refresh_token", "client_id", "client_secret", "email", "project_id":
			if field.Value.Kind() != oif.String {
				return CredentialImport{}, invalid()
			}
		case "expired":
			value, ok := field.Value.Text()
			expires, err := time.Parse(time.RFC3339, value)
			if !ok || err != nil || !expires.After(now) || expires.After(now.AddDate(10, 0, 0)) {
				return CredentialImport{}, invalid()
			}
		case "expires_in", "timestamp":
			if field.Value.Kind() != oif.Number {
				return CredentialImport{}, invalid()
			}
		default:
			return CredentialImport{}, invalid()
		}
	}
	var stored struct {
		Type         string `json:"type"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Project      string `json:"project_id"`
		ExpiresIn    *int64 `json:"expires_in"`
		Timestamp    *int64 `json:"timestamp"`
	}
	if json.Unmarshal([]byte(input), &stored) != nil || stored.Type != "antigravity" || !credential(stored.AccessToken) || !credential(stored.RefreshToken) || !credential(stored.ClientSecret) || !clientID(stored.ClientID) || stored.ClientID != configuredClientID || stored.Project != "" && !identifier(stored.Project) {
		return CredentialImport{}, invalid()
	}
	if stored.ExpiresIn != nil && *stored.ExpiresIn <= 0 || stored.Timestamp != nil && *stored.Timestamp <= 0 {
		return CredentialImport{}, invalid()
	}
	return CredentialImport{accessToken: stored.AccessToken, authorization: refreshAuthorization{ClientID: stored.ClientID, ClientSecret: stored.ClientSecret, Token: stored.RefreshToken}, project: stored.Project}, nil
}

func (c CredentialImport) Verify(ctx context.Context, client *http.Client) (abi.GrantRefresh, error) {
	bounded := &http.Client{Transport: client.Transport, Timeout: client.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	adapter := Adapter{Fetch: func(ctx context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
		r, err := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
		if err != nil {
			return abi.HTTPResponse{}, invalid()
		}
		r.Header = http.Header(request.Header)
		response, err := bounded.Do(r)
		if err != nil {
			return abi.HTTPResponse{}, invalid()
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		if err != nil {
			return abi.HTTPResponse{}, invalid()
		}
		return abi.HTTPResponse{Status: response.StatusCode, Body: body}, nil
	}}
	if !credential(c.accessToken) || !credential(c.authorization.Token) || !credential(c.authorization.ClientSecret) || !clientID(c.authorization.ClientID) {
		return abi.GrantRefresh{}, invalid()
	}
	identity, err := adapter.identity(ctx, c.accessToken)
	if err != nil || c.project != "" && c.project != identity.facts["project_id"] {
		return abi.GrantRefresh{}, invalid()
	}
	encoded, err := json.Marshal(c.authorization)
	if err != nil || len(encoded) > 16<<10 {
		return abi.GrantRefresh{}, invalid()
	}
	return abi.GrantRefresh{Profile: ProfileID, RefreshToken: string(encoded), Facts: identity.facts}, nil
}

func (c CredentialImport) Secrets() []string {
	return []string{c.accessToken, c.authorization.Token, c.authorization.ClientSecret}
}
