package mcpgrants

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

const oauthTimeout = 20 * time.Second

var errOAuth = errors.New("personal authorization could not be completed")

type oauthClient struct {
	config oauth2.Config
	client *http.Client
}

func (s *Store) oauthClient(ctx context.Context, bound Binding, redirect string) (oauthClient, error) {
	if s.Egress == nil {
		return oauthClient{}, errOAuth
	}
	for _, endpoint := range []string{bound.OAuth.AuthorizationEndpoint, bound.OAuth.TokenEndpoint, bound.OAuth.DeviceAuthorizationEndpoint} {
		if endpoint == "" {
			continue
		}
		if _, err := s.Egress.ValidateEndpoint(endpoint); err != nil {
			return oauthClient{}, errOAuth
		}
	}
	secret, secretErr := s.entraClientSecret(ctx, bound)
	if secretErr != nil {
		return oauthClient{}, secretErr
	}
	return oauthClient{config: oauth2.Config{ClientSecret: secret, ClientID: bound.OAuth.ClientID, RedirectURL: redirect, Scopes: bound.OAuth.Scopes, Endpoint: oauth2.Endpoint{AuthURL: bound.OAuth.AuthorizationEndpoint, TokenURL: bound.OAuth.TokenEndpoint, DeviceAuthURL: bound.OAuth.DeviceAuthorizationEndpoint, AuthStyle: oauth2.AuthStyleInParams}}, client: s.Egress.Client(oauthTimeout)}, nil
}

func (o oauthClient) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, o.client)
}

func validToken(token *oauth2.Token) error {
	if token == nil || token.Expiry.IsZero() || !token.Expiry.After(time.Now()) {
		return errOAuth
	}
	if token.TokenType != "Bearer" && token.TokenType != "bearer" {
		return errOAuth
	}
	if err := grants.ValidateTokenMaterial(token.AccessToken, token.RefreshToken, int64(time.Until(token.Expiry)/time.Second)); err != nil {
		return errOAuth
	}
	return nil
}

func (s *Store) verificationURL(bound Binding, raw string) error {
	if s.Egress == nil {
		return errOAuth
	}
	target, err := s.Egress.ValidateEndpoint(raw)
	if err != nil {
		return errOAuth
	}
	declared, parseErr := url.Parse(bound.OAuth.DeviceAuthorizationEndpoint)
	if parseErr != nil || target.Scheme != declared.Scheme || target.Host != declared.Host {
		return errOAuth
	}
	return nil
}

func oauthProblem(err error) error {
	if errors.Is(err, errOAuth) {
		return access.Fail(422, "grant_authorization_failed", "Personal authorization failed. Start a new enrollment.")
	}
	return grantProblem(err)
}

func (s *Store) entraClientSecret(ctx context.Context, bound Binding) (string, error) {
	if bound.OAuth.EntraOBO == nil {
		return "", nil
	}
	var id string
	var config routing.MCPOAuth
	if err := s.Pool.QueryRow(ctx, "SELECT s.entra_client_secret_id::text,v.oauth FROM olp.mcp_servers s JOIN olp.mcp_server_revisions v ON v.id=s.latest_revision_id WHERE s.id=$1 AND s.project_id=$2 AND s.enabled AND s.retired_at IS NULL", bound.ServerID, bound.ProjectID).Scan(&id, &config); err != nil {
		return "", ErrRequired
	}
	if binding(bound.ServerID, bound.ProjectID, bound.Endpoint, config, bound.Identity).Digest() != bound.Digest() {
		return "", ErrRequired
	}
	material, err := s.Keys.Read(ctx, s.Pool, s.Installation, id, secrets.MCPEntraClient)
	if err != nil {
		return "", ErrRequired
	}
	defer clear(material)
	return string(material), nil
}
