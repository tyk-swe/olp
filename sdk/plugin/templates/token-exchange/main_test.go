package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
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

func TestEnrollmentExchangesTheAPIKey(t *testing.T) {
	sent := authorizationServer(t, http.StatusOK, `{"access_token":"token-1","issued_token_type":"urn:ietf:params:oauth:token-type:access_token","token_type":"Bearer","expires_in":3600,"account":"acct-7"}`)
	grant, err := tokenExchange{}.ExchangeGrant(context.Background(), plugin.GrantExchange{Input: "key-abc\n"})
	if err != nil {
		t.Fatal(err)
	}
	if grant.AccessToken != "token-1" || grant.ExpiresIn != 3600 || grant.Principal != "acct-7" || grant.RefreshToken != "key-abc" {
		t.Fatalf("grant = %+v", grant)
	}
	form, _ := url.ParseQuery(string(sent.Body))
	if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || form.Get("subject_token") != "key-abc" || form.Get("subject_token_type") != subjectTokenType {
		t.Fatalf("token request = %v", form)
	}
}

func TestAPrincipalIsDerivedWithoutRevealingTheKey(t *testing.T) {
	authorizationServer(t, http.StatusOK, `{"access_token":"token-1","expires_in":60}`)
	grant, err := tokenExchange{}.RefreshGrant(context.Background(), plugin.GrantRefresh{RefreshToken: "key-abc"})
	if err != nil || !strings.HasPrefix(grant.Principal, "key-") || strings.Contains(grant.Principal, "abc") || grant.RefreshToken != "" {
		t.Fatalf("refresh = %+v %v", grant, err)
	}
}

func TestARevokedKeyLapsesTheGrant(t *testing.T) {
	authorizationServer(t, http.StatusBadRequest, `{"error":"invalid_grant"}`)
	_, err := tokenExchange{}.RefreshGrant(context.Background(), plugin.GrantRefresh{RefreshToken: "key-revoked"})
	var reported *abi.Error
	if !errors.As(err, &reported) || reported.Code != abi.CodeInvalidGrant {
		t.Fatalf("revoked key: %v", err)
	}
}
