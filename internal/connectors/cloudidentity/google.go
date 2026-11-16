package cloudidentity

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	cloudauth "cloud.google.com/go/auth"
	googlecredentials "cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/compute/metadata"
)

const googleScope = "https://www.googleapis.com/auth/cloud-platform"

type detected struct {
	credentials *cloudauth.Credentials
	err         error
}

func (s *Source) GoogleToken(ctx context.Context, key Key, credential Credential) (string, error) {
	if cached, ok := s.googleTokens.get(key); ok && cached.Value != "" && cached.Expiry.After(time.Now().Add(earlyRefresh)) {
		return cached.Value, nil
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	credentials, err := s.googleCredentials(ctx, credential)
	if err != nil {
		return "", err
	}
	token, err := credentials.Token(ctx)
	if err != nil {
		return "", exchangeFailure(err)
	}
	s.googleTokens.put(key, token)
	return token.Value, nil
}

func (s *Source) googleCredentials(ctx context.Context, credential Credential) (*cloudauth.Credentials, error) {
	// The discarded logger keeps environment-enabled SDK debug logs from exposing token exchanges.
	options := &googlecredentials.DetectOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Scopes: []string{googleScope},
		Client: s.public, DisableAsyncRefresh: true, EarlyTokenRefresh: earlyRefresh,
	}
	if credential.stored {
		credentials, err := googlecredentials.NewCredentialsFromJSON(googlecredentials.ServiceAccount, credential.secret, options)
		if err != nil {
			return nil, ErrRejected
		}
		return credentials, nil
	}
	// Detection probes the environment without taking a context, so it runs
	// once and a cancelled request stops waiting instead of starting another.
	s.applicationOnce.Do(func() {
		s.application = make(chan detected, 1)
		go func() {
			credentials, err := s.applicationDefault(options)
			s.application <- detected{credentials, err}
		}()
	})
	select {
	case found := <-s.application:
		s.application <- found
		if found.err != nil {
			return nil, ErrRejected
		}
		return found.credentials, nil
	case <-ctx.Done():
		return nil, ErrUnavailable
	}
}

func (s *Source) applicationDefault(options *googlecredentials.DetectOptions) (*cloudauth.Credentials, error) {
	if path := s.getenv("GOOGLE_APPLICATION_CREDENTIALS"); path != "" {
		file, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return googleCredentialsFile(file, options)
	}
	if file, err := wellKnownGoogleCredentials(); err == nil {
		return googleCredentialsFile(file, options)
	}
	client := metadata.NewWithOptions(&metadata.Options{Client: s.metadata})
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	if !client.OnGCEWithContext(ctx) {
		return nil, ErrRejected
	}
	return cloudauth.NewCredentials(&cloudauth.CredentialsOptions{
		TokenProvider: googleMetadataTokens{client: client, scopes: strings.Join(options.Scopes, ",")},
	}), nil
}

func wellKnownGoogleCredentials() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(home, ".config", "gcloud", "application_default_credentials.json"))
}

func googleCredentialsFile(file []byte, options *googlecredentials.DetectOptions) (*cloudauth.Credentials, error) {
	var header struct {
		Type googlecredentials.CredType `json:"type"`
	}
	if err := json.Unmarshal(file, &header); err != nil {
		return nil, err
	}
	return googlecredentials.NewCredentialsFromJSON(header.Type, file, options)
}

type googleMetadataTokens struct {
	client *metadata.Client
	scopes string
}

func (t googleMetadataTokens) Token(ctx context.Context) (*cloudauth.Token, error) {
	raw, err := t.client.GetWithContext(ctx, "instance/service-accounts/default/token?"+url.Values{"scopes": {t.scopes}}.Encode())
	if err != nil {
		return nil, err
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal([]byte(raw), &token); err != nil {
		return nil, err
	}
	if token.AccessToken == "" || token.ExpiresIn <= 0 {
		return nil, ErrUnavailable
	}
	return &cloudauth.Token{Value: token.AccessToken, Type: token.TokenType, Expiry: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)}, nil
}
