package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
)

type providerResponse struct {
	body       []byte
	upstreamID string
	state      string
	metadata   map[string]json.RawMessage
	expires    *time.Time
}

func readProviderResponse(body []byte, upstreamModel string) (providerResponse, bool) {
	upstreamID, ok := upstreamString(body, "id")
	if !ok || len(upstreamID) > 512 {
		return providerResponse{}, false
	}
	state := "created"
	if status, ok := upstreamString(body, "status"); ok {
		state = status
	}
	metadata := map[string]json.RawMessage{}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) == nil {
		for _, name := range responseMetadataFields() {
			if raw, present := obj[name]; present {
				metadata[name] = raw
			}
		}
	}
	if upstreamModel != "" {
		encoded, _ := json.Marshal(upstreamModel)
		metadata["upstream_model"] = encoded
	}
	var expires *time.Time
	if raw, present := metadata["expires_at"]; present {
		var seconds int64
		if json.Unmarshal(raw, &seconds) == nil && seconds > 0 {
			at := time.Unix(seconds, 0).UTC()
			expires = &at
		}
	}
	return providerResponse{body: body, upstreamID: upstreamID, state: state, metadata: metadata, expires: expires}, true
}

func (s *Server) mapStoredResponse(ctx context.Context, x *Call, authority keyauthority.Authority, body []byte) ([]byte, *fault.Error) {
	if s.Deps.Resources == nil || len(x.facts) == 0 || !responseStoreRequested(x.generation.parsed) {
		return body, nil
	}
	fact := x.facts[len(x.facts)-1]
	created, ok := readProviderResponse(body, fact.UpstreamModel)
	if !ok {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed response object.")
	}
	if x.Strict() {
		return s.storeStrictResponse(ctx, x, &fact, created)
	}
	return s.storeResponse(ctx, x, authority, &fact, created)
}

func (s *Server) storeStrictResponse(ctx context.Context, x *Call, fact *events.AttemptFact, created providerResponse) ([]byte, *fault.Error) {
	deferred := s.pendingResponseUsage(x, fact, created.metadata)
	encoded, _ := json.Marshal(created.metadata)
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	res, err := s.putStrictResponse(commitCtx, x, fact, created.upstreamID, created.state, encoded, created.expires)
	if err != nil {
		return nil, fault.ServerError(http.StatusInternalServerError, "continuation_unavailable", "The strict response contract could not be committed.")
	}
	if deferred {
		if e := s.deferResponseUsage(commitCtx, x, res, created.body, "continuation_unavailable"); e != nil {
			return nil, e
		}
	}
	doc, parseErr := oif.ParseJSON(created.body, oif.Limits{MaxBytes: int(s.Config.MaxResponseBytes)})
	if parseErr != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a malformed response object.")
	}
	projection := ResponseProjection{UpstreamID: created.upstreamID, LocalID: res.ID, Route: x.Route.Slug}
	if previous := x.previousResponse(); previous != nil {
		projection.PreviousUpstream, projection.PreviousLocal = previous.UpstreamID, previous.ID
	}
	mapped, err := projection.project(doc, "")
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unbound response identity.")
	}
	return mapped.Bytes(), nil
}

func (s *Server) storeResponse(ctx context.Context, x *Call, authority keyauthority.Authority, fact *events.AttemptFact, created providerResponse) ([]byte, *fault.Error) {
	var credential *string
	if fact.CredentialID != "" {
		credential = &fact.CredentialID
	}
	deferred := s.pendingResponseUsage(x, fact, created.metadata)
	encoded, _ := json.Marshal(created.metadata)
	identity := responseNativeIdentity(x, fact, created.upstreamID)
	res, err := s.Deps.Resources.GetByUpstream(ctx, identity)
	if errors.Is(err, resources.ErrNotFound) {
		res, err = s.Deps.Resources.Put(ctx, &resources.Resource{
			Kind:               resources.KindResponse,
			APIKeyID:           authority.ID,
			RouteSlug:          x.Route.Slug,
			ProviderID:         fact.ProviderID,
			ProviderRevisionID: fact.ProviderRevisionID,
			RouteRevisionID:    x.Route.RevisionID,
			TargetID:           fact.TargetID,
			SlotID:             fact.SlotID,
			CredentialID:       credential,
			UpstreamID:         created.upstreamID,
			State:              created.state,
			Metadata:           encoded,
			ExpiresAt:          created.expires,
		})
	}
	if err != nil {
		return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The stored response mapping could not be recorded.")
	}
	if deferred {
		if e := s.deferResponseUsage(ctx, x, res, created.body, "internal_error"); e != nil {
			return nil, e
		}
	}
	out, err := RewriteID(created.body, "id", res.ID)
	if err == nil {
		out, err = s.MapPreviousResponse(ctx, out, identity, x.pin)
	}
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed response object.")
	}
	return out, nil
}

func (s *Server) deferResponseUsage(ctx context.Context, x *Call, res *resources.Resource, body []byte, unattached string) *fault.Error {
	if err := attachGenerationHTTP(x, res); err != nil {
		return fault.ServerError(http.StatusInternalServerError, unattached, "The original response accounting owner could not be attached.")
	}
	last := &x.facts[len(x.facts)-1]
	last.GenerationUsageDeferred = true
	last.RecordEvidence(false)
	return s.reconcileResponse(ctx, res.ID, body)
}
