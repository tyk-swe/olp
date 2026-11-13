package cloudidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

func (s *Source) AzureToken(ctx context.Context, key Key, credential Credential, scope string) (string, error) {
	if cached, ok := s.azureTokens.get(key); ok && cached.Token != "" && cached.ExpiresOn.After(time.Now().Add(earlyRefresh)) {
		return cached.Token, nil
	}
	entra, err := s.entraCredential(key, credential)
	if err != nil {
		return "", ErrRejected
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	token, err := entra.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
	if err != nil {
		return "", exchangeFailure(err)
	}
	s.azureTokens.put(key, token)
	return token.Token, nil
}

func (s *Source) entraCredential(key Key, credential Credential) (azcore.TokenCredential, error) {
	if cached, ok := s.azureCredentials.get(key); ok {
		return cached, nil
	}
	entra, err := s.azureCredential(credential)
	if err != nil {
		return nil, err
	}
	s.azureCredentials.put(key, entra)
	return entra, nil
}

func entraCredentials(transport http.RoundTripper) func(Credential) (azcore.TokenCredential, error) {
	options := azcore.ClientOptions{Transport: client(transport)}
	return func(credential Credential) (azcore.TokenCredential, error) {
		if !credential.stored {
			return azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{ClientOptions: options})
		}
		var application struct {
			TenantID     string `json:"tenant_id"`
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		d := json.NewDecoder(bytes.NewReader(credential.secret))
		d.DisallowUnknownFields()
		if len(credential.secret) > storedLimit || d.Decode(&application) != nil || d.Decode(new(any)) != io.EOF ||
			!printable(application.TenantID, 1, 128) || !printable(application.ClientID, 1, 128) ||
			!printable(application.ClientSecret, 1, 1024) {
			return nil, ErrRejected
		}
		return azidentity.NewClientSecretCredential(application.TenantID, application.ClientID, application.ClientSecret,
			&azidentity.ClientSecretCredentialOptions{ClientOptions: options})
	}
}
