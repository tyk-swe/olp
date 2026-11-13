package connectors

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/connectors/cloudidentity"
	"github.com/tyk-swe/olp/internal/egress"
)

func NewBillingAuth(policy *egress.Policy, client *http.Client, getenv func(string) string) *Auth {
	return newAuth(cloudidentity.Billing(policy, client.Transport, getenv))
}

func (a *Auth) ApplyGoogleBigQuery(ctx context.Context, req *http.Request, mode string, secret []byte) (egress.Sensitive, error) {
	if req.URL == nil || req.URL.Scheme != "https" || req.URL.Hostname() != "bigquery.googleapis.com" ||
		req.URL.User != nil || req.URL.Opaque != "" || (req.URL.Port() != "" && req.URL.Port() != "443") {
		return egress.Sensitive{}, ErrAuthentication
	}
	if mode != "adc" && mode != "service_account" {
		return egress.Sensitive{}, ErrCredentialRejected
	}
	token, err := a.googleToken(ctx, Config{Kind: KindVertexAI, AuthMode: mode}, secret)
	if err != nil {
		return egress.Sensitive{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var sensitive egress.Sensitive
	sensitive.Add(string(secret), token)
	return sensitive, nil
}
