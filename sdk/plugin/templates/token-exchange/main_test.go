package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin"
)

func authorizationServer(t *testing.T, status int, body string) *plugin.HTTPRequest {
	t.Helper()
	sent := &plugin.HTTPRequest{}
	previous := fetch
	fetch = func(_ context.Context, request plugin.HTTPRequest) (plugin.HTTPResponse, error) {
		*sent = request
		return plugin.HTTPResponse{Status: status, Body: []byte(body)}, nil
	}
	t.Cleanup(func() { fetch = previous })
	return sent
}

func TestAPrincipalIsDerivedWithoutRevealingTheKey(t *testing.T) {
	authorizationServer(t, http.StatusOK, `{"access_token":"token-1","expires_in":60}`)
	grant, err := tokenExchange{}.RefreshGrant(context.Background(), plugin.GrantRefresh{RefreshToken: "key-abc"})
	if err != nil || !strings.HasPrefix(grant.Principal, "key-") || strings.Contains(grant.Principal, "abc") || grant.RefreshToken != "" {
		t.Fatalf("refresh = %+v %v", grant, err)
	}
}
