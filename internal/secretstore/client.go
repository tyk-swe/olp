package secretstore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	cloudauth "cloud.google.com/go/auth"
	googlecredentials "cloud.google.com/go/auth/credentials"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/tyk-swe/olp/internal/egress"
)

const timeout = 15 * time.Second
const maxSecret = 64 << 10

type credentialResult struct {
	credential *cloudauth.Credentials
	err        error
}

// Resolver uses ambient cloud workload identity. A Vault deployment supplies a
// trusted origin, mounted JWT, role and auth mount; no access token belongs in
// a reference.
type Resolver struct {
	client *http.Client
	mu     sync.Mutex
	google chan credentialResult
	azure  azcore.TokenCredential
	aws    map[string]aws.Config
	getenv func(string) string
}

func New(p egress.Policy) *Resolver {
	client := p.Client(timeout)
	client.Transport = boundedTransport{client.Transport}
	client.Timeout = timeout
	return &Resolver{client: client, getenv: os.Getenv, aws: map[string]aws.Config{}}
}

// Limit SDK responses as well as the REST adapters. Cloud SDK unmarshalling
// happens after this transport and must not allocate an unbounded body.
type boundedTransport struct{ http.RoundTripper }

func (transport boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.RoundTripper.RoundTrip(request)
	if err == nil && response.Body != nil {
		response.Body = http.MaxBytesReader(nil, response.Body, 256<<10)
	}
	return response, err
}

func (r *Resolver) awsConfig(ctx context.Context, region string) (aws.Config, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if value, ok := r.aws[region]; ok {
		return value, nil
	}
	value, err := config.LoadDefaultConfig(ctx, config.WithRegion(region), config.WithHTTPClient(r.client), config.WithRetryMaxAttempts(1))
	if err != nil {
		return aws.Config{}, ErrUnavailable
	}
	if len(r.aws) >= 16 {
		clear(r.aws)
	}
	r.aws[region] = value
	return value, nil
}

func (r *Resolver) googleToken(ctx context.Context) (string, error) {
	r.mu.Lock()
	if r.google == nil {
		r.google = make(chan credentialResult, 1)
		result := r.google
		options := &googlecredentials.DetectOptions{Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}, Client: r.client, DisableAsyncRefresh: true, Logger: slog.New(slog.DiscardHandler)}
		go func() {
			credential, err := googlecredentials.DetectDefault(options)
			result <- credentialResult{credential, err}
		}()
	}
	result := r.google
	r.mu.Unlock()
	select {
	case value := <-result:
		result <- value
		if value.err != nil || value.credential == nil {
			return "", ErrUnavailable
		}
		token, err := value.credential.Token(ctx)
		if err != nil || token.Value == "" {
			return "", ErrUnavailable
		}
		return token.Value, nil
	case <-ctx.Done():
		return "", ErrUnavailable
	}
}

func (r *Resolver) azureToken(ctx context.Context) (string, error) {
	r.mu.Lock()
	credential := r.azure
	if credential == nil {
		options := azcore.ClientOptions{Transport: r.client}
		var err error
		if r.getenv("AZURE_FEDERATED_TOKEN_FILE") != "" {
			credential, err = azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{ClientOptions: options})
		} else {
			managed := &azidentity.ManagedIdentityCredentialOptions{ClientOptions: options}
			if id := r.getenv("AZURE_CLIENT_ID"); id != "" {
				managed.ID = azidentity.ClientID(id)
			}
			credential, err = azidentity.NewManagedIdentityCredential(managed)
		}
		if err != nil {
			r.mu.Unlock()
			return "", ErrUnavailable
		}
		r.azure = credential
	}
	r.mu.Unlock()
	token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{"https://vault.azure.net/.default"}})
	if err != nil || token.Token == "" {
		return "", ErrUnavailable
	}
	return token.Token, nil
}

func azureResource(raw, kind string, versioned bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Hostname(), ".vault.azure.net") || u.RawPath != "" {
		return nil, ErrUnavailable
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	want := 2
	if versioned {
		want = 3
	}
	if len(parts) != want || parts[0] != kind {
		return nil, ErrUnavailable
	}
	for _, part := range parts[1:] {
		if !component.MatchString(part) {
			return nil, ErrUnavailable
		}
	}
	if versioned && len(parts[2]) != 32 {
		return nil, ErrUnavailable
	}
	return u, nil
}

func vaultResource(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, "/v1/") {
		return nil, ErrUnavailable
	}
	for _, part := range strings.Split(strings.TrimPrefix(u.Path, "/v1/"), "/") {
		if !component.MatchString(part) {
			return nil, ErrUnavailable
		}
	}
	return u, nil
}

func (r *Resolver) json(ctx context.Context, method, endpoint string, headers map[string]string, body any, out any) error {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return ErrUnavailable
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return ErrUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := r.client.Do(request)
	if err != nil {
		return ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 256<<10+1))
	if err != nil || len(data) > 256<<10 || json.Unmarshal(data, out) != nil {
		return ErrUnavailable
	}
	return nil
}
