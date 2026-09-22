package gateway

import (
	"context"
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/runtime"
)

func (s *Server) providerNetworkSecret(ctx context.Context, release *runtime.Release, provider *runtime.Provider) ([]byte, error) {
	if provider.Network == nil || provider.Network.CredentialID == "" {
		return nil, nil
	}
	id := provider.Network.CredentialID
	if s.Runtime.Revoked(id) {
		return nil, errors.New("provider network credential is revoked or authority is stale")
	}
	if secret, ok := release.Credential(id); ok {
		return secret, nil
	}
	// A resource can retain a compatible historical profile and network
	// credential after the current release changed. Current revocation wins.
	if s.Resolver == nil {
		return nil, errors.New("provider network credential is unavailable")
	}
	return s.Resolver.NetworkCredential(ctx, provider.ID, id)
}

func providerConnectionScope(provider *runtime.Provider, slot runtime.Slot) string {
	credential := "none"
	if slot.CredentialID != nil {
		credential = *slot.CredentialID
	}
	return provider.ID + "/" + provider.RevisionID + "/" + slot.ID + "/" + credential
}

func (s *Server) providerClient(ctx context.Context, release *runtime.Release, provider *runtime.Provider, slot runtime.Slot) (*http.Client, error) {
	if provider.Network == nil && provider.ProfileID == "" {
		return s.client, nil
	}
	secret, err := s.providerNetworkSecret(ctx, release, provider)
	if err != nil {
		return nil, err
	}
	return s.connections.ClientScoped(providerConnectionScope(provider, slot), *s.egress, provider.Network, secret, upstreamHeaderTimeout)
}
