package gateway

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) targetResolver() providerinvoke.Resolver {
	return providerinvoke.Resolver{Credentials: s.deps.Runtime, Policy: s.egress, Connections: s.connections, DefaultClient: s.client, HeaderTimeout: UpstreamHeaderTimeout}
}

func (s *Server) targetBinding(x *call, provider *routing.Provider, slot routing.Slot, model string) providerinvoke.Binding {
	var caller providerinvoke.CallerCredential
	if x.origin != "probe" {
		caller = x.callerSecret
	}
	return s.targetResolver().Bind(x.request.release, providerinvoke.Target{Provider: *provider, Slot: slot, Model: model}, caller)
}

func (s *Server) slotSecret(ctx context.Context, x *call, slot routing.Slot, provider *routing.Provider, model string) ([]byte, error) {
	credential, err := s.targetBinding(x, provider, slot, model).Credential(ctx)
	x.grantGeneration = credential.Generation
	return credential.Secret, err
}

// applySlotCredential prepares an upstream request with a credential slot:
// the credential source serves the slot's secret, then the connector hosts,
// authenticates and signs the request. It records the values to redact. The
// caller classifies a failure of either step alike, so a secret read the
// context interrupted is not blamed on the credential.
func (s *Server) applySlotCredential(ctx context.Context, x *call, req *http.Request, cfg connectors.Config, slot routing.Slot, body []byte, provider *routing.Provider, model string) error {
	return s.bindExchange(x, provider, slot, model, cfg, nil).authenticate(ctx, req, body)
}

func (s *Server) providerNetworkSecret(ctx context.Context, pinned *release.Release, provider *routing.Provider) ([]byte, error) {
	return s.targetResolver().Bind(pinned, providerinvoke.Target{Provider: *provider}, nil).NetworkSecret(ctx)
}

// pinEligibility reports whether a pinned slot's credential version and its
// provider's network credential may still serve.
func (s *Server) pinEligibility(p *pin) routing.Eligibility {
	if p.slot.CredentialID != nil {
		if eligibility := s.deps.Runtime.Eligibility(*p.slot.CredentialID); eligibility != routing.Eligible {
			return eligibility
		}
	}
	if p.provider.Network != nil && p.provider.Network.CredentialID != "" {
		return s.deps.Runtime.Eligibility(p.provider.Network.CredentialID)
	}
	return routing.Eligible
}

func providerConnectionScope(provider *routing.Provider, slot routing.Slot) string {
	return (providerinvoke.Target{Provider: *provider, Slot: slot}).ConnectionScope()
}

func (s *Server) providerClient(ctx context.Context, pinned *release.Release, provider *routing.Provider, slot routing.Slot) (*http.Client, error) {
	return s.targetResolver().Bind(pinned, providerinvoke.Target{Provider: *provider, Slot: slot}, nil).Client(ctx)
}
