package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/resources"
)

const responseContractVersion = "native-responses-v1"

type storedResponseContract struct {
	Version string              `json:"version"`
	Receipt interaction.Receipt `json:"receipt"`
	Binding string              `json:"binding"`
	Source  json.RawMessage     `json:"source"`
}

func responseResourceKind(x *execution) string {
	if x.strict() {
		return resources.KindStrictResponse
	}
	return resources.KindResponse
}
func (s *Server) readResponseResource(ctx context.Context, owner, id string) (*resources.Resource, *storedResponseContract, error) {
	if !strings.HasPrefix(id, resources.KindStrictResponse+"_") {
		r, err := s.Resources.Get(ctx, resources.KindResponse, owner, id)
		return r, nil, err
	}
	r, payload, err := s.Resources.ReadContract(ctx, resources.KindStrictResponse, owner, id)
	if err != nil {
		return nil, nil, err
	}
	var contract storedResponseContract
	if json.Unmarshal(payload, &contract) != nil || contract.Version != responseContractVersion || contract.Binding == "" || contract.Receipt.Obligations.Continuation != "native_response_resource" {
		return nil, nil, resources.ErrContract
	}
	return r, &contract, nil
}
func (s *Server) authorizeResponseContract(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, contract *storedResponseContract) *Error {
	current, ok := x.request.release.Snapshot.Routes[res.RouteSlug]
	if !ok || !authority.Policy.AllowProviderState || !authority.Allows("inference", current.Slug, current.ProjectID, s.now()) {
		return notFoundError("not_found", "The stored response is unavailable to this key.")
	}
	p, _, e := s.resolveResource(ctx, x, authority, res, operationGeneration)
	if e != nil {
		return e
	}
	if !p.provider.Enabled || !p.slot.Allows(p.model, res.RouteSlug, authority.ID) || p.model != contract.Binding || p.provider.RevisionID != contract.Receipt.Serving.RevisionID || p.provider.ProfileID != contract.Receipt.ProfileID || p.provider.ProfileRevision != contract.Receipt.ProfileRevision {
		return pinUnavailable()
	}
	if p.provider.Network != nil && p.provider.Network.CredentialID != "" {
		if _, err := s.providerNetworkSecret(ctx, x.request.release, &p.provider); err != nil {
			return pinUnavailable()
		}
	}
	return nil
}
func (s *Server) putStrictResponse(ctx context.Context, x *execution, fact *AttemptFact, upstream, state string, metadata []byte, providerExpiry *time.Time) (*resources.Resource, error) {
	provider, ok := x.snapshot().Providers[fact.ProviderID]
	if !ok {
		return nil, resources.ErrContract
	}
	prepared, err := x.preparedProvider(&provider, fact.UpstreamModel)
	if err != nil || prepared.plan == nil || prepared.plan.Obligations().Continuation != "native_response_resource" {
		return nil, resources.ErrContract
	}
	existing, err := s.Resources.GetByUpstream(ctx, resources.KindStrictResponse, x.keyID, fact.ProviderID, upstream)
	if err == nil {
		if existing.ProviderRevisionID != fact.ProviderRevisionID || existing.RouteRevisionID != x.route.RevisionID || existing.SlotID != fact.SlotID {
			return nil, resources.ErrContract
		}
		return existing, nil
	}
	if !errors.Is(err, resources.ErrNotFound) {
		return nil, err
	}
	expires := s.now().Add(resources.ContinuationLifetime - time.Second)
	if providerExpiry != nil && providerExpiry.Before(expires) {
		expires = *providerExpiry
	}
	version := responseContractVersion
	var credential *string
	if fact.CredentialID != "" {
		credential = &fact.CredentialID
	}
	payload, err := json.Marshal(storedResponseContract{Version: version, Receipt: prepared.plan.Receipt(), Binding: fact.UpstreamModel, Source: x.parsed.OIF().Document().Bytes()})
	if err != nil {
		return nil, err
	}
	// Keep only the non-secret serving index and the content-free deferred
	// accounting template. Native source remains encrypted in the contract.
	var billing map[string]json.RawMessage
	if json.Unmarshal(metadata, &billing) != nil {
		return nil, resources.ErrContract
	}
	metadata, _ = json.Marshal(billing)
	r := &resources.Resource{Kind: resources.KindStrictResponse, APIKeyID: x.keyID, RouteSlug: x.route.Slug, ProviderID: fact.ProviderID, ProviderRevisionID: fact.ProviderRevisionID, RouteRevisionID: x.route.RevisionID, SlotID: fact.SlotID, CredentialID: credential, UpstreamID: upstream, State: state, Metadata: metadata, ExpiresAt: &expires, ContractVersion: &version}
	if x.pin != nil {
		r.ParentID = &x.pin.UUID
	}
	return s.Resources.PutContract(ctx, r, payload)
}
