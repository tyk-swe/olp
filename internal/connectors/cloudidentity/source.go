package cloudidentity

import (
	"errors"
	"net/http"
	"sync"
	"time"

	cloudauth "cloud.google.com/go/auth"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/tyk-swe/olp/internal/egress"
)

var ErrUnavailable = errors.New("provider authentication failed")

var ErrRejected error = rejected{}

type rejected struct{}

func (rejected) Error() string        { return ErrUnavailable.Error() }
func (rejected) Is(target error) bool { return target == ErrUnavailable }

const (
	Timeout      = 10 * time.Second
	earlyRefresh = 30 * time.Second
	cacheLimit   = 256
)

type Key [32]byte

type Credential struct {
	secret []byte
	stored bool
}

func Ambient() Credential { return Credential{} }

func Stored(secret []byte) Credential { return Credential{secret: secret, stored: true} }

type Dependencies struct {
	Getenv          func(string) string
	Metadata        http.RoundTripper
	Public          http.RoundTripper
	IAMEndpoint     string
	AzureCredential func(Credential) (azcore.TokenCredential, error)
}

type Source struct {
	getenv           func(string) string
	metadata         *http.Client
	public           *http.Client
	iamEndpoint      string
	azureCredential  func(Credential) (azcore.TokenCredential, error)
	applicationOnce  sync.Once
	application      chan detected
	googleTokens     cache[*cloudauth.Token]
	azureCredentials cache[azcore.TokenCredential]
	azureTokens      cache[azcore.AccessToken]
	awsProviders     cache[aws.CredentialsProvider]
	ibmTokens        cache[ibmToken]
}

func NewSource(d Dependencies) *Source {
	return &Source{
		getenv: d.Getenv, metadata: client(d.Metadata), public: client(d.Public),
		iamEndpoint: d.IAMEndpoint, azureCredential: d.AzureCredential,
	}
}

func client(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     transport,
		Timeout:       Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return egress.ErrRedirect },
	}
}

type cache[V any] struct {
	mu      sync.Mutex
	entries map[Key]V
}

func (c *cache[V]) get(key Key) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.entries[key]
	return value, ok
}

func (c *cache[V]) put(key Key, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= cacheLimit {
		c.entries = map[Key]V{}
	}
	c.entries[key] = value
}

func exchangeFailure(err error) error {
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
		return ErrRejected
	}
	return ErrUnavailable
}

func printable(s string, shortest, longest int) bool {
	if len(s) < shortest || len(s) > longest {
		return false
	}
	for _, c := range s {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
