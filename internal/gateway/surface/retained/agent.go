package retained

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

type agentRequest struct {
	server    surface
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	reply     retainedReply
	endpoint  retainedapi.Endpoint
	body      []byte
	source    oif.Document
}

func (s surface) retainedLifecycle(w http.ResponseWriter, r *http.Request, e retainedapi.Endpoint) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	x.BeginRetainedResource(e.Operation)
	end := &retainedEnd{status: http.StatusInternalServerError}
	defer func() {
		s.kernel.Finish(x, nil, end.status)
		s.kernel.ResourceSettle(r.Context(), x, end.pin)
	}()
	reply := retainedReply{w: w, x: x, action: e.Action, surface: e.Surface, end: end}
	if !authority.Policy.AllowProviderState {
		reply.fail(kernel.ProviderStateForbidden())
		return
	}
	if err := agentQuery(r.URL.Query(), e.Action == "list"); err != nil {
		reply.fail(err)
		return
	}
	if e.Action == "list" {
		end.status = s.listRetainedLifecycle(w, r, x, authority, e, reply.fail)
		return
	}
	body, source, err := s.readAgent(r, e.Method)
	if err != nil {
		reply.fail(err)
		return
	}
	q := agentRequest{server: s, r: r, x: x, authority: authority, reply: reply, endpoint: e, body: body, source: source}
	if e.Action == "create" {
		q.create()
		return
	}
	q.change()
}

func agentQuery(query url.Values, pages bool) *fault.Error {
	for name := range query {
		if name == "key" || pages && (name == "page_size" || name == "page_token") {
			continue
		}
		return fault.InvalidRequest("unsupported_parameter", "This resource method does not accept that query parameter.", nil)
	}
	return nil
}

func (s surface) readAgent(r *http.Request, method string) ([]byte, oif.Document, *fault.Error) {
	if method != http.MethodPost && method != http.MethodPatch {
		return nil, oif.Document{}, nil
	}
	body, err := s.kernel.Gate.ReadBody(r)
	if err != nil {
		return nil, oif.Document{}, err
	}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.kernel.Config.MaxBodyBytes)})
	if parseErr != nil || source.Root().Kind() != oif.Object {
		return nil, oif.Document{}, fault.InvalidRequest("invalid_request", "A valid JSON object is required.", nil)
	}
	if err := retainedAgentInput(source); err != nil {
		return nil, oif.Document{}, err
	}
	normalized, parseErr := normalizeRetainedAgent(source)
	if parseErr != nil {
		return nil, oif.Document{}, fault.InvalidRequest("invalid_request", "The agent definition could not be normalized.", nil)
	}
	return body, normalized, nil
}

func (q agentRequest) create() {
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
	base, err := q.baseAgent(route)
	if err != nil {
		q.reply.fail(err)
		return
	}
	pin, err := s.kernel.SelectBoundPinSurface(q.r.Context(), q.x, route, e.Operation, e.Surface, "unary", kernel.ResourceDemand(q.x), base, func(provider *routing.Provider, model string) bool {
		return provider.CredentialSource != "caller" && provider.Connector().Supports(e.Operation, e.Surface, "unary") && provider.Supports(model, e.Operation, e.Surface, "unary")
	})
	end.pin = pin
	if err != nil {
		q.reply.fail(err)
		return
	}
	body, err := q.bindBase(pin, route)
	if err != nil {
		q.reply.fail(err)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(q.r.Context(), route)
	defer cancel()
	endpoint, err := q.admit(ctx, pin, route, "agents")
	if err != nil {
		q.reply.fail(err)
		return
	}
	lifecycle, err := q.begin(ctx, pin, route, submission)
	if err != nil {
		q.reply.fail(err)
		return
	}
	defer s.kernel.SettleFence(q.r.Context(), lifecycle.Close)
	q.exchange(ctx, pin, endpoint, body, lifecycle.Resource(), lifecycle)
}

func (q agentRequest) submission() (retainedSubmission, *fault.Error) {
	slug := strings.TrimSpace(q.r.Header.Get(kernel.StateRouteHeader))
	if slug == "" {
		return retainedSubmission{}, fault.InvalidRequest("missing_required_parameter", "X-OLP-Route is required.", nil)
	}
	key := q.r.Header.Get("Idempotency-Key")
	if key == "" {
		return retainedSubmission{}, fault.InvalidRequest("missing_required_parameter", "Idempotency-Key is required for resource creation.", nil)
	}
	if len(key) > 128 {
		return retainedSubmission{}, fault.InvalidRequest("invalid_idempotency_key", "Idempotency-Key must be at most 128 bytes.", nil)
	}
	digest, err := q.server.kernel.RetainedSubmissionDigest(q.endpoint, slug, q.body)
	if err != nil {
		return retainedSubmission{}, err
	}
	submission := retainedSubmission{slug: slug, key: key, digest: digest}
	return submission, q.server.unsubmitted(q.r.Context(), q.reply, q.endpoint.Operation, submission)
}

func (q agentRequest) creationRoute(slug string) (*routing.Route, *fault.Error) {
	candidate, ok := q.x.Snapshot().FindRoute(slug)
	if !ok {
		return nil, fault.ModelNotFound(slug)
	}
	route := &candidate
	q.x.Route = route
	if !route.Fidelity.Strict() {
		return nil, kernel.PolicyUnavailable("strict_route_required", "Native agent resources require a strict route.")
	}
	if !q.authority.Allows("inference", route.Slug, route.ProjectID, q.server.kernel.Now()) {
		return nil, fault.PermissionError("route_forbidden", "The API key cannot use this route.")
	}
	if !slices.Contains(route.Operations, q.endpoint.Operation) {
		return nil, kernel.PolicyUnavailable("operation_unavailable", "The route does not allow this operation.")
	}
	if err := q.x.GuardrailSurfaceGate(route); err != nil {
		return nil, err
	}
	return route, nil
}

func (q agentRequest) baseAgent(route *routing.Route) (*resources.Resource, *fault.Error) {
	value, present := q.source.Root().Lookup("base_agent")
	if !present || value.Kind() == oif.Null {
		return nil, nil
	}
	id, _ := value.Text()
	base, err := q.server.kernel.Deps.Resources.Get(q.r.Context(), "agent_resource", q.x.KeyID(), id)
	if err != nil || base.RouteSlug != route.Slug || base.RouteRevisionID != route.RevisionID || kernel.RetainedUnsettled(base.State) || base.ContractVersion == nil || *base.ContractVersion != retainedapi.Revision {
		return nil, retainedReferenceUnavailable()
	}
	return base, nil
}

func (q agentRequest) bindBase(pin *kernel.Pin, route *routing.Route) ([]byte, *fault.Error) {
	bound, err := retainedapi.Rewrite(q.source, map[string]string{"base_agent": "agent_resource"}, func(kind, id string) (string, error) {
		base, getErr := q.server.kernel.Deps.Resources.Get(q.r.Context(), kind, q.x.KeyID(), id)
		if getErr != nil || !kernel.RetainedAffinity(base, pin, route) || kernel.RetainedUnsettled(base.State) {
			return "", resources.ErrNotFound
		}
		return base.UpstreamID, nil
	})
	if err != nil {
		return nil, retainedReferenceUnavailable()
	}
	return bound.Bytes(), nil
}

func (q agentRequest) admit(ctx context.Context, pin *kernel.Pin, route *routing.Route, path string) (string, *fault.Error) {
	s := q.server
	if err := s.kernel.RetainedControlPolicy(q.x, pin, route); err != nil {
		return "", err
	}
	if err := s.kernel.ReserveState(ctx, q.x, q.authority, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
		return "", err
	}
	if err := s.kernel.GateRetainedSlot(ctx, q.x, pin, route); err != nil {
		return "", err
	}
	endpoint, err := pin.Provider.Connector().ResourceURL(pin.Model, path, nil)
	if err != nil {
		return "", fault.ServerError(502, "provider_protocol_error", "The retained endpoint is unavailable.")
	}
	return endpoint, nil
}

func (q agentRequest) begin(ctx context.Context, pin *kernel.Pin, route *routing.Route, submission retainedSubmission) (*resources.Lifecycle, *fault.Error) {
	e := q.endpoint
	meta, _ := json.Marshal(map[string]string{"upstream_model": pin.Model, "contract_revision": retainedapi.Revision})
	version := retainedapi.Revision
	candidate := &resources.Resource{Kind: e.Kind, APIKeyID: q.x.KeyID(), RouteSlug: route.Slug, ProviderID: pin.Provider.ID, ProviderRevisionID: pin.Provider.RevisionID, RouteRevisionID: route.RevisionID, TargetID: pin.Target.ID, SlotID: pin.Slot.ID, CredentialID: pin.Slot.CredentialID, Metadata: meta, ContractVersion: &version}
	lifecycle, err := q.server.kernel.Deps.Resources.BeginCreate(ctx, candidate, resources.Submission{Operation: e.Operation, Key: submission.key, Digest: submission.digest})
	if errors.Is(err, resources.ErrSubmissionConflict) {
		return nil, retainedSubmissionConflict()
	}
	if err != nil {
		return nil, fault.ServerError(503, "provider_state_unavailable", "The submission could not be persisted.")
	}
	q.reply.w.Header().Set("X-OLP-Resource-ID", lifecycle.Resource().ID)
	if lifecycle.Existing() {
		return nil, retainedSubmissionExists()
	}
	if err := lifecycle.Dispatch(ctx); err != nil {
		return nil, fault.ServerError(503, "provider_state_unavailable", "The submission could not be fenced.")
	}
	return lifecycle, nil
}

func (q agentRequest) change() {
	s, e, end := q.server, q.endpoint, q.reply.end
	lifecycle, getErr := s.kernel.Deps.Resources.OpenOwned(q.r.Context(), e.Kind, q.x.KeyID(), q.r.PathValue("id"))
	if getErr != nil {
		q.reply.fail(retainedMissing())
		return
	}
	resource := lifecycle.Resource()
	pin, route, err := q.resolve(resource)
	end.pin = pin
	if err != nil {
		q.reply.fail(err)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(q.r.Context(), route)
	defer cancel()
	endpoint, err := q.admit(ctx, pin, route, "agents/"+url.PathEscape(resource.UpstreamID))
	if err != nil {
		q.reply.fail(err)
		return
	}
	q.exchange(ctx, pin, endpoint, q.body, resource, nil)
}

func (q agentRequest) resolve(resource *resources.Resource) (*kernel.Pin, *routing.Route, *fault.Error) {
	e := q.endpoint
	if resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
		return nil, nil, kernel.PinUnavailable()
	}
	use := kernel.RetainedRetrieval
	if e.Action == "delete" {
		use = kernel.RetainedHousekeeping
	}
	pin, route, err := q.server.kernel.ResolveResource(q.r.Context(), q.x, q.authority, resource, e.Operation, use)
	if err != nil {
		return pin, nil, err
	}
	q.x.Route = route
	if !pin.Provider.Connector().Supports(e.Operation, e.Surface, "unary") {
		return pin, nil, kernel.PinUnavailable()
	}
	if e.Action != "delete" {
		if err := q.x.GuardrailSurfaceGate(route); err != nil {
			return pin, nil, err
		}
	}
	if kernel.RetainedUnsettled(resource.State) {
		return pin, nil, fault.ServerError(409, "submission_unknown", "Creation has not been reconciled; this request cannot be resubmitted.")
	}
	return pin, route, nil
}

func (q agentRequest) exchange(ctx context.Context, pin *kernel.Pin, endpoint string, body []byte, resource *resources.Resource, creation *resources.Lifecycle) {
	s, e, end := q.server, q.endpoint, q.reply.end
	native, failure := s.kernel.PinnedDo(ctx, q.x, pin, e.Method, endpoint, body, "application/json")
	if failure != nil {
		q.reply.fail(q.refused(ctx, resource, creation, failure))
		return
	}
	defer native.Body.Close()
	response, readErr := native.ReadBody(s.kernel.RetainedResponseLimit())
	if readErr != nil {
		q.reply.fail(fault.ServerError(502, "provider_protocol_error", "The provider response exceeded the supported resource contract."))
		return
	}
	if native.StatusCode < 200 || native.StatusCode >= 300 {
		q.reply.fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an unsupported resource response."))
		return
	}
	commitCtx, cancel := kernel.ResourceCommitContext(ctx)
	defer cancel()
	if e.Action == "delete" {
		q.forget(commitCtx, native, resource)
		return
	}
	kept, rewrite, err := q.keep(commitCtx, native, response, resource, creation)
	if err != nil {
		q.reply.fail(err)
		return
	}
	projected, projectErr := s.projectRetainedAgent(ctx, kept, rewrite)
	if projectErr != nil {
		q.reply.fail(kernel.RetainedProjectionError(native.Exchange, projectErr))
		return
	}
	native.Exchange.CompleteRetainedControl()
	end.status = native.StatusCode
	s.kernel.WriteStateJSONStatus(q.reply.w, q.x, projected, end.status, native.Exchange)
}

func (q agentRequest) refused(ctx context.Context, resource *resources.Resource, creation *resources.Lifecycle, failure *kernel.AttemptFailure) *fault.Error {
	if err := kernel.RecordRetainedRejection(ctx, creation, failure); err != nil {
		return err
	}
	if q.endpoint.Action == "create" || failure.Status != http.StatusNotFound {
		return kernel.RetainedPublicError(kernel.UpstreamError(failure), failure.Status)
	}
	commitCtx, cancel := kernel.ResourceCommitContext(ctx)
	defer cancel()
	if err := q.server.kernel.Deps.Resources.Tombstone(commitCtx, resource.ID); err != nil {
		return fault.ServerError(503, "provider_state_unavailable", "The deleted resource could not be reconciled.")
	}
	return retainedMissing()
}

func (q agentRequest) forget(ctx context.Context, native *kernel.PinnedResponse, resource *resources.Resource) {
	s, end := q.server, q.reply.end
	native.Exchange.CompleteRetainedControl()
	if err := s.kernel.Deps.Resources.Tombstone(ctx, resource.ID); err != nil {
		q.reply.fail(fault.ServerError(503, "provider_state_unavailable", "Deletion was accepted but its local record could not be updated."))
		return
	}
	end.status = native.StatusCode
	if end.status == http.StatusNoContent {
		s.kernel.WriteStateJSONStatus(q.reply.w, q.x, nil, end.status, native.Exchange)
		return
	}
	s.kernel.WriteStateJSONStatus(q.reply.w, q.x, []byte(`{}`), end.status, native.Exchange)
}

func (q agentRequest) keep(ctx context.Context, native *kernel.PinnedResponse, response []byte, resource *resources.Resource, creation *resources.Lifecycle) (*resources.Resource, retainedapi.PreparedRewrite, *fault.Error) {
	nativeID, valid := kernel.NativeResourceID(response, kernel.RetainedBodyLimit)
	if !valid {
		return nil, retainedapi.PreparedRewrite{}, fault.ServerError(502, "provider_protocol_error", "The provider response omitted a valid resource identity.")
	}
	if creation == nil && nativeID != resource.UpstreamID {
		return nil, retainedapi.PreparedRewrite{}, fault.ServerError(502, "provider_protocol_error", "The provider returned a mismatched resource identity.")
	}
	rewrite, referenceErr := prepareRetainedAgent(response)
	unbound := fault.ServerError(502, "provider_protocol_error", "The provider response contained an unbound resource identity.")
	if creation == nil {
		if referenceErr != nil {
			return nil, retainedapi.PreparedRewrite{}, unbound
		}
		return resource, rewrite, nil
	}
	prepared, prepareErr := creation.PrepareCreated(resources.CreationResult{IDs: []string{nativeID}})
	if prepareErr != nil || prepared.Commit(ctx) != nil {
		if referenceErr == nil {
			native.Exchange.Interrupt(kernel.ClassProtocol, true)
		}
		return nil, retainedapi.PreparedRewrite{}, fault.ServerError(503, "provider_state_unavailable", "The provider accepted creation but its resource identity could not be committed.")
	}
	if referenceErr != nil {
		return nil, retainedapi.PreparedRewrite{}, unbound
	}
	return prepared.Resource(), rewrite, nil
}
