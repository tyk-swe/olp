package reconcile

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
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
	provider, route, slot, err := w.deps.Resolver.ResolveCurrent(ctx, &resource, "batch")
	if errors.Is(err, resources.ErrUnavailable) {
		return upstream.Target{}, errCredentialUnavailable
	}
	if err != nil {
		return upstream.Target{}, errRuntimeUnavailable
	}
	index := slices.IndexFunc(route.Targets, func(target routing.Target) bool {
		return target.ID == settlement.Binding.TargetID && target.ProviderID == settlement.Binding.ProviderID
	})
	if index < 0 || route.Targets[index].ProviderModel == "" {
		return upstream.Target{}, errRuntimeUnavailable
	}
	config := provider.Connector()
	if config.CredentialSource == "caller" {
		return upstream.Target{}, errCredentialUnavailable
	}
	secret, err := w.slotSecret(ctx, slot, config)
	if err != nil {
		return upstream.Target{}, err
	}
	client, err := w.client(ctx, provider, slot)
	if err != nil {
		return upstream.Target{}, err
	}
	return upstream.Target{Config: config, Model: route.Targets[index].ProviderModel, Secret: secret, Client: client}, nil
}

func (w *Worker) slotSecret(ctx context.Context, slot *routing.Slot, config connectors.Config) ([]byte, error) {
	if slot.CredentialID == nil {
		if connectors.SecretRequired(config.AuthMode) {
			return nil, errCredentialUnavailable
		}
		return nil, nil
	}
	if w.deps.Credentials.Eligibility(*slot.CredentialID) != routing.Eligible {
		return nil, errCredentialUnavailable
	}
	secret, _, err := w.deps.Credentials.Secret(ctx, nil, *slot.CredentialID)
	if err != nil {
		return nil, errors.Join(errCredentialUnavailable, err)
	}
	return secret, nil
}

func (w *Worker) client(ctx context.Context, provider *routing.Provider, slot *routing.Slot) (*http.Client, error) {
	if provider.Network == nil && provider.ProfileID == "" {
		return w.deps.Client, nil
	}
	networkSecret, err := w.networkSecret(ctx, provider)
	if err != nil {
		return nil, err
	}
	credentialID := "none"
	if slot.CredentialID != nil {
		credentialID = *slot.CredentialID
	}
	scope := provider.ID + "/" + provider.RevisionID + "/" + slot.ID + "/" + credentialID
	client, err := w.deps.Connections.ClientScoped(scope, *w.deps.Egress, provider.Network, networkSecret, clientTimeout)
	if err != nil {
		return nil, errors.Join(errRuntimeUnavailable, err)
	}
	return client, nil
}

func (w *Worker) networkSecret(ctx context.Context, provider *routing.Provider) ([]byte, error) {
	if provider.Network == nil || provider.Network.CredentialID == "" {
		return nil, nil
	}
	if w.deps.Credentials.Eligibility(provider.Network.CredentialID) != routing.Eligible {
		return nil, errCredentialUnavailable
	}
	secret, err := w.deps.Credentials.NetworkSecret(ctx, nil, provider.ID, provider.Network.CredentialID)
	if err != nil {
		return nil, errors.Join(errCredentialUnavailable, err)
	}
	return secret, nil
}
