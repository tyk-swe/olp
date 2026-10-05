package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
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

func TestEnrollmentExchangesClientCredentialsForAToken(t *testing.T) {
	sent := authorizationServer(t, http.StatusOK, `{"access_token":"token-1","token_type":"Bearer","expires_in":3600}`)
	grant, err := clientCredentials{}.ExchangeGrant(context.Background(), plugin.GrantExchange{Input: " client-a:s3cr&t\n"})
	if err != nil {
		t.Fatal(err)
	}
	if grant.AccessToken != "token-1" || grant.ExpiresIn != 3600 || grant.Principal != "client-a" || grant.RefreshToken != "client-a:s3cr&t" {
		t.Fatalf("grant = %+v", grant)
	}
	form, _ := url.ParseQuery(string(sent.Body))
	if sent.URL != tokenEndpoint || form.Get("grant_type") != "client_credentials" || form.Get("scope") != scope {
		t.Fatalf("token request = %+v", sent)
	}
	if got := sent.Header["Authorization"][0]; got != "Basic Y2xpZW50LWE6czNjciUyNnQ=" {
		t.Fatalf("Authorization = %s", got)
	}
}

func TestRefreshReissuesAndKeepsTheClientCredentials(t *testing.T) {
	authorizationServer(t, http.StatusOK, `{"access_token":"token-2","expires_in":60}`)
	grant, err := clientCredentials{}.RefreshGrant(context.Background(), plugin.GrantRefresh{RefreshToken: "client-a:secret"})
	if err != nil || grant.AccessToken != "token-2" || grant.RefreshToken != "" {
		t.Fatalf("refresh = %+v %v", grant, err)
	}
}

func TestRefusedCredentialsLapseTheGrant(t *testing.T) {
	authorizationServer(t, http.StatusBadRequest, `{"error":"invalid_client"}`)
	_, err := clientCredentials{}.RefreshGrant(context.Background(), plugin.GrantRefresh{RefreshToken: "client-a:revoked"})
	var reported *abi.Error
	if !errors.As(err, &reported) || reported.Code != abi.CodeInvalidGrant {
		t.Fatalf("refused credentials: %v", err)
	}
	if _, err = (clientCredentials{}).ExchangeGrant(context.Background(), plugin.GrantExchange{Input: "no-separator"}); !errors.As(err, &reported) || reported.Code != abi.CodeInvalidRequest {
		t.Fatalf("malformed input: %v", err)
	}
}

func TestStartGrantSendsTheOperatorToTheClientsPage(t *testing.T) {
	authorization, err := clientCredentials{}.StartGrant(context.Background(), plugin.GrantStart{})
	if err != nil || authorization.URL != clientsPage || authorization.Device != nil {
		t.Fatalf("authorization = %+v %v", authorization, err)
	}
}
