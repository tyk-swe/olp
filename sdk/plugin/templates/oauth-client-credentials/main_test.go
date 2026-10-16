package main

import (
	"context"
	"errors"
	"net/http"
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
