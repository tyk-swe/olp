package connectors

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/egress"
)

func NewGoogleBillingAuth(policy *egress.Policy, client *http.Client) *Auth {
	auth := NewAuth(policy)
	public := &egress.Policy{}
	auth.publicClient = &http.Client{
		Transport:     boundedAuthTransport{base: client.Transport, policy: public},
		Timeout:       authTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return egress.ErrRedirect },
	}
	return auth
}

func (a *Auth) ApplyGoogleBigQuery(ctx context.Context, req *http.Request, mode string, secret []byte) (egress.Sensitive, error) {
	if req.URL == nil || req.URL.Scheme != "https" || req.URL.Hostname() != "bigquery.googleapis.com" ||
		req.URL.User != nil || req.URL.Opaque != "" || (req.URL.Port() != "" && req.URL.Port() != "443") {
		return egress.Sensitive{}, ErrAuthentication
	}
	if mode != "adc" && mode != "service_account" {
		return egress.Sensitive{}, ErrCredentialRejected
	}
	token, err := a.googleToken(ctx, Config{Kind: "vertex_ai", AuthMode: mode}, secret)
	if err != nil {
		return egress.Sensitive{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token.Value)
	var sensitive egress.Sensitive
	sensitive.Add(string(secret), token.Value)
	return sensitive, nil
}
