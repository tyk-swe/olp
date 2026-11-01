package claudeauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
	"golang.org/x/oauth2"
)

const (
	OAuthBeta   = "oauth-2025-04-20"
	ProfileID   = "claude-subscription"
	Upstream    = "https://api.anthropic.com"
	TokenURL    = "https://platform.claude.com/v1/oauth/token"
	ProfileURL  = Upstream + "/api/oauth/profile"
	ClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	RedirectURI = "https://platform.claude.com/oauth/code/callback"
)

type Adapter struct {
	Fetch func(context.Context, abi.HTTPRequest) (abi.HTTPResponse, error)
}

func (Adapter) Manifest() abi.Manifest { return Manifest() }

func Manifest() abi.Manifest {
	return abi.Manifest{
		Name: "claude", Version: "0.1.0", Description: "Claude subscription enrollment for OLP code-mode. Live inference is not qualified.",
		Origins: []string{"https://claude.ai", "https://platform.claude.com", Upstream},
		Profiles: []abi.Profile{{ID: ProfileID, Label: "Claude subscription (live qualification pending)", Dialect: "anthropic-messages", Signing: true,
			Grant:   &abi.GrantAuthentication{Facts: []string{"account_uuid", "organization_uuid"}},
			Hosting: abi.Hosting{Address: Upstream, Headers: map[string]string{"Authorization": "Bearer {credential}"}},
		}},
	}
}

func (Adapter) Sign(context.Context, abi.SignRequest) (abi.SignResult, error) {
	return abi.SignResult{}, &abi.Error{Code: "code_mode_required", Message: "Use a published code-mode route. Ordinary inference and synthetic probes are disabled."}
}

type session struct{ State, Verifier string }

func (Adapter) StartGrant(_ context.Context, start abi.GrantStart) (abi.GrantAuthorization, error) {
	if start.Profile != ProfileID {
		return abi.GrantAuthorization{}, invalid()
	}
	state := session{State: oauth2.GenerateVerifier(), Verifier: oauth2.GenerateVerifier()}
	configuration := oauth2.Config{ClientID: ClientID, RedirectURL: RedirectURI,
		Endpoint: oauth2.Endpoint{AuthURL: "https://claude.ai/oauth/authorize"},
		Scopes:   []string{"user:profile", "user:inference", "user:sessions:claude_code", "user:mcp_servers", "user:file_upload"},
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return abi.GrantAuthorization{}, invalid()
	}
	return abi.GrantAuthorization{URL: configuration.AuthCodeURL(state.State, oauth2.S256ChallengeOption(state.Verifier), oauth2.SetAuthURLParam("code", "true")), Session: string(encoded)}, nil
}

type tokenRequest struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code,omitempty"`
	RedirectURI  string `json:"redirect_uri,omitempty"`
	ClientID     string `json:"client_id"`
	CodeVerifier string `json:"code_verifier,omitempty"`
	State        string `json:"state,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

func (a Adapter) ExchangeGrant(ctx context.Context, exchange abi.GrantExchange) (abi.Grant, error) {
	var state session
	if exchange.Profile != ProfileID || len(exchange.Session) > 16<<10 || json.Unmarshal([]byte(exchange.Session), &state) != nil || !validToken(state.State) || !validToken(state.Verifier) {
		return abi.Grant{}, invalid()
	}
	code, returned := callback(exchange.Input)
	if returned != state.State {
		return abi.Grant{}, &abi.Error{Code: abi.CodeStateMismatch, Message: "Paste the code returned by this sign-in."}
	}
	if !validToken(code) {
		return abi.Grant{}, invalid()
	}
	return a.issue(ctx, tokenRequest{GrantType: "authorization_code", ClientID: ClientID, RedirectURI: RedirectURI, Code: code, State: state.State, CodeVerifier: state.Verifier})
}

func callback(input string) (string, string) {
	address, err := url.Parse(strings.TrimSpace(input))
	if err == nil && address.Scheme != "" {
		if address.Scheme+"://"+address.Host+address.Path != RedirectURI || address.User != nil || address.Fragment != "" || address.Query().Get("error") != "" || len(address.Query()["code"]) != 1 || len(address.Query()["state"]) != 1 {
			return "", ""
		}
		return address.Query().Get("code"), address.Query().Get("state")
	}
	code, state, _ := strings.Cut(strings.TrimSpace(input), "#")
	return code, state
}

func (a Adapter) RefreshGrant(ctx context.Context, refresh abi.GrantRefresh) (abi.Grant, error) {
	if refresh.Profile != ProfileID || !validToken(refresh.RefreshToken) {
		return abi.Grant{}, invalid()
	}
	grant, err := a.issue(ctx, tokenRequest{GrantType: "refresh_token", ClientID: ClientID, RefreshToken: refresh.RefreshToken})
	if err != nil {
		return abi.Grant{}, err
	}
	if !maps.Equal(grant.Facts, refresh.Facts) {
		return abi.Grant{}, &abi.Error{Code: abi.CodeInvalidGrant, Message: "Claude authorization names a different account or organization. Enroll again."}
	}
	return grant, nil
}

type profile struct {
	Account struct {
		UUID string `json:"uuid"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
	} `json:"organization"`
}

func Principal(facts map[string]string) string {
	account, accountErr := uuid.Parse(facts["account_uuid"])
	organization, organizationErr := uuid.Parse(facts["organization_uuid"])
	if len(facts) != 2 || accountErr != nil || organizationErr != nil || account == uuid.Nil || organization == uuid.Nil || account.String() != facts["account_uuid"] || organization.String() != facts["organization_uuid"] {
		return ""
	}
	sum := sha256.Sum256([]byte(account.String() + "\x00" + organization.String()))
	return "claude:" + hex.EncodeToString(sum[:])
}

func ValidAccessToken(token string) bool { return validToken(token) }

func validToken(token string) bool {
	if token == "" || len(token) > 16<<10 {
		return false
	}
	for _, character := range token {
		if character <= 32 || character >= 127 {
			return false
		}
	}
	return true
}

func (a Adapter) issue(ctx context.Context, request tokenRequest) (abi.Grant, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return abi.Grant{}, invalid()
	}
	tokens, err := authorizationCall[tokenResponse](ctx, a, abi.HTTPRequest{Method: "POST", URL: TokenURL, Header: map[string][]string{"Content-Type": {"application/json"}}, Body: body})
	if err != nil {
		return abi.Grant{}, err
	}

	if !validToken(tokens.AccessToken) || !strings.EqualFold(tokens.TokenType, "bearer") || tokens.ExpiresIn < 1 || tokens.ExpiresIn > 366*24*60*60 || tokens.RefreshToken != "" && !validToken(tokens.RefreshToken) || request.GrantType == "authorization_code" && tokens.RefreshToken == "" {
		return abi.Grant{}, invalid()
	}
	identity, err := authorizationCall[profile](ctx, a, abi.HTTPRequest{Method: "GET", URL: ProfileURL, Header: map[string][]string{"Authorization": {"Bearer " + tokens.AccessToken}}})
	if err != nil {
		return abi.Grant{}, err
	}

	facts, err := profileFacts(identity)
	if err != nil {
		return abi.Grant{}, err
	}
	return abi.Grant{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresIn: tokens.ExpiresIn, Principal: Principal(facts), Facts: facts}, nil
}

func profileFacts(identity profile) (map[string]string, error) {
	facts := map[string]string{"account_uuid": strings.ToLower(identity.Account.UUID), "organization_uuid": strings.ToLower(identity.Organization.UUID)}
	if Principal(facts) == "" {
		return nil, invalid()
	}
	return facts, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func authorizationCall[T tokenResponse | profile](ctx context.Context, a Adapter, request abi.HTTPRequest) (T, error) {
	var output T
	response, err := a.Fetch(ctx, request)
	if err != nil {
		return output, &abi.Error{Code: abi.CodeHTTPFailed, Message: "Claude authorization request did not complete."}
	}
	if response.Status != 200 {
		code := responseCode(response.Status, response.Body)
		return output, &abi.Error{Code: code, Message: "Claude authorization endpoint refused the request."}
	}
	if _, err := oif.ParseJSON(response.Body, oif.Limits{MaxBytes: 64 << 10, MaxDepth: 16, MaxNodes: 1024}); err != nil {
		return output, invalid()
	}
	if json.Unmarshal(response.Body, &output) != nil {
		return output, invalid()
	}
	return output, nil
}

func responseCode(status int, body []byte) string {
	var refusal struct {
		Error string `json:"error"`
	}
	if status == 401 || json.Unmarshal(body, &refusal) == nil && refusal.Error == "invalid_grant" {
		return abi.CodeInvalidGrant
	}
	return "claude_authorization_failed"
}

func invalid() error {
	return &abi.Error{Code: "claude_authorization_invalid", Message: "Claude authorization response or verified identity is unsupported. Enroll again."}
}
