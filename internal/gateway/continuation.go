package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

const continuationHeader = "X-OLP-Continuation"
const submissionHeader = "X-OLP-Submission-ID"
const handleHeader = "X-OLP-Continuation-Handle"

type continuationExecution struct {
	version, submission string
	parent              *resources.Resource
	prior               *interaction.Continuation
	resource            *resources.Resource
	stored              *storedContinuation
	replay              *interaction.Delivery
	emitted             int
}
type storedContinuation struct {
	Version     string                    `json:"version"`
	Source      json.RawMessage           `json:"source"`
	Receipt     interaction.Receipt       `json:"receipt"`
	Binding     string                    `json:"binding"`
	Interaction *interaction.Continuation `json:"interaction,omitempty"`
	Delivery    interaction.Delivery      `json:"delivery"`
}

func (x *execution) snapshot() *runtime.Snapshot {
	if x.historicalSnapshot != nil {
		return x.historicalSnapshot
	}
	return x.request.release.Snapshot
}
func continuationError(code, message string) *Error {
	return serverError(http.StatusConflict, code, message)
}

func decodeStoredContinuation(payload []byte) (*storedContinuation, error) {
	if len(payload) > resources.MaxContinuationBytes {
		return nil, resources.ErrContract
	}
	var state storedContinuation
	if err := json.Unmarshal(payload, &state); err != nil || state.Version != interaction.ContinuationV1 || len(state.Source) == 0 || state.Binding == "" {
		return nil, resources.ErrContract
	}
	return &state, nil
}
func (s *Server) prepareContinuation(ctx context.Context, x *execution) *Error {
	values := x.semanticHeaders.Values(continuationHeader)
	submission := x.semanticHeaders.Values(submissionHeader)
	handles := x.semanticHeaders.Values(handleHeader)
	if len(values) == 0 && len(submission) == 0 && len(handles) == 0 {
		return nil
	}
	if len(values) != 1 || values[0] != interaction.ContinuationV1 || len(submission) != 1 || len(handles) > 1 || x.family != openai.FamilyChat {
		return invalidRequest("state_carrier", "Provide the tested chat-anthropic-tools-v1 continuation contract and one submission identity.", nil)
	}
	route, ok := x.request.release.Snapshot.Routes[x.parsed.Route]
	if !ok || runtime.FidelityMode(route.Fidelity) != runtime.FidelityStrict {
		return invalidRequest("state_carrier", "Negotiated continuation requires a strict route.", nil)
	}
	if !x.authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		return permissionError("route_forbidden", "This key is not authorized for the continuation route.")
	}
	if !s.Resources.Encrypted() {
		return serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Encrypted continuation authority is not configured.")
	}
	c := &continuationExecution{version: values[0], submission: submission[0]}
	x.continuation = c
	// Existing ready delivery is recoverable for the resource lifetime even after
	// the shorter window for claiming a fresh submission has closed.
	res, payload, err := s.Resources.FindSubmission(ctx, x.keyID, c.submission)
	if err == nil {
		state, e := decodeStoredContinuation(payload)
		if e != nil {
			return continuationError("continuation_unavailable", "The stored continuation contract is unavailable.")
		}
		if !interaction.SameSource(state.Source, x.parsed.OIF().Document().Bytes()) || res.RouteSlug != x.parsed.Route {
			return continuationError("continuation_mismatch", "This submission identity belongs to a different request.")
		}
		if e := s.authorizeContinuation(ctx, x, res, state); e != nil {
			return e
		}
		if res.State != resources.StateReady || state.Interaction == nil {
			return continuationError("continuation_outcome_unknown", "This submission already reserved provider work; no fresh inference will run. Recover only after a ready result is available.")
		}
		c.resource, c.stored, c.replay = res, state, &state.Delivery
		return nil
	}
	if !errors.Is(err, resources.ErrNotFound) {
		return continuationError("continuation_unavailable", "The stored continuation contract is unavailable.")
	}
	if err := resources.ValidateSubmission(c.submission, s.now()); err != nil {
		return invalidRequest("invalid_submission_identity", "Provide a timestamp.UUID submission identity from the last fifteen minutes, allowing five minutes of clock skew.", nil)
	}
	if len(handles) == 0 {
		return nil
	}
	res, payload, err = s.Resources.ReadContract(ctx, resources.KindContinuation, x.keyID, handles[0])
	if err != nil || res.State != resources.StateReady {
		return continuationError("continuation_unavailable", "The continuation handle is expired, incomplete, or unavailable to this key.")
	}
	state, err := decodeStoredContinuation(payload)
	if err != nil || state.Interaction == nil || res.RouteSlug != x.parsed.Route {
		return continuationError("continuation_mismatch", "The continuation handle does not belong to this route and contract.")
	}
	if e := s.authorizeContinuation(ctx, x, res, state); e != nil {
		return e
	}
	c.parent, c.prior = res, state.Interaction
	x.pin = res
	x.serving = &state.Receipt.Serving
	x.servingSlot = res.SlotID
	x.servingBinding = state.Binding
	return nil
}
func (s *Server) authorizeContinuation(ctx context.Context, x *execution, res *resources.Resource, state *storedContinuation) *Error {
	p, _, e := s.resolveResource(ctx, x, x.authority, res, operationGeneration)
	if e != nil {
		return e
	}
	if !p.provider.Enabled || !p.slot.Allows(p.model, res.RouteSlug, x.keyID) {
		return pinUnavailable()
	}
	if p.provider.Network != nil && p.provider.Network.CredentialID != "" {
		if _, err := s.providerNetworkSecret(ctx, x.request.release, &p.provider); err != nil {
			return pinUnavailable()
		}
	}
	if p.model != state.Binding || p.provider.RevisionID != state.Receipt.Serving.RevisionID || p.provider.ProfileID != state.Receipt.ProfileID || p.provider.ProfileRevision != state.Receipt.ProfileRevision {
		return pinUnavailable()
	}
	return nil
}

// claimToolWork runs inside the single Attempt immediately before provider Do.
// Authentication and transport preparation may fail earlier without a claim.
func (s *Server) claimToolWork(ctx context.Context, x *execution, plan *interaction.Plan, a runtime.Attempt, slot runtime.Slot) error {
	c := x.continuation
	if c == nil || !plan.ToolContinuation() {
		return resources.ErrContract
	}
	if c.resource != nil {
		return resources.ErrTransition
	}
	expires := s.now().Add(resources.ContinuationLifetime - time.Second)
	version := interaction.ContinuationV1
	metadata, _ := json.Marshal(map[string]string{"upstream_model": a.UpstreamModel})
	r := &resources.Resource{Kind: resources.KindContinuation, APIKeyID: x.keyID, RouteSlug: x.route.Slug, ProviderID: a.ProviderID, ProviderRevisionID: a.ProviderRevisionID, RouteRevisionID: x.route.RevisionID, SlotID: slot.ID, CredentialID: slot.CredentialID, Metadata: metadata, ExpiresAt: &expires, ContractVersion: &version, SubmissionID: &c.submission}
	if c.parent != nil {
		r.ParentID = &c.parent.UUID
	}
	state := &storedContinuation{Version: version, Source: x.parsed.OIF().Document().Bytes(), Receipt: plan.Receipt(), Binding: a.UpstreamModel}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	res, created, err := s.Resources.ClaimContinuation(ctx, r, payload)
	if err != nil {
		return err
	}
	if !created {
		return resources.ErrTransition
	}
	c.resource, c.stored = res, state
	return s.Resources.StartDispatch(ctx, res)
}
func (s *Server) commitToolDelivery(ctx context.Context, x *execution, state *interaction.Continuation, delivery interaction.Delivery) error {
	c := x.continuation
	if c == nil || c.resource == nil || c.stored == nil {
		return resources.ErrContract
	}
	c.stored.Interaction, c.stored.Delivery = state, delivery
	payload, err := json.Marshal(c.stored)
	if err != nil {
		return err
	}
	if len(payload) > resources.MaxContinuationBytes {
		return resources.ErrPayloadTooLarge
	}
	return s.Resources.CompleteContinuation(ctx, c.resource, payload)
}
func (s *Server) replayContinuation(w http.ResponseWriter, x *execution) (int, error) {
	delivery := x.continuation.replay
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-OLP-Delivery-Replay", "true")
	w.Header().Set("X-Should-Retry", "false")
	if delivery.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range delivery.Frames {
			if _, err := w.Write(interaction.ContinuationFrame(frame)); err != nil {
				return http.StatusOK, err
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return http.StatusOK, err
			}
		}
		_, err := w.Write([]byte("data: [DONE]\n\n"))
		return http.StatusOK, err
	}
	w.Header().Set("Content-Type", "application/json")
	_, err := w.Write(delivery.Body)
	return http.StatusOK, err
}

func (s *Server) validateToolDelivery(delivery interaction.Delivery) error {
	for _, frame := range delivery.Frames {
		if int64(len(frame)+8) > s.cfg.MaxEventBytes {
			return openai.ErrEventTooLarge
		}
	}
	if int64(len(delivery.Body)) > s.cfg.MaxResponseBytes {
		return errResponseTooLarge
	}
	return nil
}

// recoverContinuation serves already committed client delivery only. It performs
// current authority checks and never enters admission, provider dispatch or a
// second inference retry engine.
func (s *Server) recoverContinuation(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, openai.FamilyChat)
	if done {
		return
	}
	defer s.release(r.Context())
	x.authority = authority
	if values := r.Header.Values(continuationHeader); len(values) != 1 || values[0] != interaction.ContinuationV1 {
		s.stateFail(x, w, invalidRequest("state_carrier", "Recovery requires the tested chat-anthropic-tools-v1 client contract.", nil), x.family)
		return
	}
	var res *resources.Resource
	var payload []byte
	var err error
	if submission := r.PathValue("submission"); submission != "" {
		res, payload, err = s.Resources.FindSubmission(r.Context(), authority.ID, submission)
	} else {
		res, payload, err = s.Resources.ReadContract(r.Context(), resources.KindContinuation, authority.ID, r.PathValue("id"))
	}
	if err != nil {
		s.stateFail(x, w, notFoundError("continuation_unavailable", "No unexpired continuation with this identity is available to this key."), x.family)
		return
	}
	route, exists := x.request.release.Snapshot.Routes[res.RouteSlug]
	if !exists || !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		s.stateFail(x, w, notFoundError("continuation_unavailable", "The continuation is unavailable to this key."), x.family)
		return
	}
	x.route = &route
	state, err := decodeStoredContinuation(payload)
	if err != nil {
		s.stateFail(x, w, continuationError("continuation_unavailable", "The stored contract is unavailable."), x.family)
		return
	}
	if e := s.authorizeContinuation(r.Context(), x, res, state); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if res.State != resources.StateReady || state.Interaction == nil {
		s.stateFail(x, w, continuationError("continuation_outcome_unknown", "This submission reserved provider work but has no committed ready delivery; fresh inference will not run."), x.family)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Should-Retry", "false")
	_ = json.NewEncoder(w).Encode(map[string]any{"version": state.Version, "handle": res.ID, "state": "ready", "assistant": state.Interaction.Assistant, "delivery": state.Delivery})
	s.finish(x, nil, http.StatusOK)
}
