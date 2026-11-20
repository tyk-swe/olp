package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/resources"
)

const ContinuationHeader = "X-OLP-Continuation"
const submissionHeader = "X-OLP-Submission-ID"
const handleHeader = "X-OLP-Continuation-Handle"

type continuationExecution struct {
	version, submission string
	parent              *resources.Resource
	parentState         *StoredContinuation
	prior               *interaction.Continuation
	resource            *resources.Resource
	stored              *StoredContinuation
	replay              *interaction.Delivery
	emitted             int
}
type StoredContinuation struct {
	Version         string                    `json:"version"`
	Source          json.RawMessage           `json:"source"`
	Receipt         oif.Receipt               `json:"receipt"`
	Binding         string                    `json:"binding"`
	Interaction     *interaction.Continuation `json:"interaction,omitempty"`
	Delivery        interaction.Delivery      `json:"delivery"`
	SemanticHeaders http.Header               `json:"semantic_headers"`
	Query           url.Values                `json:"query"`
}

func (x *Call) Snapshot() *routing.View {
	if x.historicalSnapshot != nil {
		return x.historicalSnapshot
	}
	if x.request.release == nil {
		return nil
	}
	return x.request.release.Snapshot()
}
func ContinuationError(code, message string) *fault.Error {
	return fault.ServerError(http.StatusConflict, code, message)
}

func DecodeStoredContinuation(payload []byte) (*StoredContinuation, error) {
	if len(payload) > resources.MaxContinuationBytes {
		return nil, resources.ErrContract
	}
	var state StoredContinuation
	if err := json.Unmarshal(payload, &state); err != nil || state.Version != interaction.ContinuationV1 || len(state.Source) == 0 || state.Binding == "" {
		return nil, resources.ErrContract
	}
	return &state, nil
}
func (s *Server) prepareContinuation(ctx context.Context, x *Call) *fault.Error {
	values := x.semanticHeaders.Values(ContinuationHeader)
	submission := x.semanticHeaders.Values(submissionHeader)
	handles := x.semanticHeaders.Values(handleHeader)
	if len(values) == 0 && len(submission) == 0 && len(handles) == 0 {
		return nil
	}
	if len(values) != 1 || values[0] != interaction.ContinuationV1 || len(submission) != 1 || len(handles) > 1 || x.family != wire.Chat {
		return fault.InvalidRequest("state_carrier", "Provide the tested chat-anthropic-tools-v1 continuation contract and one submission identity.", nil)
	}
	route, ok := x.request.release.Snapshot().FindRoute(x.generation.parsed.Route)
	if !ok || !route.Fidelity.Strict() {
		return fault.InvalidRequest("state_carrier", "Negotiated continuation requires a strict route.", nil)
	}
	if !x.authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
		return fault.PermissionError("route_forbidden", "This key is not authorized for the continuation route.")
	}
	if !x.authority.Policy.AllowProviderState {
		return fault.InvalidRequest("policy_conflict", "Encrypted native continuation requires allow_provider_state on this API key.", nil)
	}
	if !s.Deps.Resources.Encrypted() {
		return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Encrypted continuation authority is not configured.")
	}
	c := &continuationExecution{version: values[0], submission: submission[0]}
	x.generation.continuation = c
	// Existing ready delivery is recoverable for the resource lifetime even after
	// the shorter window for claiming a fresh submission has closed.
	res, payload, err := s.Deps.Resources.FindSubmission(ctx, x.keyID, c.submission)
	if err == nil {
		state, e := DecodeStoredContinuation(payload)
		if e != nil {
			return ContinuationError("continuation_unavailable", "The stored continuation contract is unavailable.")
		}
		parentMatches := res.ParentID == nil && len(handles) == 0 || res.ParentID != nil && len(handles) == 1 && handles[0] == resources.LocalID(resources.KindContinuation, *res.ParentID)
		if !parentMatches || !reflect.DeepEqual(state.SemanticHeaders, continuationSemanticHeaders(x.semanticHeaders)) || !reflect.DeepEqual(state.Query, x.semanticQuery) || !interaction.SameSource(state.Source, x.generation.parsed.OIF().Document().Bytes()) || res.RouteSlug != x.generation.parsed.Route {
			return ContinuationError("continuation_mismatch", "This submission identity belongs to a different request.")
		}
		if e := s.AuthorizeContinuation(ctx, x, res, state); e != nil {
			return e
		}
		if res.State != resources.StateReady || state.Interaction == nil {
			return ContinuationError("continuation_outcome_unknown", "This submission already reserved provider work; no fresh inference will run. Recover only after a ready result is available.")
		}
		c.resource, c.stored, c.replay = res, state, &state.Delivery
		return nil
	}
	if !errors.Is(err, resources.ErrNotFound) {
		return ContinuationError("continuation_unavailable", "The stored continuation contract is unavailable.")
	}
	if err := resources.ValidateSubmission(c.submission, s.Now()); err != nil {
		return fault.InvalidRequest("invalid_submission_identity", "Provide a timestamp.UUID submission identity from the last fifteen minutes, allowing five minutes of clock skew.", nil)
	}
	if len(handles) == 0 {
		return nil
	}
	res, payload, err = s.Deps.Resources.ReadContract(ctx, resources.KindContinuation, x.keyID, handles[0])
	if err != nil || res.State != resources.StateReady {
		return ContinuationError("continuation_unavailable", "The continuation handle is expired, incomplete, or unavailable to this key.")
	}
	state, err := DecodeStoredContinuation(payload)
	if err != nil || state.Interaction == nil || res.RouteSlug != x.generation.parsed.Route {
		return ContinuationError("continuation_mismatch", "The continuation handle does not belong to this route and contract.")
	}
	// The pinned planning boundary resolves and authorizes this historical
	// provider once, after the owner-scoped encrypted handle has been read.
	// Delivery replay and public recovery still authorize directly below.
	c.parent, c.parentState, c.prior = res, state, state.Interaction
	x.pin = res
	x.serving = &state.Receipt.Serving
	x.servingSlot = res.SlotID
	x.servingBinding = state.Binding
	return nil
}
func (s *Server) AuthorizeContinuation(ctx context.Context, x *Call, res *resources.Resource, state *StoredContinuation) *fault.Error {
	p, _, e := s.ResolveResource(ctx, x, x.authority, res, operationGeneration, RetainedRetrieval)
	if e != nil {
		return e
	}
	return s.authorizeContinuationPin(ctx, x, res, state, p)
}
func (s *Server) authorizeContinuationPin(ctx context.Context, x *Call, res *resources.Resource, state *StoredContinuation, p *Pin) *fault.Error {
	if !p.Provider.Enabled || !p.Slot.Allows(p.Model, res.RouteSlug, x.keyID) {
		return PinUnavailable()
	}
	if p.Provider.Network != nil && p.Provider.Network.CredentialID != "" {
		if _, err := s.providerNetworkSecret(ctx, x.request.release, &p.Provider); err != nil {
			return PinUnavailable()
		}
	}
	if p.Model != state.Binding || p.Provider.RevisionID != state.Receipt.Serving.RevisionID || p.Provider.ProfileID != state.Receipt.ProfileID || p.Provider.ProfileRevision != state.Receipt.ProfileRevision {
		return PinUnavailable()
	}
	return nil
}

// claimToolWork runs inside the single Attempt immediately before provider Do.
// Authentication and transport preparation may fail earlier without a claim.
func (s *Server) claimToolWork(ctx context.Context, x *Call, plan *interaction.Plan, a routing.Attempt, slot routing.Slot) error {
	c := x.generation.continuation
	if c == nil || !plan.ToolContinuation() {
		return resources.ErrContract
	}
	if c.resource != nil {
		return resources.ErrTransition
	}
	expires := s.Now().Add(resources.ContinuationLifetime - time.Second)
	version := interaction.ContinuationV1
	metadata, _ := json.Marshal(map[string]string{"upstream_model": a.UpstreamModel})
	r := &resources.Resource{Kind: resources.KindContinuation, APIKeyID: x.keyID, RouteSlug: x.Route.Slug, ProviderID: a.ProviderID, ProviderRevisionID: a.ProviderRevisionID, RouteRevisionID: x.Route.RevisionID, TargetID: a.TargetID, SlotID: slot.ID, CredentialID: slot.CredentialID, Metadata: metadata, ExpiresAt: &expires, ContractVersion: &version, SubmissionID: &c.submission}
	if c.parent != nil {
		r.ParentID = &c.parent.UUID
	}
	state := &StoredContinuation{Version: version, Source: x.generation.parsed.OIF().Document().Bytes(), Receipt: plan.Receipt(), Binding: a.UpstreamModel, SemanticHeaders: continuationSemanticHeaders(x.semanticHeaders), Query: x.semanticQuery}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	res, created, err := s.Deps.Resources.ClaimForDispatch(ctx, r, payload)
	if err != nil {
		return err
	}
	if !created {
		return resources.ErrTransition
	}
	c.resource, c.stored = res, state
	return nil
}
func (s *Server) commitToolDelivery(ctx context.Context, x *Call, state *interaction.Continuation, delivery interaction.Delivery) error {
	c := x.generation.continuation
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
	return s.Deps.Resources.CompleteContinuation(ctx, c.resource, payload)
}

// replayContinuation answers a request from the delivery already committed for
// it. That is before admission: the replay makes no attempt and holds no
// reservation, so it has no allowance or metadata to state.
func (s *Server) replayContinuation(w http.ResponseWriter, x *Call) error {
	delivery := x.generation.continuation.replay
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-OLP-Delivery-Replay", "true")
	w.Header().Set("X-Should-Retry", "false")
	if delivery.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range delivery.Frames {
			if err := s.WriteContinuationPayload(w, x, interaction.ContinuationFrame(frame)); err != nil {
				return err
			}
		}
		return s.WriteContinuationPayload(w, x, []byte("data: [DONE]\n\n"))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return s.WriteContinuationPayload(w, x, delivery.Body)
}

// Cached correspondence has the same bounded delivery obligation as its
// original response. It never enters provider dispatch or owns new usage.
func (s *Server) WriteContinuationPayload(w http.ResponseWriter, x *Call, data []byte) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = rc.Flush()
	}
	if err != nil {
		return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	if len(data) != 0 {
		x.Delivered(s.Now())
	}
	return nil
}

func ContinuationDeliveryOutcome(err error) *Outcome {
	out := &Outcome{Committed: true}
	if err != nil {
		out.Err = (&AttemptFailure{class: ClassCancelled}).Fault()
		out.Cancelled = true
	}
	return out
}

func (s *Server) validateToolDelivery(delivery interaction.Delivery) error {
	for _, frame := range delivery.Frames {
		if int64(len(frame)+8) > s.Config.MaxEventBytes {
			return wire.ErrEventTooLarge
		}
	}
	if int64(len(delivery.Body)) > s.Config.MaxResponseBytes {
		return ErrResponseTooLarge
	}
	return nil
}

// recoverContinuation serves already committed client delivery only. It performs
// current authority checks and never enters admission, provider dispatch or a
// second inference retry engine, so like a replay it adds no response headers.
// Only semantic controls enter encrypted correspondence, never authentication,
// tracing, cookies, helper headers or transient SDK transport settings.
func continuationSemanticHeaders(input http.Header) http.Header {
	out := http.Header{}
	for name, values := range input {
		canonical := http.CanonicalHeaderKey(name)
		if connectors.IsServingIdentityHeader(canonical) {
			out[canonical] = append(out[canonical], values...)
			continue
		}
		switch canonical {
		case "Anthropic-Version", "Anthropic-Beta", "Openai-Beta", "Openai-Version", "Api-Version", "Idempotency-Key", "X-Idempotency-Key":
			out[canonical] = append(out[canonical], values...)
		default:
			if strings.HasPrefix(canonical, "X-Amzn-Bedrock-") {
				out[canonical] = append(out[canonical], values...)
			}
		}
	}
	return out
}
