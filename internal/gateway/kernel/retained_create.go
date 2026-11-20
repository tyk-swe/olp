package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

type retainedSubmission struct {
	slug   string
	key    string
	digest []byte
}

func (q retainedRequest) create() {
	s, e, end := q.server, q.endpoint, q.reply.end
	submission, err := q.submission()
	if err != nil {
		q.reply.fail(err)
		return
	}
	route, err := q.creationRoute(submission.slug)
	if err != nil {
		q.reply.fail(err)
		return
	}
	bound, err := q.boundResource()
	if err != nil {
		q.reply.fail(err)
		return
	}
	qualifiers, err := q.slotQualifiers(route)
	if err != nil {
		q.reply.fail(err)
		return
	}
	pin, err := s.selectBoundPinSurface(q.r.Context(), q.x, route, e.Operation, e.Surface, "unary", resourceDemand(q.x), bound, func(provider *routing.Provider, model string) bool {
		return provider.CredentialSource != "caller" && provider.Connector().Supports(e.Operation, e.Surface, "unary") && provider.Supports(model, e.Operation, e.Surface, "unary")
	}, qualifiers...)
	end.pin = pin
	if err != nil {
		q.reply.fail(err)
		return
	}
	rewritten, contentType, err := q.rewriteCreation(pin, route)
	if err != nil {
		q.reply.fail(err)
		return
	}
	ctx, cancel := s.StateDeadline(q.r.Context(), route)
	defer cancel()
	if err := s.admitRetainedRequest(ctx, q.x, q.authority, pin, route, q.metersWork()); err != nil {
		q.reply.fail(err)
		return
	}
	endpoint, err := q.upstreamURL(ctx, pin, q.ancestors)
	if err != nil {
		q.reply.fail(err)
		return
	}
	lifecycle, body, err := q.beginCreation(ctx, pin, route, submission, rewritten)
	if err != nil {
		q.reply.fail(err)
		return
	}
	defer s.settleFence(q.r.Context(), lifecycle.Close)
	end.status = s.retainedDispatch(q.reply.w, q.r.WithContext(ctx), q.x, pin, e, lifecycle.Resource(), q.parent, endpoint, body, contentType, lifecycle, q.reply.fail)
}

func (q retainedRequest) submission() (retainedSubmission, *fault.Error) {
	slug := strings.TrimSpace(q.r.Header.Get(StateRouteHeader))
	if q.parent != nil {
		slug = q.parent.RouteSlug
	}
	if slug == "" {
		return retainedSubmission{}, fault.InvalidRequest("missing_required_parameter", "X-OLP-Route is required.", nil)
	}
	key := q.r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		return retainedSubmission{}, fault.InvalidRequest("missing_required_parameter", "Idempotency-Key is required for resource creation.", nil)
	}
	scope := slug
	if q.parent != nil {
		scope += "\x00" + q.parent.ID
	}
	digest, err := q.server.retainedBodyDigest(q.endpoint, scope, q.input.body, q.input.contentType)
	if err != nil {
		return retainedSubmission{}, err
	}
	submission := retainedSubmission{slug: slug, key: key, digest: digest}
	return submission, q.server.unsubmitted(q.r.Context(), q.reply, q.endpoint.Operation, submission)
}

func (s *Server) unsubmitted(ctx context.Context, reply retainedReply, operation string, submission retainedSubmission) *fault.Error {
	prior, err := s.Deps.Resources.LookupLifecycle(ctx, reply.x.keyID, operation, submission.key, submission.digest)
	if errors.Is(err, resources.ErrSubmissionConflict) {
		return retainedSubmissionConflict()
	}
	if err == nil {
		reply.x.Route = &routing.Route{Slug: prior.RouteSlug, RevisionID: prior.RouteRevisionID}
		reply.w.Header().Set("X-OLP-Resource-ID", prior.ID)
		return retainedSubmissionExists()
	}
	if !errors.Is(err, resources.ErrNotFound) {
		return fault.ServerError(503, "provider_state_unavailable", "The submission could not be read.")
	}
	return nil
}

func retainedSubmissionConflict() *fault.Error {
	return fault.ServerError(409, "idempotency_conflict", "This submission identity is already bound to another request.")
}

func retainedSubmissionExists() *fault.Error {
	return fault.ServerError(409, "submission_exists", "This submission already exists; inspect X-OLP-Resource-ID instead of recreating it.")
}

func (q retainedRequest) creationRoute(slug string) (*routing.Route, *fault.Error) {
	candidate, ok := q.x.Snapshot().FindRoute(slug)
	if !ok {
		return nil, fault.ModelNotFound(slug)
	}
	route := &candidate
	q.x.Route = route
	if !q.authority.Allows("inference", route.Slug, route.ProjectID, q.server.Now()) {
		return nil, fault.PermissionError("route_forbidden", "The API key cannot use this route.")
	}
	if !slices.Contains(route.Operations, q.endpoint.Operation) {
		return nil, PolicyUnavailable("operation_unavailable", "The route does not allow this operation.")
	}
	if err := q.x.GuardrailSurfaceGate(route); err != nil {
		return nil, err
	}
	return route, nil
}

func (q retainedRequest) boundResource() (*resources.Resource, *fault.Error) {
	if q.parent != nil || q.input.source.Root().Kind() != oif.Object {
		return q.parent, nil
	}
	referenced, err := q.server.referencedResources(q.r.Context(), q.x, q.input.source, q.endpoint)
	if err != nil {
		return nil, retainedReferenceUnavailable()
	}
	if len(referenced) == 0 {
		return nil, nil
	}
	for _, candidate := range referenced[1:] {
		if !samePin(candidate, referenced[0]) {
			return nil, retainedReferenceUnavailable()
		}
	}
	return referenced[0], nil
}

func (q retainedRequest) slotQualifiers(route *routing.Route) ([]retainedSlotQualifier, *fault.Error) {
	if q.endpoint.Operation != "eval" {
		return nil, nil
	}
	qualify, err := retainedEvalSlots(route, q.x.keyID, q.input.source, q.parent)
	if err != nil {
		return nil, fault.InvalidRequest("unsupported_parameter", err.Error(), nil)
	}
	return []retainedSlotQualifier{qualify}, nil
}

func (q retainedRequest) rewriteCreation(pin *Pin, route *routing.Route) ([]byte, string, *fault.Error) {
	if q.input.source.Root().Kind() != oif.Object {
		return q.input.body, q.input.contentType, nil
	}
	rewritten, err := q.server.rewriteRetainedRequest(q.r.Context(), q.x, q.authority, pin, route, q.input.source, q.endpoint)
	if err != nil {
		return nil, "", retainedReferenceUnavailable()
	}
	body, contentType, err := encodeRetainedRequestBody(q.endpoint, rewritten, q.input.contentType)
	if err != nil {
		return nil, "", fault.InvalidRequest("invalid_request", "The resource upload could not be encoded.", nil)
	}
	return body, contentType, nil
}

func (q retainedRequest) beginCreation(ctx context.Context, p *Pin, route *routing.Route, submission retainedSubmission, rewritten []byte) (*resources.Lifecycle, []byte, *fault.Error) {
	s, e := q.server, q.endpoint
	fields, err := retainedInputMetadata(p, route, q.x.keyID, e, q.input.source, q.parent)
	if err != nil {
		return nil, nil, fault.InvalidRequest("unsupported_parameter", err.Error(), nil)
	}
	meta, _ := json.Marshal(fields)
	version := retainedapi.Revision
	candidate := &resources.Resource{Kind: e.Kind, APIKeyID: q.x.keyID, RouteSlug: route.Slug, ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID, TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID, Metadata: meta, ContractVersion: &version}
	if q.parent != nil {
		candidate.ParentID = &q.parent.UUID
		candidate.RouteRevisionID = q.parent.RouteRevisionID
	}
	if err := s.prepareRetainedAccounting(q.x, p, route, candidate, e); err != nil {
		return nil, nil, err
	}
	lifecycle, err := s.Deps.Resources.BeginCreate(ctx, candidate, resources.Submission{Operation: e.Operation, Key: submission.key, Digest: submission.digest})
	if errors.Is(err, resources.ErrSubmissionConflict) {
		return nil, nil, retainedSubmissionConflict()
	}
	if err != nil {
		return nil, nil, fault.ServerError(503, "provider_state_unavailable", "The submission could not be persisted.")
	}
	q.reply.w.Header().Set("X-OLP-Resource-ID", lifecycle.Resource().ID)
	if lifecycle.Existing() {
		return nil, nil, retainedSubmissionExists()
	}
	body, err := retainedCorrelationBody(e, lifecycle.Resource(), rewritten)
	if err != nil {
		return nil, nil, fault.InvalidRequest("invalid_request", "The resource correlation metadata could not be bound.", nil)
	}
	if err := lifecycle.Dispatch(ctx); err != nil {
		return nil, nil, fault.ServerError(503, "provider_state_unavailable", "The submission could not be fenced.")
	}
	return lifecycle, body, nil
}
