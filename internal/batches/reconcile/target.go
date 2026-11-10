package reconcile

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

const clientTimeout = 5 * time.Minute

var (
	errRuntimeUnavailable    = errors.New("provider resource runtime is unavailable")
	errCredentialUnavailable = errors.New("provider resource credential is unavailable")
)

func targetFailure(err error) batches.ErrorClass {
	if errors.Is(err, errCredentialUnavailable) {
		return errorCredentialUnavailable
	}
	return errorRuntimeUnavailable
}

func (w *Worker) target(ctx context.Context, settlement batches.Settlement) (upstream.Target, error) {
	resource := settlement.Binding.Resource(settlement.ResourceID, settlement.UpstreamID)
	pin, err := w.deps.Resolver.ResolveCurrent(ctx, &resource, "batch")
	if errors.Is(err, resources.ErrUnavailable) {
		return upstream.Target{}, errCredentialUnavailable
	}
	if err != nil {
		return upstream.Target{}, errRuntimeUnavailable
	}
	provider, slot := pin.Invocation.Provider, pin.Invocation.Slot
	config := provider.Connector()
	if config.CredentialSource == "caller" {
		return upstream.Target{}, errCredentialUnavailable
	}
	if slot.CredentialID == nil && connectors.SecretRequired(config.AuthMode) || slot.CredentialID != nil && w.deps.Credentials.Eligibility(*slot.CredentialID) != routing.Eligible {
		return upstream.Target{}, errCredentialUnavailable
	}
	if provider.Network != nil && provider.Network.CredentialID != "" && w.deps.Credentials.Eligibility(provider.Network.CredentialID) != routing.Eligible {
		return upstream.Target{}, errCredentialUnavailable
	}
	resolver := providerinvoke.Resolver{Credentials: w.deps.Credentials, Policy: w.deps.Egress,
		Connections: w.deps.Connections, DefaultClient: w.deps.Client, HeaderTimeout: clientTimeout}
	target, err := resolver.Bind(nil, pin.Invocation, nil).Resolve(ctx)
	if err != nil {
		var failure *providerinvoke.ResolutionError
		if errors.As(err, &failure) && failure.Stage != providerinvoke.ConnectionResolution {
			return upstream.Target{}, errors.Join(errCredentialUnavailable, err)
		}
		return upstream.Target{}, errors.Join(errRuntimeUnavailable, err)
	}
	return upstream.Target{Config: config, Model: pin.Invocation.Model, Secret: target.Credential.Secret, Client: target.Client}, nil
}
