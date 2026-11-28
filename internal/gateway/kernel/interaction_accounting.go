package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/resources"
)

func (s *Server) StoreInteraction(ctx context.Context, exchange *PinnedExchange, p *Pin, owner, upstreamID, state string, parent *resources.Resource, deferUsage bool) (*resources.Resource, error) {
	x := exchange.request
	version := resources.InteractionContract
	expiry := s.Now().Add(resources.ContinuationLifetime - time.Minute)
	model, _ := json.Marshal(p.Model)
	metadataFields := map[string]json.RawMessage{"upstream_model": model}
	if deferUsage && !s.pendingGenerationUsage(x, exchange.fact(), metadataFields) {
		return nil, errors.New("Interaction generation accounting template unavailable")
	}
	metadata, err := json.Marshal(metadataFields)
	if err != nil {
		return nil, err
	}
	r := &resources.Resource{Kind: resources.KindInteraction, APIKeyID: owner, RouteSlug: x.Route.Slug,
		ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: x.Route.RevisionID,
		TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID, State: state, Metadata: metadata,
		ContractVersion: &version, ExpiresAt: &expiry}
	if parent != nil {
		r.ParentID = &parent.UUID
	}
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	stored, err := s.Deps.Resources.PutInteractionContract(commitCtx, r, upstreamID)
	if err == nil && deferUsage {
		if err = attachGenerationHTTP(x, stored); err != nil {
			return nil, err
		}
		exchange.fact().GenerationUsageDeferred = true
		exchange.recordEvidence()
	}
	return stored, err
}
