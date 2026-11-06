package personal

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"golang.org/x/oauth2"
)

func TestValidTokenRequiresLiveBearerMaterial(t *testing.T) {
	live := &oauth2.Token{AccessToken: "access", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}
	if err := validToken(live); err != nil {
		t.Fatalf("validToken(live bearer): %v", err)
	}
	live.TokenType = "bearer"
	if err := validToken(live); err != nil {
		t.Fatalf("validToken(lowercase bearer): %v", err)
	}
	cases := []*oauth2.Token{
		nil,
		{AccessToken: "access", TokenType: "Bearer"},
		{AccessToken: "access", TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute)},
		{AccessToken: "access", TokenType: "Basic", Expiry: time.Now().Add(time.Hour)},
		{TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)},
		{AccessToken: "line\nbreak", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)},
	}
	for i, token := range cases {
		if err := validToken(token); !errors.Is(err, errOAuth) {
			t.Errorf("validToken(case %d) err = %v, want errOAuth", i, err)
		}
	}
}

func TestGrantProblemMapping(t *testing.T) {
	other := errors.New("database unavailable")
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{pgx.ErrNoRows, 404, "not_found"},
		{fmt.Errorf("wrapped: %w", pgx.ErrNoRows), 404, "not_found"},
		{ErrRequired, 409, "grant_binding_changed"},
	}
	for _, tc := range cases {
		var problem *access.Problem
		if got := grantProblem(tc.err); !errors.As(got, &problem) || problem.Status != tc.status || problem.Code != tc.code {
			t.Errorf("grantProblem(%v) = %v, want status %d code %q", tc.err, got, tc.status, tc.code)
		}
	}
	if got := grantProblem(other); got != other {
		t.Errorf("grantProblem rewrote an unrelated error: %v", got)
	}
	var problem *access.Problem
	if got := oauthProblem(errOAuth); !errors.As(got, &problem) || problem.Status != 422 || problem.Code != "grant_authorization_failed" {
		t.Errorf("oauthProblem(errOAuth) = %v", got)
	}
	if got := oauthProblem(ErrRequired); !errors.As(got, &problem) || problem.Code != "grant_binding_changed" {
		t.Errorf("oauthProblem(ErrRequired) = %v, want grant problem mapping", got)
	}
}

func TestDelegatedExpiryCapsAtAgentExpiry(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	if got := delegatedExpiry(keyauthority.Authority{}, now); !got.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("default expiry = %v", got)
	}
	soon := now.Add(time.Minute)
	agent := keyauthority.Authority{DelegationAgentID: "agent-1", ExpiresAt: &soon}
	if got := delegatedExpiry(agent, now); !got.Equal(soon) {
		t.Fatalf("agent expiry = %v, want capped at %v", got, soon)
	}
	later := now.Add(time.Hour)
	agent.ExpiresAt = &later
	if got := delegatedExpiry(agent, now); !got.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("far agent expiry = %v, want the two minute cap", got)
	}
	plain := keyauthority.Authority{ExpiresAt: &soon}
	if got := delegatedExpiry(plain, now); !got.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("non-agent expiry = %v, want the two minute cap", got)
	}
}

func TestReplacedSecretIDs(t *testing.T) {
	if got := replacedSecretIDs(storedGrant{AccessID: "access-1"}); len(got) != 1 || got[0] != "access-1" {
		t.Fatalf("access only = %v", got)
	}
	refresh := "refresh-1"
	got := replacedSecretIDs(storedGrant{AccessID: "access-1", RefreshID: &refresh})
	if len(got) != 2 || got[0] != "access-1" || got[1] != "refresh-1" {
		t.Fatalf("with refresh = %v", got)
	}
}
