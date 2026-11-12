package probe

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

const (
	Timeout     = 15 * time.Second
	BodyLimit   = 1 << 20
	concurrency = 4
)

type NetworkSecrets interface {
	NetworkSecret(ctx context.Context, providerID, credentialID string) ([]byte, error)
}

type Prober struct {
	egress      *egress.Policy
	auth        *connectors.Auth
	plugins     *plugins.Host
	secrets     NetworkSecrets
	client      *http.Client
	connections *egress.ConnectionClientCache
	slots       chan struct{}
}

func New(policy *egress.Policy, signer connectors.Signer, carriers *plugins.Host, secrets NetworkSecrets) *Prober {
	auth := connectors.NewAuth(policy)
	auth.Signer = signer
	return &Prober{
		egress:      policy,
		auth:        auth,
		plugins:     carriers,
		secrets:     secrets,
		client:      policy.Client(Timeout),
		connections: egress.NewConnectionClientCache(128),
		slots:       make(chan struct{}, concurrency),
	}
}

type Target struct {
	ProviderID  string
	Connection  connection.Prepared
	Credential  []byte
	ProbeModels []string
}

// Budget bounds a sequence of probes by the caller's deadline less a reserve
// for recording the outcome: one Timeout, or half the remaining time when less
// than two remain.
func Budget(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	reserve := min(Timeout, time.Until(deadline)/2)
	return context.WithDeadline(ctx, deadline.Add(-max(reserve, 0)))
}

// Client is the connection the provider's traffic leaves through, scoped to
// its transport and credential so a changed network credential never reuses
// a pooled connection.
func (p *Prober) Client(ctx context.Context, t Target) (*http.Client, error) {
	document := t.Connection.Document()
	if document.Options.Network == nil && document.ProfileID == "" {
		return p.client, nil
	}
	if t.ProviderID == "" {
		return nil, errors.New("provider identity is required for configured connections")
	}
	var secret []byte
	if document.Options.Network != nil && document.Options.Network.CredentialID != "" {
		read, err := p.secrets.NetworkSecret(ctx, t.ProviderID, document.Options.Network.CredentialID)
		if err != nil {
			return nil, err
		}
		secret = read
	}
	digest := sha256.Sum256(t.Credential)
	scope := t.ProviderID + "/" + document.Fingerprint() + "/" + hex.EncodeToString(digest[:])
	return p.connections.ClientScoped(scope, *p.egress, document.Options.Network, secret, Timeout)
}

type request struct {
	method         string
	path           string
	body           []byte
	contentType    string
	signingService string
	header         http.Header
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (p *Prober) call(ctx context.Context, t Target, method, path string, body []byte) (int, []byte, error) {
	resp, err := p.do(ctx, t, request{method: method, path: path, body: body})
	return resp.status, resp.body, err
}

func (p *Prober) do(ctx context.Context, t Target, r request) (response, error) {
	base, err := p.egress.ValidateEndpoint(providerconfig.Value(t.Connection.Document().Endpoint))
	if err != nil {
		return response{}, &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
	}
	endpoint, err := p.resolve(base, r.path)
	if err != nil {
		return response{}, err
	}
	ctx, release, err := p.slot(ctx)
	if err != nil {
		return response{}, err
	}
	defer release()
	var reader io.Reader
	if r.body != nil {
		reader = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, endpoint, reader)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "olp/probe")
	if r.body != nil {
		req.Header.Set("Content-Type", cmp.Or(r.contentType, "application/json"))
	}
	for name, values := range r.header {
		req.Header[name] = values
	}
	transport := t.Connection.Transport()
	transport.SigningService = r.signingService
	sensitive, err := p.auth.Apply(ctx, req, transport, t.Credential, r.body)
	switch {
	case errors.Is(err, connectors.ErrSigningUnavailable):
		return response{}, &Failure{Reason: UpstreamUnavailable, Detail: err.Error()}
	case err != nil:
		return response{}, &Failure{Reason: CredentialInvalid, Detail: err.Error()}
	}
	client, err := p.carrier(ctx, t, transport, sensitive.Values())
	if err != nil {
		return response{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return response{}, Classify(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, BodyLimit+1))
	if err != nil {
		return response{status: resp.StatusCode, header: resp.Header.Clone()}, Classify(err)
	}
	if len(data) > BodyLimit {
		return response{status: resp.StatusCode, header: resp.Header.Clone()}, &Failure{Reason: UpstreamResponseTooLarge, Detail: "The upstream response exceeded the probe body limit."}
	}
	return response{status: resp.StatusCode, header: resp.Header.Clone(), body: data}, nil
}

func (p *Prober) resolve(base *url.URL, path string) (string, error) {
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		return base.String() + path, nil
	}
	u, err := url.Parse(path)
	if err != nil {
		return "", err
	}
	u.RawQuery = ""
	if _, err := p.egress.ValidateEndpoint(u.String()); err != nil {
		return "", err
	}
	return path, nil
}

func (p *Prober) carrier(ctx context.Context, t Target, transport connectors.Config, secrets []string) (*http.Client, error) {
	if !transport.CarriedByPlugin() {
		client, err := p.Client(ctx, t)
		if err != nil {
			return nil, &Failure{Reason: NetworkCredentialInvalid, Detail: "The configured provider network connection is unavailable."}
		}
		return client, nil
	}
	if p.plugins == nil {
		return nil, &Failure{Reason: UpstreamUnavailable, Detail: "This process runs no plugins that carry traffic."}
	}
	return transport.CarrierClient(p.plugins, secrets), nil
}

func anthropicFilesBeta() http.Header {
	return http.Header{"Anthropic-Beta": {"files-api-2025-04-14"}}
}

func (p *Prober) slot(ctx context.Context) (context.Context, func(), error) {
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx, func() {}, ctx.Err()
	}
	bounded, cancel := context.WithTimeout(ctx, Timeout)
	return bounded, func() { cancel(); <-p.slots }, nil
}
