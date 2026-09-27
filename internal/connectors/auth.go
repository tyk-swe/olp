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
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"

	"github.com/tyk-swe/olp/internal/egress"
)

// ErrAuthentication reports that authentication could not authorize an
// upstream request. Every authentication failure matches it. Unless it also
// matches ErrCredentialRejected, the failure may pass on a later attempt, such
// as a token that could not be fetched.
var ErrAuthentication = errors.New("provider authentication failed")

// ErrCredentialRejected reports a credential that cannot authorize requests
// until it changes: it is malformed, or its authority refused it.
var ErrCredentialRejected error = credentialRejected{}

type credentialRejected struct{}

func (credentialRejected) Error() string        { return ErrAuthentication.Error() }
func (credentialRejected) Is(target error) bool { return target == ErrAuthentication }

const authTimeout = 10 * time.Second
const authBodyLimit = 1 << 20

type Auth struct {
	mu           sync.Mutex
	tokens       map[[32]byte]*cloudauth.Token
	aws          map[[32]byte]aws.CredentialsProvider
	azure        map[[32]byte]azcore.TokenCredential
	azureTokens  map[[32]byte]azcore.AccessToken
	client       *http.Client
	googleClient *http.Client
	azureClient  *http.Client
	adc          chan credentialResult
	azureFactory func(mode string, secret []byte) (azcore.TokenCredential, error)
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

	azurePolicy := *policy
	azurePolicy.AllowedNetworks = append([]netip.Prefix{}, policy.AllowedNetworks...)
	azurePolicy.PlainHTTPHosts = append([]string{}, policy.PlainHTTPHosts...)
	imds := netip.MustParseAddr("169.254.169.254")
	azurePolicy.AllowedNetworks = append(azurePolicy.AllowedNetworks, netip.PrefixFrom(imds, imds.BitLen()))
	azurePolicy.PlainHTTPHosts = append(azurePolicy.PlainHTTPHosts, "169.254.169.254")
	for _, name := range []string{"IDENTITY_ENDPOINT", "MSI_ENDPOINT", "IMDS_ENDPOINT"} {
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		endpoint, err := url.Parse(raw)
		if err != nil || endpoint.Hostname() == "" {
			continue
		}
		host := strings.ToLower(endpoint.Hostname())
		if endpoint.Scheme == "http" {
			azurePolicy.PlainHTTPHosts = append(azurePolicy.PlainHTTPHosts, host)
		}
		if address, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
			azurePolicy.AllowedNetworks = append(azurePolicy.AllowedNetworks, netip.PrefixFrom(address.Unmap(), address.Unmap().BitLen()))
		}
	}
	azureClient := azurePolicy.Client(authTimeout)
	azureClient.Timeout = authTimeout
	azureClient.Transport = azureIdentityTransport{base: boundedAuthTransport{base: azureClient.Transport, policy: &azurePolicy}}
	return &Auth{tokens: map[[32]byte]*cloudauth.Token{}, aws: map[[32]byte]aws.CredentialsProvider{},
		azure: map[[32]byte]azcore.TokenCredential{}, azureTokens: map[[32]byte]azcore.AccessToken{},
		client: client, googleClient: googleClient, azureClient: azureClient}
}

var azureAuthorityHosts = map[string]bool{
	"login.microsoftonline.com":              true,
	"login.microsoftonline.us":               true,
	"login.chinacloudapi.cn":                 true,
	"login.microsoftonline.eaglex.ic.gov":    true,
	"login.microsoftonline.microsoft.scloud": true,
	"login.microsoft.com":                    true,
}

type azureIdentityTransport struct {
	base http.RoundTripper
}

func (t azureIdentityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	host := strings.ToLower(strings.TrimSuffix(r.URL.Hostname(), "."))
	if !azureIdentityHost(host) {
		return nil, ErrAuthentication
	}
	return t.base.RoundTrip(r)
}

func azureIdentityHost(host string) bool {
	if azureAuthorityHosts[host] || host == "169.254.169.254" {
		return true
	}
	for _, name := range []string{"IDENTITY_ENDPOINT", "MSI_ENDPOINT", "IMDS_ENDPOINT", "AZURE_AUTHORITY_HOST"} {
		raw := os.Getenv(name)
		if raw == "" {
			continue
		}
		if endpoint, err := url.Parse(raw); err == nil && strings.EqualFold(endpoint.Hostname(), host) {
			return true
		}
	}
	return false
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

// Apply prepares an upstream request in three stages, which every upstream
// call path runs in this order:
//
//  1. Hosting places the request. The caller addressed it from the connector;
//     hosting adds its semantic headers, query settings and version headers,
//     and a plugin profile's declared headers and query parameters, which may
//     carry the credential.
//  2. Authentication authorizes it, using the authenticator registered for
//     the connector's auth mode.
//  3. Signing runs last, over the finished request and its body.
//
// It returns the values to redact wherever upstream text is recorded.
func (a *Auth) Apply(ctx context.Context, req *http.Request, c Config, secret, body []byte) ([]string, error) {
	placed, err := c.host(req, secret)
	if err != nil {
		return nil, err
	}
	authenticator, ok := authenticators[c.AuthMode]
	if !ok {
		return nil, ErrCredentialRejected
	}
	authorized, err := authenticator.authenticate(a, ctx, req, c, secret)
	if err != nil {
		return nil, err
	}
	sensitive := append(append([]string{string(secret)}, placed...), authorized.sensitive...)
	if authorized.sign == nil {
		return sensitive, nil
	}
	signed, err := authorized.sign(ctx, req, body)
	if err != nil {
		return nil, err
	}
	return append(sensitive, signed...), nil
}

// An authenticator authorizes upstream requests for one auth mode.
type authenticator struct {
	// credential reports whether the mode authorizes with a stored credential
	// secret rather than ambient credentials or none.
	credential bool
	// authenticate adds the mode's authorization to a placed request.
	authenticate func(a *Auth, ctx context.Context, req *http.Request, c Config, secret []byte) (authorization, error)
}

// An authorization is what authentication leaves for the rest of the request's
// preparation.
type authorization struct {
	// sensitive holds the values authentication added that must be redacted.
	sensitive []string
	// sign, when set, runs over the finished request and returns the values it
	// added that must be redacted.
	sign func(ctx context.Context, req *http.Request, body []byte) ([]string, error)
}

// authenticators registers the authenticator for each auth mode. A provider
// kind lists the modes its validation admits; a mode without an authenticator
// never authorizes a request.
var authenticators = map[string]authenticator{
	"none":                {authenticate: (*Auth).authenticateNone},
	"api_key":             {credential: true, authenticate: (*Auth).authenticateAPIKey},
	"headers":             {credential: true, authenticate: (*Auth).authenticateHeaders},
	"adc":                 {authenticate: (*Auth).authenticateGoogle},
	"service_account":     {credential: true, authenticate: (*Auth).authenticateGoogle},
	"azure_default":       {authenticate: (*Auth).authenticateAzure},
	"azure_client_secret": {credential: true, authenticate: (*Auth).authenticateAzure},
	"default_chain":       {authenticate: (*Auth).authenticateAWS},
	"static":              {credential: true, authenticate: (*Auth).authenticateAWS},
	AuthStaticCredential:  {credential: true, authenticate: (*Auth).authenticatePlaced},
	AuthGrant:             {credential: true, authenticate: (*Auth).authenticatePlaced},
}

// SecretRequired reports whether an auth mode authorizes with a stored
// credential secret. An unregistered mode requires one.
func SecretRequired(mode string) bool {
	authenticator, ok := authenticators[mode]
	return !ok || authenticator.credential
}

// authenticateNone sends no authorization.
func (*Auth) authenticateNone(context.Context, *http.Request, Config, []byte) (authorization, error) {
	return authorization{}, nil
}

// authenticatePlaced adds no authorization of its own: the plugin profile's
// hosting adaptation placed the static credential or the grant's access token.
func (*Auth) authenticatePlaced(context.Context, *http.Request, Config, []byte) (authorization, error) {
	return authorization{}, nil
}

// authenticateAPIKey sends the key in the header the provider kind expects.
func (*Auth) authenticateAPIKey(_ context.Context, req *http.Request, c Config, secret []byte) (authorization, error) {
	if len(secret) == 0 || strings.ContainsAny(string(secret), "\r\n\x00") {
		return authorization{}, ErrCredentialRejected
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
	return authorization{}, nil
}

// authenticateHeaders sends the encrypted header values the connector names.
func (*Auth) authenticateHeaders(_ context.Context, req *http.Request, c Config, secret []byte) (authorization, error) {
	if err := egress.ApplyCredentialHeaders(req.Header, c.CredentialHeaders, secret); err != nil {
		return authorization{}, ErrCredentialRejected
	}
	var sensitive []string
	for _, h := range c.CredentialHeaders {
		sensitive = append(sensitive, req.Header.Get(h))
	}
	return authorization{sensitive: sensitive}, nil
}

// authenticateGoogle sends an access token from application default
// credentials or a service-account key.
func (a *Auth) authenticateGoogle(ctx context.Context, req *http.Request, c Config, secret []byte) (authorization, error) {
	token, err := a.googleToken(ctx, c, secret)
	if err != nil {
		return authorization{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token.Value)
	return authorization{sensitive: []string{token.Value}}, nil
}

// authenticateAzure sends a Microsoft Entra access token.
func (a *Auth) authenticateAzure(ctx context.Context, req *http.Request, c Config, secret []byte) (authorization, error) {
	token, err := a.azureToken(ctx, c, secret)
	if err != nil {
		return authorization{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token.Token)
	return authorization{sensitive: []string{token.Token}}, nil
}

// authenticateAWS resolves AWS credentials, which sign the finished request
// with SigV4.
func (a *Auth) authenticateAWS(ctx context.Context, _ *http.Request, c Config, secret []byte) (authorization, error) {
	creds, err := a.awsCredentials(ctx, c, secret)
	if err != nil {
		return authorization{}, err
	}
	sign := func(ctx context.Context, req *http.Request, body []byte) ([]string, error) {
		hash := sha256.Sum256(body)
		if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(hash[:]), "bedrock", c.CloudRegion, time.Now()); err != nil {
			return nil, ErrAuthentication
		}
		return []string{req.Header.Get("Authorization")}, nil
	}
	return authorization{sensitive: []string{creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken}, sign: sign}, nil
}

// tokenFailure classifies a failure to fetch a token or cloud credentials. A
// refusal from the credential authority, such as an invalid client or grant,
// rejects the credential; any other failure, such as an unreachable
// authority, may pass on a later attempt.
func tokenFailure(err error) error {
	status := 0
	if e, ok := errors.AsType[*cloudauth.Error](err); ok && e.Response != nil {
		status = e.Response.StatusCode
	} else if e, ok := errors.AsType[*azidentity.AuthenticationFailedError](err); ok && e.RawResponse != nil {
		status = e.RawResponse.StatusCode
	} else if e, ok := errors.AsType[interface {
		error
		HTTPStatusCode() int
	}](err); ok {
		status = e.HTTPStatusCode()
	}
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
		return ErrCredentialRejected
	}
	return ErrAuthentication
}

func cacheKey(c Config, secret []byte) [32]byte {
	return sha256.Sum256(append([]byte(c.Kind+"\x00"+c.AuthMode+"\x00"+c.CloudRegion+"\x00"+c.CloudProject+"\x00"+c.ProfileID+"\x00"+c.ProfileRevision+"\x00"+c.AzureScope()+"\x00"), secret...))
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
			return nil, ErrAuthentication
		}
	}
	if err != nil {
		return nil, ErrCredentialRejected
	}
	token, err := cred.Token(ctx)
	if err != nil {
		return nil, tokenFailure(err)
	}
	a.mu.Lock()
	if len(a.tokens) >= 256 {
		clear(a.tokens)
	}
	a.tokens[key] = token
	a.mu.Unlock()
	return token, nil
}

const azureScope = "https://cognitiveservices.azure.com/.default"

func (a *Auth) azureToken(ctx context.Context, c Config, secret []byte) (azcore.AccessToken, error) {
	key := cacheKey(c, secret)
	a.mu.Lock()
	cached := a.azureTokens[key]
	a.mu.Unlock()
	if cached.Token != "" && cached.ExpiresOn.After(time.Now().Add(30*time.Second)) {
		return cached, nil
	}
	credential, e := a.azureCredential(c, secret, key)
	if e != nil {
		return azcore.AccessToken{}, ErrCredentialRejected
	}
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{c.AzureScope()}})
	if err != nil {
		return azcore.AccessToken{}, tokenFailure(err)
	}
	a.mu.Lock()
	if len(a.azureTokens) >= 256 {
		clear(a.azureTokens)
	}
	a.azureTokens[key] = token
	a.mu.Unlock()
	return token, nil
}

func (a *Auth) azureCredential(c Config, secret []byte, key [32]byte) (azcore.TokenCredential, error) {
	a.mu.Lock()
	credential := a.azure[key]
	a.mu.Unlock()
	if credential != nil {
		return credential, nil
	}
	credential, err := a.newAzureCredential(c.AuthMode, secret)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if len(a.azure) >= 256 {
		clear(a.azure)
	}
	a.azure[key] = credential
	a.mu.Unlock()
	return credential, nil
}

func (a *Auth) newAzureCredential(mode string, secret []byte) (azcore.TokenCredential, error) {
	if a.azureFactory != nil {
		return a.azureFactory(mode, secret)
	}
	options := azcore.ClientOptions{Transport: a.azureClient}
	switch mode {
	case "azure_default":
		return azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{ClientOptions: options})
	case "azure_client_secret":
		var v struct {
			TenantID     string `json:"tenant_id"`
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
		}
		d := json.NewDecoder(bytes.NewReader(secret))
		d.DisallowUnknownFields()
		if len(secret) > 16384 || d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF ||
			!secretComponent(v.TenantID, 1, 128) || !secretComponent(v.ClientID, 1, 128) ||
			!secretComponent(v.ClientSecret, 1, 1024) {
			return nil, ErrCredentialRejected
		}
		return azidentity.NewClientSecretCredential(v.TenantID, v.ClientID, v.ClientSecret, &azidentity.ClientSecretCredentialOptions{ClientOptions: options})
	}
	return nil, ErrCredentialRejected
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
			return aws.Credentials{}, ErrCredentialRejected
		}
		creds, err := credentials.NewStaticCredentialsProvider(v.AccessKeyID, v.SecretAccessKey, v.SessionToken).Retrieve(ctx)
		if err != nil {
			return aws.Credentials{}, ErrCredentialRejected
		}
		return creds, nil
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
			return aws.Credentials{}, ErrCredentialRejected
		}
		provider = cfg.Credentials
		a.mu.Lock()
		if len(a.aws) >= 256 {
			clear(a.aws)
		}
		a.aws[key] = provider
		a.mu.Unlock()
	}
	creds, err := provider.Retrieve(ctx)
	if err != nil {
		return aws.Credentials{}, tokenFailure(err)
	}
	return creds, nil
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
