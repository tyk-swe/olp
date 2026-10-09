package gateway

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
)

// slotSecret asks for a credential slot's usable secret and remembers its
// grant generation for this attempt. A slot without a credential version
// authenticates without one.
func (s *Server) slotSecret(ctx context.Context, x *execution, slot runtime.Slot, provider *runtime.Provider, model string) ([]byte, error) {
	x.grantGeneration = 0
	if provider.CredentialSource == "caller" && x.origin != "probe" {
		return x.callerSecret(provider.ID, model)
	}
	if slot.CredentialID == nil {
		return nil, nil
	}
	secret, generation, err := s.Runtime.Secret(ctx, x.request.release, *slot.CredentialID)
	x.grantGeneration = generation
	return secret, err
}

// applySlotCredential prepares an upstream request with a credential slot:
// the credential source serves the slot's secret, then the connector hosts,
// authenticates and signs the request. It records the values to redact. The
// caller classifies a failure of either step alike, so a secret read the
// context interrupted is not blamed on the credential.
func (s *Server) applySlotCredential(ctx context.Context, x *execution, req *http.Request, cfg connectors.Config, slot runtime.Slot, body []byte, provider *runtime.Provider, model string) error {
	secret, err := s.slotSecret(ctx, x, slot, provider, model)
	if err != nil {
		return err
	}
	return s.applyCredentials(ctx, x, req, cfg, secret, body)
}

// providerNetworkSecret asks the credential source for the provider's network
// credential. A resource can retain a compatible historical profile and
// network credential after the current release changed; current revocation
// still wins.
func (s *Server) providerNetworkSecret(ctx context.Context, release *runtime.Release, provider *runtime.Provider) ([]byte, error) {
	if provider.Network == nil || provider.Network.CredentialID == "" {
		return nil, nil
	}
	return s.Runtime.NetworkSecret(ctx, release, provider.ID, provider.Network.CredentialID)
}

// pinEligibility reports whether a pinned slot's credential version and its
// provider's network credential may still serve.
func (s *Server) pinEligibility(p *pin) runtime.Eligibility {
	if p.slot.CredentialID != nil {
		if eligibility := s.Runtime.Eligibility(*p.slot.CredentialID); eligibility != runtime.Eligible {
			return eligibility
		}
	}
	if p.provider.Network != nil && p.provider.Network.CredentialID != "" {
		return s.Runtime.Eligibility(p.provider.Network.CredentialID)
	}
	return runtime.Eligible
}

func providerConnectionScope(provider *runtime.Provider, slot runtime.Slot) string {
	credential := "none"
	if slot.CredentialID != nil {
		credential = *slot.CredentialID
	}
	return provider.ID + "/" + provider.RevisionID + "/" + slot.ID + "/" + credential
}

func (s *Server) providerClient(ctx context.Context, release *runtime.Release, provider *runtime.Provider, slot runtime.Slot) (*http.Client, error) {
	if provider.CredentialSource == "caller" {
		secret, err := s.providerNetworkSecret(ctx, release, provider)
		if err != nil {
			return nil, err
		}
		return s.egress.EphemeralClient(provider.Network, secret, upstreamHeaderTimeout)
	}
	if provider.Network == nil && provider.ProfileID == "" {
		return s.client, nil
	}
	secret, err := s.providerNetworkSecret(ctx, release, provider)
	if err != nil {
		return nil, err
	}
	return s.connections.ClientScoped(providerConnectionScope(provider, slot), *s.egress, provider.Network, secret, upstreamHeaderTimeout)
}
