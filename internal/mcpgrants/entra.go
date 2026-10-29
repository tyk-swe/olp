package mcpgrants

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/workload"
	"golang.org/x/oauth2"
)

func (s *Store) ResolveEntra(ctx context.Context, authority access.Authority, tool routing.MCPToolsetTool, assertion workload.UserAssertion) ([]byte, error) {
	bound, err := Bind(authority, tool)
	if err != nil {
		return nil, err
	}
	return s.resolveEntra(ctx, bound, assertion)
}

func (s *Store) ResolveAgentEntra(ctx context.Context, authority access.Authority, publicationID string, agent agentcontract.Definition, assertion workload.UserAssertion) ([]byte, string, error) {
	bound, err := BindAgent(authority, publicationID, agent)
	if err != nil {
		return nil, "", err
	}
	token, err := s.resolveEntra(ctx, bound, assertion)
	return token, bound.Digest(), err
}

func (s *Store) resolveEntra(ctx context.Context, bound Binding, assertion workload.UserAssertion) ([]byte, error) {
	if bound.OAuth.EntraOBO == nil || bound.Identity.Kind != EndUserIdentity || assertion.Token() == "" || !assertion.ExpiresAt().After(time.Now()) {
		return nil, ErrRequired
	}
	if _, err := liveBinding(ctx, s.Pool, bound); err != nil {
		return nil, err
	}
	current, currentErr := readGrant(ctx, s.Pool, bound)
	if currentErr == nil && current.RefreshID == nil && !current.Expires.After(time.Now()) {
		return s.renewEntra(ctx, bound, current.ID, assertion)
	}
	token, resolveErr := s.resolveGrant(ctx, bound, "")
	if resolveErr == nil {
		return token, nil
	}
	if !errors.Is(resolveErr, ErrRequired) {
		return nil, resolveErr
	}
	resource, acquireErr := s.Pool.Acquire(ctx)
	if acquireErr != nil {
		return nil, acquireErr
	}
	conn := resource.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))
	return grants.WithRefreshLock(ctx, conn, "mcp-obo:"+bound.Digest(), true, func() ([]byte, error) {
		var exists bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.mcp_user_grants WHERE binding_digest=$1)", bound.Digest()).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			return s.resolveGrant(ctx, bound, "")
		}
		if _, err := liveBinding(ctx, conn, bound); err != nil {
			return nil, err
		}
		client, clientErr := s.oauthClient(ctx, bound, "", "")
		if clientErr != nil {
			return nil, clientErr
		}
		defer client.client.CloseIdleConnections()
		item := flowRecord{Enrollment: Enrollment{ID: access.NewID(), Binding: bound, ExpiresAt: time.Now().Add(10 * time.Minute)}}
		attempt := access.NewID()
		if _, err := conn.Exec(ctx, `INSERT INTO olp.mcp_grant_enrollments(id,server_id,agent_id,project_id,binding,binding_digest,handoff_digest,expires_at,exchange_attempt_id) VALUES($1,NULLIF($2,'')::uuid,NULLIF($3,'')::uuid,$4,$5,$6,$7,$8,$9)`, item.ID, bound.ServerID, bound.AgentID, bound.ProjectID, bound, bound.Digest(), s.Auth.Digest(secrets.MCPEnrollmentDigest, rand.Text()), item.ExpiresAt, attempt); err != nil {
			return nil, err
		}
		issued, issueErr := client.exchangeEntra(ctx, bound, assertion)
		if issueErr != nil {
			return nil, errors.Join(ErrRequired, s.endFlow(ctx, item.ID, "failed"))
		}
		if err := s.completeGrant(ctx, item, attempt, issued); err != nil {
			return nil, err
		}
		return []byte(issued.AccessToken), nil
	})
}

type entraTokenResponse struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	TokenType    string      `json:"token_type"`
	ExpiresIn    json.Number `json:"expires_in"`
}

func (o oauthClient) exchangeEntra(ctx context.Context, bound Binding, assertion workload.UserAssertion) (*oauth2.Token, error) {
	if !assertion.ExpiresAt().After(time.Now()) {
		return nil, ErrRequired
	}
	exchangeCtx, cancel := context.WithTimeout(ctx, oauthTimeout)
	defer cancel()
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "requested_token_use": {"on_behalf_of"}, "client_id": {o.config.ClientID}, "client_secret": {o.config.ClientSecret}, "assertion": {assertion.Token()}, "scope": {strings.Join(bound.OAuth.Scopes, " ")}}
	request, err := http.NewRequestWithContext(exchangeCtx, http.MethodPost, o.config.Endpoint.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errOAuth
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, requestErr := o.client.Do(request)
	if requestErr != nil {
		return nil, errOAuth
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errOAuth
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
	if readErr != nil || len(body) > 65536 {
		return nil, errOAuth
	}
	defer clear(body)
	var result entraTokenResponse
	if json.Unmarshal(body, &result) != nil {
		return nil, errOAuth
	}
	expires, expiryErr := strconv.ParseInt(string(result.ExpiresIn), 10, 64)
	if expiryErr != nil || expires <= 0 || expires > 86400*365 {
		return nil, errOAuth
	}
	token := &oauth2.Token{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, TokenType: result.TokenType, Expiry: time.Now().Add(time.Duration(expires) * time.Second)}
	if err := validToken(token); err != nil {
		return nil, err
	}
	return token, nil
}

func (s *Store) renewEntra(ctx context.Context, bound Binding, id string, assertion workload.UserAssertion) ([]byte, error) {
	resource, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	conn := resource.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))
	return grants.WithRefreshLock(ctx, conn, "mcp:"+id, true, func() ([]byte, error) {
		current, readErr := readGrant(ctx, conn, bound)
		if readErr != nil || current.ID != id {
			return nil, ErrRequired
		}
		if current.Expires.After(time.Now()) {
			return s.readAccess(ctx, conn, bound, current)
		}
		if current.RefreshID != nil {
			return s.refreshLocked(ctx, conn, bound, id, "")
		}
		return s.replaceGrant(ctx, conn, bound, current, "", func(client oauthClient) (*oauth2.Token, error) { return client.exchangeEntra(ctx, bound, assertion) })
	})
}
