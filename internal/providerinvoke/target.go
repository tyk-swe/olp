package providerinvoke

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type Target struct {
	Provider routing.Provider
	Slot     routing.Slot
	Model    string
}

func (t Target) ConnectionScope() string {
	credential := "none"
	if t.Slot.CredentialID != nil {
		credential = *t.Slot.CredentialID
	}
	return t.Provider.ID + "/" + t.Provider.RevisionID + "/" + t.Slot.ID + "/" + credential
}

type CallerCredential func(providerID, model string) ([]byte, error)

type Resolver struct {
	Credentials   release.Credentials
	Policy        *egress.Policy
	Connections   *egress.ConnectionClientCache
	DefaultClient *http.Client
	HeaderTimeout time.Duration
	Scope         string
}

type Binding struct {
	resolver Resolver
	release  *release.Release
	target   Target
	caller   CallerCredential
}

type Credential struct {
	Secret     []byte
	Generation int64
}

type Resolved struct {
	Credential    Credential
	Client        *http.Client
	NetworkSecret []byte
}

type ResolutionStage string

const (
	CredentialResolution ResolutionStage = "credential"
	NetworkResolution    ResolutionStage = "network"
	ConnectionResolution ResolutionStage = "connection"
)

type ResolutionError struct {
	Stage ResolutionStage
	Cause error
}

func (e *ResolutionError) Error() string { return e.Cause.Error() }
func (e *ResolutionError) Unwrap() error { return e.Cause }

func (r Resolver) Bind(pinned *release.Release, target Target, caller CallerCredential) Binding {
	return Binding{resolver: r, release: pinned, target: target, caller: caller}
}

func (b Binding) Resolve(ctx context.Context) (Resolved, error) {
	credential, err := b.Credential(ctx)
	if err != nil {
		return Resolved{}, &ResolutionError{Stage: CredentialResolution, Cause: err}
	}
	client, network, err := b.resolveClient(ctx)
	return Resolved{Credential: credential, Client: client, NetworkSecret: network}, err
}

func (b Binding) Credential(ctx context.Context) (Credential, error) {
	if b.target.Provider.CredentialSource == "caller" && b.caller != nil {
		secret, err := b.caller(b.target.Provider.ID, b.target.Model)
		return Credential{Secret: secret}, err
	}
	if b.target.Slot.CredentialID == nil {
		return Credential{}, nil
	}
	secret, generation, err := b.resolver.Credentials.Secret(ctx, b.release, *b.target.Slot.CredentialID)
	return Credential{Secret: secret, Generation: generation}, err
}

func (b Binding) NetworkSecret(ctx context.Context) ([]byte, error) {
	network := b.target.Provider.Network
	if network == nil || network.CredentialID == "" {
		return nil, nil
	}
	return b.resolver.Credentials.NetworkSecret(ctx, b.release, b.target.Provider.ID, network.CredentialID)
}

func (b Binding) Client(ctx context.Context) (*http.Client, error) {
	client, _, err := b.resolveClient(ctx)
	if failure, ok := err.(*ResolutionError); ok {
		return client, failure.Cause
	}
	return client, err
}

func (b Binding) resolveClient(ctx context.Context) (*http.Client, []byte, error) {
	provider := b.target.Provider
	if provider.CredentialSource != "caller" && provider.Network == nil && provider.ProfileID == "" && b.resolver.Scope == "" {
		return b.resolver.DefaultClient, nil, nil
	}
	secret, err := b.NetworkSecret(ctx)
	if err != nil {
		return nil, nil, &ResolutionError{Stage: NetworkResolution, Cause: err}
	}
	if provider.CredentialSource == "caller" {
		client, err := b.resolver.Policy.EphemeralClient(provider.Network, secret, b.resolver.HeaderTimeout)
		return resolvedClient(client, secret, err)
	}
	scope := b.target.ConnectionScope()
	if b.resolver.Scope != "" {
		scope = b.resolver.Scope
	}
	client, err := b.resolver.Connections.ClientScoped(scope, *b.resolver.Policy, provider.Network, secret, b.resolver.HeaderTimeout)
	return resolvedClient(client, secret, err)
}

func resolvedClient(client *http.Client, secret []byte, err error) (*http.Client, []byte, error) {
	if err != nil {
		return nil, secret, &ResolutionError{Stage: ConnectionResolution, Cause: err}
	}
	return client, secret, nil
}
