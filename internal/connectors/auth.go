package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	cloudauth "cloud.google.com/go/auth"
	googlecredentials "cloud.google.com/go/auth/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"

	"github.com/tyk-swe/olp/internal/egress"
)

var ErrAuthentication = errors.New("provider authentication failed")

const authTimeout = 10 * time.Second
const authBodyLimit = 1 << 20

type Auth struct {
	mu           sync.Mutex
	tokens       map[[32]byte]*cloudauth.Token
	aws          map[[32]byte]aws.CredentialsProvider
	client       *http.Client
	googleClient *http.Client
	adc          chan credentialResult
}

func NewAuth(policy *egress.Policy) *Auth {
	// The maintained AWS chain owns metadata credential discovery. Permit its
	// fixed link-local endpoints only on this authentication transport.
	awsPolicy := *policy
	awsPolicy.AllowedNetworks = append([]netip.Prefix{}, policy.AllowedNetworks...)
	awsPolicy.PlainHTTPHosts = append([]string{}, policy.PlainHTTPHosts...)
	for _, host := range []string{"169.254.169.254", "169.254.170.2", "169.254.170.23", "fd00:ec2::254", "fd00:ec2::23"} {
		address := netip.MustParseAddr(host)
		awsPolicy.AllowedNetworks = append(awsPolicy.AllowedNetworks, netip.PrefixFrom(address, address.BitLen()))
		awsPolicy.PlainHTTPHosts = append(awsPolicy.PlainHTTPHosts, host)
	}
	client := awsPolicy.Client(authTimeout)
	client.Timeout = authTimeout
	client.Transport = boundedAuthTransport{base: client.Transport, policy: &awsPolicy}
	public := &egress.Policy{}
	googleClient := public.Client(authTimeout)
	googleClient.Timeout = authTimeout
	googleClient.Transport = boundedAuthTransport{base: googleClient.Transport, policy: public}
	return &Auth{tokens: map[[32]byte]*cloudauth.Token{}, aws: map[[32]byte]aws.CredentialsProvider{}, client: client, googleClient: googleClient}
}

type boundedAuthTransport struct {
	base   http.RoundTripper
	policy *egress.Policy
}

func (t boundedAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Authentication URLs may carry queries. Google token endpoints use a
	// separate public-only policy; provider exceptions never relax that boundary.
	endpoint := *r.URL
	endpoint.RawQuery = ""
	endpoint.ForceQuery = false
	if _, e := t.policy.ValidateEndpoint(endpoint.String()); e != nil {
		return nil, ErrAuthentication
	}
	response, e := t.base.RoundTrip(r)
	if e != nil {
		return nil, e
	}
	response.Body = &boundedAuthBody{ReadCloser: response.Body, remaining: authBodyLimit}
	return response, nil
}

type boundedAuthBody struct {
	io.ReadCloser
	remaining int
}

func (b *boundedAuthBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var extra [1]byte
		n, e := b.ReadCloser.Read(extra[:])
		if n > 0 {
			return 0, ErrAuthentication
		}
		return 0, e
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, e := b.ReadCloser.Read(p)
	b.remaining -= n
	return n, e
}

func (a *Auth) Apply(ctx context.Context, req *http.Request, c Config, secret, body []byte) ([]string, error) {
	sensitive := []string{string(secret)}
	if c.Kind == "anthropic" {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	switch c.AuthMode {
	case "none":
		return nil, nil
	case "headers":
		if e := egress.ApplyCredentialHeaders(req.Header, c.CredentialHeaders, secret); e != nil {
			return nil, ErrAuthentication
		}
		for _, h := range c.CredentialHeaders {
			sensitive = append(sensitive, req.Header.Get(h))
		}
	case "api_key":
		if len(secret) == 0 || strings.ContainsAny(string(secret), "\r\n\x00") {
			return nil, ErrAuthentication
		}
		switch c.Kind {
		case "anthropic":
			req.Header.Set("X-Api-Key", string(secret))
		case "gemini":
			req.Header.Set("X-Goog-Api-Key", string(secret))
		case "azure_openai":
			req.Header.Set("Api-Key", string(secret))
		default:
			req.Header.Set("Authorization", "Bearer "+string(secret))
		}
	case "adc", "service_account":
		token, e := a.googleToken(ctx, c, secret)
		if e != nil {
			return nil, ErrAuthentication
		}
		req.Header.Set("Authorization", "Bearer "+token.Value)
		sensitive = append(sensitive, token.Value)
	case "static", "default_chain":
		creds, e := a.awsCredentials(ctx, c, secret)
		if e != nil {
			return nil, ErrAuthentication
		}
		hash := sha256.Sum256(body)
		if e := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(hash[:]), "bedrock", c.CloudRegion, time.Now()); e != nil {
			return nil, ErrAuthentication
		}
		sensitive = append(sensitive, creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken, req.Header.Get("Authorization"))
	default:
		return nil, ErrAuthentication
	}
	return sensitive, nil
}
func cacheKey(c Config, secret []byte) [32]byte {
	return sha256.Sum256(append([]byte(c.Kind+"\x00"+c.AuthMode+"\x00"+c.CloudRegion+"\x00"), secret...))
}

type credentialResult struct {
	credentials *cloudauth.Credentials
	err         error
}

func (a *Auth) googleToken(ctx context.Context, c Config, secret []byte) (*cloudauth.Token, error) {
	key := cacheKey(c, secret)
	a.mu.Lock()
	cached := a.tokens[key]
	a.mu.Unlock()
	if cached != nil && cached.Value != "" && cached.Expiry.After(time.Now().Add(30*time.Second)) {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	// Do not let environment-enabled SDK debug logs expose token exchanges.
	opts := &googlecredentials.DetectOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}, Client: a.googleClient, DisableAsyncRefresh: true, EarlyTokenRefresh: 30 * time.Second}
	var cred *cloudauth.Credentials
	var err error
	if c.AuthMode == "service_account" {
		cred, err = googlecredentials.NewCredentialsFromJSON(googlecredentials.ServiceAccount, secret, opts)
	} else {
		// ADC detection includes a bounded environment probe without a context API.
		// Start it once; canceled requests stop waiting without spawning more probes.
		a.mu.Lock()
		if a.adc == nil {
			a.adc = make(chan credentialResult, 1)
			result := a.adc
			go func() { creds, e := googlecredentials.DetectDefault(opts); result <- credentialResult{creds, e} }()
		}
		result := a.adc
		a.mu.Unlock()
		select {
		case value := <-result:
			result <- value
			cred, err = value.credentials, value.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, ErrAuthentication
	}
	token, err := cred.Token(ctx)
	if err != nil {
		return nil, ErrAuthentication
	}
	a.mu.Lock()
	if len(a.tokens) >= 256 {
		clear(a.tokens)
	}
	a.tokens[key] = token
	a.mu.Unlock()
	return token, nil
}
func (a *Auth) awsCredentials(ctx context.Context, c Config, secret []byte) (aws.Credentials, error) {
	if c.AuthMode == "static" {
		var v struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SessionToken    string `json:"session_token"`
		}
		d := json.NewDecoder(bytes.NewReader(secret))
		d.DisallowUnknownFields()
		if len(secret) > 16384 || d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || !secretComponent(v.AccessKeyID, 16, 256) || !secretComponent(v.SecretAccessKey, 16, 1024) || v.SessionToken != "" && !secretComponent(v.SessionToken, 1, 8192) {
			return aws.Credentials{}, ErrAuthentication
		}
		return credentials.NewStaticCredentialsProvider(v.AccessKeyID, v.SecretAccessKey, v.SessionToken).Retrieve(ctx)
	}
	key := cacheKey(c, secret)
	a.mu.Lock()
	provider := a.aws[key]
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	if provider == nil {
		cfg, e := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.CloudRegion), awsconfig.WithHTTPClient(a.client), awsconfig.WithRetryMaxAttempts(1), awsconfig.WithProcessCredentialOptions(func(o *processcreds.Options) { o.Timeout = authTimeout }))
		if e != nil {
			return aws.Credentials{}, ErrAuthentication
		}
		provider = cfg.Credentials
		a.mu.Lock()
		if len(a.aws) >= 256 {
			clear(a.aws)
		}
		a.aws[key] = provider
		a.mu.Unlock()
	}
	return provider.Retrieve(ctx)
}
func secretComponent(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
