package kernel

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

type retainedWork struct {
	operation string
	path      string
	storeKind string
}

func (k retainedWork) generates() bool { return k.storeKind == "" }

func (k retainedWork) codec() retainedapi.Endpoint {
	if k.generates() {
		return retainedapi.Endpoint{Family: retainedapi.Family{Operation: "compaction", Surface: "openai"}}
	}
	return retainedapi.Endpoint{Family: retainedapi.Family{Operation: "retrieval", Surface: "openai"}}
}

type retainedWorkRequest struct {
	server    *Server
	r         *http.Request
	x         *Call
	authority keyauthority.Authority
	reply     retainedReply
	work      retainedWork
	body      []byte
	source    oif.Document
}

type retainedWorkTarget struct {
	pin      *Pin
	route    *routing.Route
	resource *resources.Resource
	path     string
}

func (s *Server) retainedSearch(w http.ResponseWriter, r *http.Request) {
	if id, err := uuid.Parse(r.PathValue("id")); err == nil {
		r.SetPathValue("binding_id", id.String())
		s.queryRetrieval(w, r)
		return
	}
	s.retainedProxy(w, r, retainedWork{operation: "retrieval", path: "vector_stores/{id}/search", storeKind: "vector_store"})
}

func (s *Server) retainedCompact(w http.ResponseWriter, r *http.Request) {
	s.retainedProxy(w, r, retainedWork{operation: "generation", path: "responses/compact"})
}

func (s *Server) retainedProxy(w http.ResponseWriter, r *http.Request, work retainedWork) {
	x, authority, done := s.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	x.operationOverride = work.operation
	x.mode = "unary"
	x.actor = "api_key_resource"
	x.retained = retainedOperation{use: RetainedNewWork, billing: retainedMeteredWork}
	end := &retainedEnd{status: http.StatusInternalServerError}
	defer func() {
		s.Finish(x, nil, end.status)
		s.retainedSettle(r.Context(), x, end.pin)
	}()
	reply := retainedReply{w: w, x: x, surface: "openai", end: end}
	if !authority.Policy.AllowProviderState {
		reply.fail(providerStateForbidden())
		return
	}
	q, err := s.readRetainedWork(r, x, authority, work, reply)
	if err != nil {
		reply.fail(err)
		return
	}
	target, err := q.target()
	if err != nil {
		reply.fail(err)
		return
	}
	x.Route = target.route
	if err := x.GuardrailSurfaceGate(target.route); err != nil {
		reply.fail(err)
		return
	}
	body, err := q.rewrite(target)
	if err != nil {
		reply.fail(err)
		return
	}
	x.estimate = int64(len(body))
	ctx, cancel := s.StateDeadline(r.Context(), target.route)
	defer cancel()
	if err := s.admitRetainedRequest(ctx, x, authority, target.pin, target.route, false); err != nil {
		reply.fail(err)
		return
	}
	if err := q.perform(ctx, target, body); err != nil {
		reply.fail(err)
	}
}

func (s *Server) readRetainedWork(r *http.Request, x *Call, authority keyauthority.Authority, work retainedWork, reply retainedReply) (retainedWorkRequest, *fault.Error) {
	body, err := s.Gate.ReadBody(r)
	if err != nil {
		return retainedWorkRequest{}, err
	}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.Config.MaxBodyBytes)})
	if parseErr != nil || source.Root().Kind() != oif.Object {
		return retainedWorkRequest{}, fault.InvalidRequest("invalid_request", "A valid JSON object is required.", nil)
	}
	if len(r.URL.Query()) != 0 {
		return retainedWorkRequest{}, fault.InvalidRequest("invalid_parameter", "This operation does not support query parameters.", nil)
	}
	return retainedWorkRequest{server: s, r: r, x: x, authority: authority, reply: reply, work: work, body: body, source: source}, nil
}

func (q retainedWorkRequest) target() (retainedWorkTarget, *fault.Error) {
	if q.work.generates() {
		return q.routeTarget()
	}
	return q.storeTarget()
}

// Searching a retained store starts new billable work, so it must satisfy
// current new-work restrictions rather than the historical retrieval contract
// that created the binding.
func (q retainedWorkRequest) storeTarget() (retainedWorkTarget, *fault.Error) {
	resource, getErr := q.server.Deps.Resources.Get(q.r.Context(), q.work.storeKind, q.x.keyID, q.r.PathValue("id"))
	if getErr != nil {
		return retainedWorkTarget{}, retainedMissing()
	}
	pin, route, err := q.server.ResolveResource(q.r.Context(), q.x, q.authority, resource, q.work.operation, RetainedNewWork)
	q.reply.end.pin = pin
	if err != nil {
		return retainedWorkTarget{}, err
	}
	path := strings.Replace(q.work.path, "{id}", url.PathEscape(resource.UpstreamID), 1)
	return retainedWorkTarget{pin: pin, route: route, resource: resource, path: path}, nil
}

// The work is priced before selection reserves the provider hold, because
// selecting first would admit the slot against the floored minimum estimate.
// Retained references own the pin that must serve the call, so they are
// resolved first and selection is bound to their target and credential slot.
func (q retainedWorkRequest) routeTarget() (retainedWorkTarget, *fault.Error) {
	route, err := q.route()
	if err != nil {
		return retainedWorkTarget{}, err
	}
	q.x.estimate = int64(len(q.body))
	bound, err := q.boundReference()
	if err != nil {
		return retainedWorkTarget{}, err
	}
	operation := q.work.operation
	pin, err := q.server.selectBoundPinSurface(q.r.Context(), q.x, route, operation, "openai", "unary", bound, func(provider *routing.Provider, model string) bool {
		return provider.CredentialSource != "caller" && (provider.Kind == "openai" || provider.Kind == "azure_openai") && provider.Connector().Supports(operation, "openai", "unary")
	})
	q.reply.end.pin = pin
	if err != nil {
		return retainedWorkTarget{}, err
	}
	return retainedWorkTarget{pin: pin, route: route, path: q.work.path}, nil
}

func (q retainedWorkRequest) route() (*routing.Route, *fault.Error) {
	slug := strings.TrimSpace(q.r.Header.Get(StateRouteHeader))
	if slug == "" {
		return nil, fault.InvalidRequest("missing_required_parameter", "X-OLP-Route is required.", nil)
	}
	candidate, ok := q.x.Snapshot().FindRoute(slug)
	if !ok {
		return nil, fault.ModelNotFound(slug)
	}
	route := &candidate
	q.x.Route = route
	if !q.authority.Allows("inference", route.Slug, route.ProjectID, q.server.Now()) {
		return nil, fault.PermissionError("route_forbidden", "The API key cannot use this route.")
	}
	if !slices.Contains(route.Operations, q.work.operation) {
		return nil, PolicyUnavailable("operation_unavailable", "The route does not allow this operation.")
	}
	if err := q.x.GuardrailSurfaceGate(route); err != nil {
		return nil, err
	}
	return route, nil
}

func (q retainedWorkRequest) boundReference() (*resources.Resource, *fault.Error) {
	referenced, err := q.referenced()
	if err != nil || len(referenced) == 0 {
		return nil, err
	}
	bound := referenced[0]
	for _, candidate := range referenced[1:] {
		if !samePin(candidate, bound) {
			return nil, retainedReferenceUnavailable()
		}
	}
	return bound, nil
}

func (q retainedWorkRequest) referenced() ([]*resources.Resource, *fault.Error) {
	previous, err := q.previousResponse()
	if err != nil {
		return nil, err
	}
	fileIDs := retainedFileIDs(q.source.Root())
	files := make([]*resources.Resource, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		resolved, _, readErr := q.server.readDurable(q.r.Context(), durableKind(resources.KindFile, fileID), q.x.keyID, fileID)
		if readErr != nil {
			return nil, retainedReferenceUnavailable()
		}
		files = append(files, resolved)
	}
	return slices.Concat(previous, files), nil
}

func (q retainedWorkRequest) previousResponse() ([]*resources.Resource, *fault.Error) {
	value, present := q.source.Root().Lookup("previous_response_id")
	if !present {
		return nil, nil
	}
	id, isText := value.Text()
	if !isText || id == "" {
		return nil, nil
	}
	resolved, _, readErr := q.server.readResponseResource(q.r.Context(), q.x.keyID, id)
	if readErr != nil {
		return nil, retainedReferenceUnavailable()
	}
	return []*resources.Resource{resolved}, nil
}

func (q retainedWorkRequest) rewrite(target retainedWorkTarget) ([]byte, *fault.Error) {
	rewritten, err := q.server.rewriteRetainedRequest(q.r.Context(), q.x, q.authority, target.pin, target.route, q.source, q.work.codec())
	if err != nil {
		return nil, retainedReferenceUnavailable()
	}
	return rewritten.Bytes(), nil
}

func (q retainedWorkRequest) perform(ctx context.Context, target retainedWorkTarget, body []byte) *fault.Error {
	endpoint, urlErr := target.pin.Provider.Connector().ResourceURL(target.pin.Model, target.path, nil)
	if urlErr != nil {
		return fault.ServerError(502, "provider_protocol_error", "The retained endpoint is unavailable.")
	}
	native, failure := q.server.PinnedDo(ctx, q.x, target.pin, http.MethodPost, endpoint, body, "application/json")
	if failure != nil {
		return retainedPublicError(UpstreamError(failure), failure.Status)
	}
	defer native.Body.Close()
	projected, err := q.accept(ctx, target, native)
	if err != nil {
		return err
	}
	native.Exchange.Complete(ClassSuccess)
	q.reply.end.status = native.StatusCode
	q.server.WriteStateJSONStatus(q.reply.w, q.x, projected, native.StatusCode, native.Exchange)
	return nil
}

func (q retainedWorkRequest) accept(ctx context.Context, target retainedWorkTarget, native *PinnedResponse) ([]byte, *fault.Error) {
	response, readErr := native.ReadBody(q.server.retainedResponseLimit())
	if readErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider returned an unsupported resource response.")
	}
	source, parseErr := oif.ParseJSON(response, oif.Limits{MaxBytes: retainedBodyLimit})
	if parseErr != nil || source.Root().Kind() != oif.Object {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid response.")
	}
	if q.work.generates() && observeRetainedUsage(native.Exchange, source, true) != nil {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider returned invalid usage.")
	}
	codec := q.work.codec()
	rewrite, referenceErr := retainedapi.PrepareRewrite(source, retainedResponseRefs(codec, source))
	if referenceErr != nil {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider response contained an unbound resource reference.")
	}
	projected, projectErr := q.server.projectRetainedGeneric(ctx, target.owner(q.x.keyID), codec, source, rewrite)
	if projectErr != nil {
		return nil, retainedProjectionError(native.Exchange, projectErr)
	}
	if !q.work.generates() {
		_ = observeRetainedUsage(native.Exchange, source, false)
	}
	return projected, nil
}

// Compaction has no parent resource, so its returned references are
// projected against the serving pin's owner-scoped identity.
func (t retainedWorkTarget) owner(keyID string) *resources.Resource {
	if t.resource != nil {
		return t.resource
	}
	return &resources.Resource{
		APIKeyID: keyID, RouteSlug: t.route.Slug, ProviderID: t.pin.Provider.ID, ProviderRevisionID: t.pin.Provider.RevisionID,
		RouteRevisionID: t.route.RevisionID, TargetID: t.pin.Target.ID, SlotID: t.pin.Slot.ID, CredentialID: t.pin.Slot.CredentialID,
	}
}
