package connectors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/tyk-swe/olp/internal/connectors/cloudidentity"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/vendors"
)

// Every authentication failure matches ErrAuthentication. One that also
// matches ErrCredentialRejected blames the credential, and any other may pass
// on a later attempt.
var (
	ErrAuthentication     = cloudidentity.ErrUnavailable
	ErrCredentialRejected = cloudidentity.ErrRejected
)

type Auth struct {
	// Without a Signer, a request of a plugin profile that declares signing fails authentication.
	Signer Signer
	cloud  *cloudidentity.Source
}

func NewAuth(policy *egress.Policy, getenv func(string) string) *Auth {
	return newAuth(cloudidentity.Standard(policy, getenv))
}

func newAuth(dependencies cloudidentity.Dependencies) *Auth {
	return &Auth{cloud: cloudidentity.NewSource(dependencies)}
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
//  3. Signing runs last, over the finished request and its body: the auth
//     mode's signature, such as SigV4, then a plugin profile's signing hook.
//
// It returns the values to redact wherever upstream text is recorded.
func (a *Auth) Apply(ctx context.Context, req *http.Request, c Config, secret, body []byte) (egress.Sensitive, error) {
	if c.Kind == "vertex_ai" && !c.vertexDestination(req.URL) {
		return egress.Sensitive{}, ErrAuthentication
	}
	if c.CredentialSource == "caller" {
		if err := ValidateCredentialSource(c.CredentialSource, c.Kind, c.AuthMode); err != nil {
			return egress.Sensitive{}, ErrCredentialRejected
		}
	}
	placed, err := c.host(req, secret)
	if err != nil {
		return egress.Sensitive{}, err
	}
	authenticator, ok := authenticators[c.AuthMode]
	if !ok {
		return egress.Sensitive{}, ErrCredentialRejected
	}
	authorized, err := authenticator.authenticate(a, ctx, req, c, secret)
	if err != nil {
		return egress.Sensitive{}, err
	}
	sensitive := append(append([]string{string(secret)}, placed...), authorized.sensitive...)
	if authorized.sign != nil {
		signed, err := authorized.sign(ctx, req, body)
		if err != nil {
			return egress.Sensitive{}, err
		}
		sensitive = append(sensitive, signed...)
	}
	if c.Plugin != nil {
		signed, err := c.Plugin.sign(ctx, a.Signer, c.PluginOptions, req, secret, body, sensitive)
		if err != nil {
			return egress.Sensitive{}, err
		}
		sensitive = append(sensitive, signed...)
	}
	var applied egress.Sensitive
	applied.Add(sensitive...)
	return applied, nil
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

const (
	AuthNone              = "none"
	AuthAPIKey            = "api_key"
	AuthHeaders           = "headers"
	AuthADC               = "adc"
	AuthServiceAccount    = "service_account"
	AuthAzureDefault      = "azure_default"
	AuthAzureClientSecret = "azure_client_secret"
	AuthDefaultChain      = "default_chain"
	AuthStatic            = "static"
	AuthIBMIAM            = "ibm_iam"
)

// authenticators registers the authenticator for each auth mode. A provider
// kind lists the modes its validation admits; a mode without an authenticator
// never authorizes a request.
var authenticators = map[string]authenticator{
	AuthNone:              {authenticate: (*Auth).authenticateNone},
	AuthAPIKey:            {credential: true, authenticate: (*Auth).authenticateAPIKey},
	AuthHeaders:           {credential: true, authenticate: (*Auth).authenticateHeaders},
	AuthADC:               {authenticate: (*Auth).authenticateGoogle},
	AuthServiceAccount:    {credential: true, authenticate: (*Auth).authenticateGoogle},
	AuthAzureDefault:      {authenticate: (*Auth).authenticateAzure},
	AuthAzureClientSecret: {credential: true, authenticate: (*Auth).authenticateAzure},
	AuthDefaultChain:      {authenticate: (*Auth).authenticateAWS},
	AuthStatic:            {credential: true, authenticate: (*Auth).authenticateAWS},
	AuthIBMIAM:            {credential: true, authenticate: (*Auth).authenticateIBM},
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
	case "azure_openai", KindAzureAISearch:
		req.Header.Set("Api-Key", string(secret))
	default:
		if placement, ok := vendors.CredentialFor(c.VendorID); ok {
			req.Header.Set(placement.Header, placement.Scheme+string(secret))
			break
		}
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

func (a *Auth) authenticateGoogle(ctx context.Context, req *http.Request, c Config, secret []byte) (authorization, error) {
	token, err := a.googleToken(ctx, c, secret)
	if err != nil {
		return authorization{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return authorization{sensitive: []string{token}}, nil
}

func (a *Auth) authenticateAzure(ctx context.Context, req *http.Request, c Config, secret []byte) (authorization, error) {
	token, err := a.azureToken(ctx, c, secret, c.AzureScope())
	if err != nil {
		return authorization{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return authorization{sensitive: []string{token}}, nil
}

func (a *Auth) authenticateIBM(ctx context.Context, req *http.Request, c Config, secret []byte) (authorization, error) {
	token, err := a.cloud.IBMToken(ctx, cacheKey(c, secret), secret)
	if err != nil {
		return authorization{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return authorization{sensitive: []string{token}}, nil
}

// authenticateAWS resolves AWS credentials, which sign the finished request
// with SigV4, or sign the bearer token a hosting accepts instead.
func (a *Auth) authenticateAWS(ctx context.Context, req *http.Request, c Config, secret []byte) (authorization, error) {
	creds, err := a.awsCredentials(ctx, c, secret)
	if err != nil {
		return authorization{}, err
	}
	if c.traits().awsBearer {
		token, err := sagemakerToken(ctx, creds, c.CloudRegion, time.Now())
		if err != nil {
			return authorization{}, ErrAuthentication
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return authorization{sensitive: []string{creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken, token}}, nil
	}
	sign := func(ctx context.Context, req *http.Request, body []byte) ([]string, error) {
		hash := sha256.Sum256(body)
		signer := v4.NewSigner(func(options *v4.SignerOptions) {
			options.DisableURIPathEscaping = c.awsService() == "s3"
		})
		if err := signer.SignHTTP(ctx, creds, req, hex.EncodeToString(hash[:]), c.awsService(), c.CloudRegion, time.Now()); err != nil {
			return nil, ErrAuthentication
		}
		header := req.Header.Get("Authorization")
		return []string{header, sigV4Signature(header)}, nil
	}
	return authorization{sensitive: []string{creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken}, sign: sign}, nil
}

const azureStorageScope = "https://storage.azure.com/.default"

func (a *Auth) ApplyAzureStorage(ctx context.Context, req *http.Request, mode string, secret []byte) (egress.Sensitive, error) {
	if !storageDestination(req.URL, "azure_blob") {
		return egress.Sensitive{}, ErrAuthentication
	}
	if mode != AuthAzureDefault && mode != AuthAzureClientSecret {
		return egress.Sensitive{}, ErrCredentialRejected
	}
	token, err := a.azureToken(ctx, Config{Kind: "azure_blob", AuthMode: mode}, secret, azureStorageScope)
	if err != nil {
		return egress.Sensitive{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	var sensitive egress.Sensitive
	sensitive.Add(string(secret), token)
	return sensitive, nil
}

func sigV4Signature(authorization string) string {
	_, parameter, found := strings.Cut(authorization, "Signature=")
	if !found {
		return ""
	}
	signature, _, _ := strings.Cut(parameter, ",")
	return signature
}

func cacheKey(c Config, secret []byte) cloudidentity.Key {
	return sha256.Sum256(append([]byte(c.Kind+"\x00"+c.AuthMode+"\x00"+c.CloudRegion+"\x00"+c.CloudProject+"\x00"+c.ProfileID+"\x00"+c.ProfileRevision+"\x00"+c.AzureScope()+"\x00"), secret...))
}

func identityOf(c Config, storedMode string, secret []byte) cloudidentity.Credential {
	if c.AuthMode == storedMode {
		return cloudidentity.Stored(secret)
	}
	return cloudidentity.Ambient()
}

func (a *Auth) googleToken(ctx context.Context, c Config, secret []byte) (string, error) {
	return a.cloud.GoogleToken(ctx, cacheKey(c, secret), identityOf(c, AuthServiceAccount, secret))
}

func (a *Auth) azureToken(ctx context.Context, c Config, secret []byte, scope string) (string, error) {
	return a.cloud.AzureToken(ctx, cacheKey(c, secret), identityOf(c, AuthAzureClientSecret, secret), scope)
}

func (a *Auth) awsCredentials(ctx context.Context, c Config, secret []byte) (aws.Credentials, error) {
	return a.cloud.AWSCredentials(ctx, cacheKey(c, secret), identityOf(c, AuthStatic, secret), c.CloudRegion)
}

func (a *Auth) AWSWorkloadCredentials(ctx context.Context, region string) (aws.Credentials, error) {
	return a.cloud.AWSWorkloadCredentials(ctx, cacheKey(Config{AuthMode: AuthDefaultChain, CloudRegion: region}, nil), region)
}
