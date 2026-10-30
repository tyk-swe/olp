package antigravityauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type Adapter struct {
	Fetch    func(context.Context, abi.HTTPRequest) (abi.HTTPResponse, error)
	ClientID func(context.Context) string
}

type session struct {
	ClientID string `json:"client_id"`
	Verifier string `json:"verifier"`
	State    string `json:"state"`
}

type refreshAuthorization struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Token        string `json:"refresh_token"`
}

func configuration(clientID, secret string) oauth2.Config {
	return oauth2.Config{
		ClientID: clientID, ClientSecret: secret, RedirectURL: RedirectURI,
		Endpoint: oauth2.Endpoint{AuthURL: Issuer + "/o/oauth2/v2/auth", TokenURL: TokenEndpoint, AuthStyle: oauth2.AuthStyleInParams},
		Scopes:   []string{"openid", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile", "https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/cclog", "https://www.googleapis.com/auth/experimentsandconfigs"},
	}
}

func (a Adapter) StartGrant(ctx context.Context, start abi.GrantStart) (abi.GrantAuthorization, error) {
	if start.Profile != ProfileID || a.ClientID == nil || !clientID(a.ClientID(ctx)) {
		return abi.GrantAuthorization{}, invalid()
	}
	state := session{ClientID: a.ClientID(ctx), Verifier: oauth2.GenerateVerifier(), State: oauth2.GenerateVerifier()}
	encoded, err := json.Marshal(state)
	if err != nil {
		return abi.GrantAuthorization{}, invalid()
	}
	config := configuration(state.ClientID, "")
	return abi.GrantAuthorization{Session: string(encoded), URL: config.AuthCodeURL(state.State, oauth2.AccessTypeOffline, oauth2.ApprovalForce, oauth2.S256ChallengeOption(state.Verifier))}, nil
}

func (a Adapter) ExchangeGrant(ctx context.Context, exchange abi.GrantExchange) (abi.Grant, error) {
	var state session
	var input struct {
		Callback string `json:"callback_url"`
		Secret   string `json:"client_secret"`
	}
	if exchange.Profile != ProfileID || a.ClientID == nil || len(exchange.Session) > 16<<10 || len(exchange.Input) > 16<<10 || json.Unmarshal([]byte(exchange.Session), &state) != nil || json.Unmarshal([]byte(exchange.Input), &input) != nil || state.ClientID != a.ClientID(ctx) || !clientID(state.ClientID) || len(state.Verifier) < 43 || len(state.State) < 43 || !credential(input.Secret) {
		return abi.Grant{}, invalid()
	}
	callback, err := url.Parse(input.Callback)
	if err != nil || callback.Scheme+"://"+callback.Host+callback.Path != RedirectURI || callback.User != nil || callback.Fragment != "" {
		return abi.Grant{}, invalid()
	}
	query, err := url.ParseQuery(callback.RawQuery)
	if err != nil || len(query["state"]) != 1 || query.Get("state") != state.State {
		return abi.Grant{}, &abi.Error{Code: abi.CodeStateMismatch, Message: "The callback belongs to a different enrollment."}
	}
	if len(query["code"]) != 1 || !credential(query.Get("code")) || query.Get("error") != "" || len(query["iss"]) > 1 || query.Get("iss") != "" && query.Get("iss") != Issuer {
		return abi.Grant{}, invalid()
	}
	config := configuration(state.ClientID, input.Secret)
	token, err := config.Exchange(a.oauthContext(ctx), query.Get("code"), oauth2.VerifierOption(state.Verifier))
	if err != nil {
		return abi.Grant{}, authorizationFailure(err)
	}
	return a.grant(ctx, token, refreshAuthorization{ClientID: state.ClientID, ClientSecret: input.Secret, Token: token.RefreshToken})
}

func (a Adapter) RefreshGrant(ctx context.Context, refresh abi.GrantRefresh) (abi.Grant, error) {
	var authorization refreshAuthorization
	if refresh.Profile != ProfileID || a.ClientID == nil || len(refresh.RefreshToken) > 16<<10 || json.Unmarshal([]byte(refresh.RefreshToken), &authorization) != nil || authorization.ClientID != a.ClientID(ctx) || !clientID(authorization.ClientID) || !credential(authorization.ClientSecret) || !credential(authorization.Token) || Principal(refresh.Facts["subject"], refresh.Facts["project_id"]) == "" {
		return abi.Grant{}, invalid()
	}
	config := configuration(authorization.ClientID, authorization.ClientSecret)
	token, err := config.TokenSource(a.oauthContext(ctx), &oauth2.Token{RefreshToken: authorization.Token}).Token()
	if err != nil {
		return abi.Grant{}, authorizationFailure(err)
	}
	grant, err := a.grant(ctx, token, refreshAuthorization{ClientID: authorization.ClientID, ClientSecret: authorization.ClientSecret, Token: token.RefreshToken})
	if err != nil {
		return abi.Grant{}, err
	}
	if !maps.Equal(grant.Facts, refresh.Facts) {
		return abi.Grant{}, &abi.Error{Code: abi.CodeInvalidGrant, Message: "The refreshed authorization names a different subject or project. Enroll again."}
	}
	return grant, nil
}

func (a Adapter) grant(ctx context.Context, token *oauth2.Token, authorization refreshAuthorization) (abi.Grant, error) {
	if !credential(token.AccessToken) || !strings.EqualFold(token.TokenType, "Bearer") || !credential(authorization.Token) || token.Expiry.IsZero() || !token.Expiry.After(time.Now()) {
		return abi.Grant{}, invalid()
	}
	identity, err := a.identity(ctx, token.AccessToken)
	if err != nil {
		return abi.Grant{}, err
	}
	encoded, err := json.Marshal(authorization)
	if err != nil || len(encoded) > 16<<10 {
		return abi.Grant{}, invalid()
	}
	return abi.Grant{AccessToken: token.AccessToken, RefreshToken: string(encoded), ExpiresIn: int64(time.Until(token.Expiry).Seconds()), Principal: identity.principal, Facts: identity.facts}, nil
}

type verifiedIdentity struct {
	principal string
	facts     map[string]string
}

func (a Adapter) identity(ctx context.Context, accessToken string) (verifiedIdentity, error) {
	profile, err := a.fetchJSON(ctx, abi.HTTPRequest{Method: http.MethodGet, URL: UserInfoEndpoint, Header: map[string][]string{"Authorization": {"Bearer " + accessToken}}})
	if err != nil {
		return verifiedIdentity{}, err
	}
	subjectValue, _ := profile.Root().Lookup("sub")
	subject, _ := subjectValue.Text()
	project, err := a.fetchJSON(ctx, abi.HTTPRequest{Method: http.MethodPost, URL: ProjectEndpoint, Header: map[string][]string{"Authorization": {"Bearer " + accessToken}, "Content-Type": {"application/json"}}, Body: []byte(`{"metadata":{}}`)})
	if err != nil {
		return verifiedIdentity{}, err
	}
	projectValue, _ := project.Root().Lookup("cloudaicompanionProject")
	projectID := projectIdentifier(projectValue)
	principal := Principal(subject, projectID)
	if principal == "" {
		return verifiedIdentity{}, invalid()
	}
	return verifiedIdentity{principal: principal, facts: map[string]string{"subject": subject, "project_id": projectID}}, nil
}

func projectIdentifier(value oif.Value) string {
	if text, ok := value.Text(); ok {
		return text
	}
	identifier, _ := value.Lookup("id")
	text, _ := identifier.Text()
	return text
}

func (a Adapter) fetchJSON(ctx context.Context, request abi.HTTPRequest) (oif.Document, error) {
	response, err := a.Fetch(ctx, request)
	if err != nil || response.Status != http.StatusOK || len(response.Body) > 64<<10 {
		return oif.Document{}, invalid()
	}
	document, err := oif.ParseJSON(response.Body, oif.Limits{})
	if err != nil || document.Root().Kind() != oif.Object {
		return oif.Document{}, invalid()
	}
	return document, nil
}

func (a Adapter) oauthContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: tokenTransport{fetch: a.Fetch}})
}

type tokenTransport struct {
	fetch func(context.Context, abi.HTTPRequest) (abi.HTTPResponse, error)
}

func (t tokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.String() != TokenEndpoint || request.Method != http.MethodPost {
		return nil, invalid()
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 16<<10))
	if err != nil {
		return nil, invalid()
	}
	response, err := t.fetch(request.Context(), abi.HTTPRequest{Method: request.Method, URL: request.URL.String(), Header: request.Header.Clone(), Body: body})
	if err != nil {
		return nil, &abi.Error{Code: abi.CodeHTTPFailed, Message: "The authorization request did not complete."}
	}
	return &http.Response{StatusCode: response.Status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(response.Body)))}, nil
}

func clientID(value string) bool {
	return identifier(value) && strings.HasSuffix(value, ".apps.googleusercontent.com")
}

func credential(value string) bool {
	return value != "" && len(value) <= 8<<10 && !strings.ContainsAny(value, "\r\n\x00")
}

func authorizationFailure(err error) error {
	if refusal, ok := errors.AsType[*oauth2.RetrieveError](err); ok && refusal.ErrorCode == "invalid_grant" {
		return &abi.Error{Code: abi.CodeInvalidGrant, Message: "The authorization server refused the grant. Enroll again."}
	}
	return &abi.Error{Code: abi.CodeHTTPFailed, Message: "The authorization request did not complete."}
}

func invalid() error {
	return &abi.Error{Code: "antigravity_authorization_invalid", Message: "Antigravity authorization or verified subject/project evidence is unsupported. Enroll again."}
}
