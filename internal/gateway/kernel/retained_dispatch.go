package kernel

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/usage"
)

type retainedExchange struct {
	server   *Server
	x        *Call
	endpoint retainedapi.Endpoint
	resource *resources.Resource
	parent   *resources.Resource
	fence    *resources.Lifecycle
	deletion bool
}

type retainedAnswer struct {
	native       *PinnedResponse
	body         []byte
	commit       context.Context
	parsed       oif.Document
	parseErr     error
	rewrite      retainedapi.PreparedRewrite
	referenceErr error
}

type RetainedDispatchRequest struct {
	Call        *Call
	Pin         *Pin
	Endpoint    retainedapi.Endpoint
	Resource    *resources.Resource
	Parent      *resources.Resource
	URL         string
	Body        []byte
	ContentType string
	Lifecycle   *resources.Lifecycle
	Fail        func(*fault.Error)
}

func (s *Server) RetainedDispatch(w http.ResponseWriter, r *http.Request, req RetainedDispatchRequest) int {
	x, p, e, resource, parent, endpoint, body, contentType, lifecycle, fail := req.Call, req.Pin, req.Endpoint, req.Resource, req.Parent, req.URL, req.Body, req.ContentType, req.Lifecycle, req.Fail
	deletion := e.Action == "delete" && resource.Kind == "context_cache"
	fence, err := s.dispatchFence(r.Context(), resource, lifecycle, deletion)
	if err != nil {
		s.SettleFence(r.Context(), fence.Reject)
		fail(fault.ServerError(503, "provider_state_unavailable", "The deletion intent could not be committed."))
		return 503
	}
	if deletion {
		defer s.SettleFence(r.Context(), fence.Close)
	}
	c := retainedExchange{server: s, x: x, endpoint: e, resource: resource, parent: parent, fence: fence, deletion: deletion}

	native, failure := s.PinnedDo(r.Context(), x, p, e.Method, endpoint, body, contentType)
	if failure != nil {
		refusal := c.refused(r.Context(), w.Header(), failure)
		fail(refusal)
		return refusal.Status
	}
	defer native.Body.Close()
	response, readErr := native.ReadBody(s.RetainedResponseLimit())
	if readErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an unsupported resource response."))
		return 502
	}
	commit, cancel := ResourceCommitContext(r.Context())
	defer cancel()
	a := c.answer(native, response, commit)
	if e.Action == "delete" {
		written, refusal := c.forget(w, a)
		if refusal != nil {
			fail(refusal)
			return refusal.Status
		}
		if written {
			return native.StatusCode
		}
	}
	if e.Action == "content" {
		return c.deliverContent(w, a)
	}
	projected, refusal := c.keep(r.Context(), a)
	if refusal != nil {
		fail(refusal)
		return refusal.Status
	}
	s.WriteStateJSONStatus(w, x, projected, native.StatusCode, native.Exchange)
	return native.StatusCode
}

func (s *Server) dispatchFence(ctx context.Context, resource *resources.Resource, lifecycle *resources.Lifecycle, deletion bool) (*resources.Lifecycle, error) {
	if !deletion {
		return lifecycle, nil
	}
	return s.Deps.Resources.BeginDeletion(ctx, resource)
}

func (c retainedExchange) refused(ctx context.Context, headers http.Header, failure *AttemptFailure) *fault.Error {
	recorded, err := RecordRetainedRejection(ctx, c.fence, failure)
	if err != nil {
		return err
	}
	e := c.endpoint
	if recorded && e.Metered && (e.Action == "update" || e.Action == "patch") &&
		failure.Status >= http.StatusBadRequest && failure.Status < http.StatusInternalServerError {
		headers.Set(headerMutationStatus, "not_accepted")
	}
	if c.resource != nil && e.Action != "create" && failure.Status == http.StatusNotFound {
		commit, cancel := ResourceCommitContext(ctx)
		defer cancel()
		if removeErr := c.server.Deps.Resources.Tombstone(commit, c.resource.ID); removeErr == nil {
			return fault.NotFoundError("not_found", "No resource with this identifier exists for this key.")
		}
	}
	return RetainedPublicError(UpstreamError(failure), failure.Status)
}

func RecordRetainedRejection(ctx context.Context, fence *resources.Lifecycle, failure *AttemptFailure) (bool, *fault.Error) {
	if fence == nil || !failure.rejectedWithoutWork() {
		return false, nil
	}
	commit, cancel := ResourceCommitContext(ctx)
	defer cancel()
	if err := fence.Reject(commit); err != nil {
		return false, fault.ServerError(503, "provider_state_unavailable", "Provider rejection could not be committed.")
	}
	return true, nil
}

func (c retainedExchange) answer(native *PinnedResponse, body []byte, commit context.Context) retainedAnswer {
	a := retainedAnswer{native: native, body: body, commit: commit}
	if c.endpoint.Action == "content" {
		return a
	}
	parsed, parseErr := oif.ParseJSON(c.document(native, body), oif.Limits{MaxBytes: RetainedBodyLimit})
	rewrite, referenceErr := c.references(parsed, parseErr)
	return retainedAnswer{native: native, body: body, commit: commit, parsed: parsed, parseErr: parseErr, rewrite: rewrite, referenceErr: referenceErr}
}

func (c retainedExchange) document(native *PinnedResponse, body []byte) []byte {
	if c.endpoint.Action == "delete" && native.StatusCode == http.StatusNoContent {
		return []byte(`{}`)
	}
	return body
}

func (c retainedExchange) references(parsed oif.Document, parseErr error) (retainedapi.PreparedRewrite, error) {
	var rewrite retainedapi.PreparedRewrite
	var referenceErr error
	if parseErr == nil && parsed.Root().Kind() == oif.Object {
		rewrite, referenceErr = retainedapi.PrepareRewrite(parsed, RetainedResponseRefs(c.endpoint, parsed))
	}
	if c.endpoint.Action == "delete" && !retainedDeleteIdentityMatches(c.endpoint, parsed, c.resource, c.parent) {
		return rewrite, resources.ErrContract
	}
	return rewrite, referenceErr
}

func (c retainedExchange) forget(w http.ResponseWriter, a retainedAnswer) (bool, *fault.Error) {
	s := c.server
	if a.parseErr == nil && a.parsed.Root().Kind() == oif.Object && a.referenceErr == nil {
		_ = ObserveRetainedUsage(a.native.Exchange, a.parsed, false)
		a.native.Exchange.Complete(ClassSuccess)
	}
	if putErr := s.Deps.Resources.Tombstone(a.commit, c.resource.ID); putErr != nil {
		return false, fault.ServerError(503, "provider_state_unavailable", "Deletion was accepted but its local record could not be updated.")
	}
	if c.deletion {
		if err := c.fence.AcceptMutation(a.commit); err != nil {
			return false, fault.ServerError(503, "provider_state_unavailable", "The accepted deletion could not be committed.")
		}
	}
	if a.native.StatusCode != http.StatusNoContent {
		return false, nil
	}
	observation, observeErr := resources.ObserveRetained(c.resource, c.endpoint, a.parsed, s.Now())
	if observeErr != nil || s.Deps.Resources.RecordRetainedObservation(a.commit, c.resource, observation, nil) != nil {
		return false, fault.ServerError(503, "provider_state_unavailable", "Deletion evidence could not be committed.")
	}
	s.WriteStateJSONStatus(w, c.x, nil, a.native.StatusCode, a.native.Exchange)
	return true, nil
}

func (c retainedExchange) deliverContent(w http.ResponseWriter, a retainedAnswer) int {
	s, x, native, response := c.server, c.x, a.native, a.body
	_ = ObserveRetainedUsage(native.Exchange, oif.Document{}, false)
	native.Exchange.Complete(ClassSuccess)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	w.Header().Set("Content-Type", "application/octet-stream")
	x.ResponseHeaders(w.Header(), false)
	w.WriteHeader(native.StatusCode)
	n, writeErr := w.Write(response)
	if writeErr == nil && n == len(response) {
		writeErr = http.NewResponseController(w).Flush()
	}
	if writeErr != nil || n != len(response) {
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The resource result could not be delivered.")
		native.Exchange.Delivery(DeliveryState(n))
		s.Finish(x, &Outcome{Err: x.Failure, Committed: true, Cancelled: true}, 0)
		return native.StatusCode
	}
	if n > 0 {
		x.Delivered(s.Now())
	}
	native.Exchange.Delivery(usage.ClientTerminal)
	s.Finish(x, &Outcome{Committed: true}, native.StatusCode)
	return native.StatusCode
}

func (c retainedExchange) keep(ctx context.Context, a retainedAnswer) ([]byte, *fault.Error) {
	if refusal := c.identified(a); refusal != nil {
		return nil, refusal
	}
	creation, resource, refusal := c.bind(a)
	if refusal != nil {
		return nil, refusal
	}
	return c.record(ctx, a, creation, resource)
}

func (c retainedExchange) identified(a retainedAnswer) *fault.Error {
	e := c.endpoint
	if a.parseErr != nil || a.parsed.Root().Kind() != oif.Object {
		return fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid resource object.")
	}
	if e.Action != "create" && e.Action != "list" && e.Action != "delete" && !(e.Action == "cancel" && e.Surface == "native") {
		identity, _ := a.parsed.Root().Lookup(e.Identity)
		if id, ok := identity.Text(); !ok || id != c.resource.UpstreamID {
			return fault.ServerError(502, "provider_protocol_error", "The provider returned a different resource identity.")
		}
	}
	return nil
}

func (c retainedExchange) bind(a retainedAnswer) (*resources.PreparedCreation, *resources.Resource, *fault.Error) {
	s, e := c.server, c.endpoint
	if e.Action != "create" {
		if e.Action != "list" && e.Action != "delete" {
			if err := resources.EnrichRetained(c.resource, e, a.parsed, s.Now()); err != nil {
				return nil, nil, fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed.")
			}
		}
		return nil, c.resource, nil
	}
	ids, idErr := RetainedCreateIDs(e, a.body)
	if idErr != nil {
		return nil, nil, fault.ServerError(502, "provider_protocol_error", "The provider response omitted a valid resource identity.")
	}
	prepared, prepareErr := c.fence.PrepareCreated(resources.CreationResult{
		IDs: ids, Observation: &resources.CreationObservation{Endpoint: e, Document: a.parsed, At: s.Now()},
	})
	if errors.Is(prepareErr, resources.ErrRetainedMetadata) {
		return nil, nil, fault.ServerError(502, "provider_protocol_error", "The provider returned invalid resource billing metadata.")
	}
	if prepareErr != nil {
		a.native.Exchange.Interrupt(ClassProtocol, true)
		return nil, nil, fault.ServerError(503, "provider_state_unavailable", "The provider accepted creation but its resource identities could not be committed.")
	}
	return prepared, prepared.Resource(), nil
}

func (c retainedExchange) record(ctx context.Context, a retainedAnswer, creation *resources.PreparedCreation, resource *resources.Resource) ([]byte, *fault.Error) {
	s, e, native := c.server, c.endpoint, a.native
	observation, observeErr := resources.ObserveRetained(resource, e, a.parsed, s.Now())
	if creation != nil {
		if err := creation.Commit(a.commit); err != nil {
			if a.referenceErr == nil && observeErr == nil {
				native.Exchange.Interrupt(ClassProtocol, true)
			}
			return nil, fault.ServerError(503, "provider_state_unavailable", "The provider accepted creation but its resource identities could not be committed.")
		}
	}
	if a.referenceErr != nil {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider response contained an unbound resource identity.")
	}
	if observeErr != nil {
		return nil, fault.ServerError(503, "provider_state_unavailable", "Resource usage evidence could not be committed.")
	}
	if s.PersistRetainedResult(a.commit, resource, e, a.parsed) != nil {
		native.Exchange.Interrupt(ClassProtocol, true)
		return nil, fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed.")
	}
	projected, refusal := c.project(ctx, a, resource)
	if refusal != nil {
		return nil, refusal
	}
	if e.Action != "delete" {
		_ = ObserveRetainedUsage(native.Exchange, a.parsed, false)
		native.Exchange.Complete(ClassSuccess)
	}
	if s.Deps.Resources.RecordRetainedObservation(a.commit, resource, observation, nil) != nil {
		return nil, fault.ServerError(503, "provider_state_unavailable", "Resource usage evidence could not be committed.")
	}
	if e.Metered && (e.Action == "update" || e.Action == "patch") {
		if err := c.fence.AcceptMutation(a.commit); err != nil {
			return nil, fault.ServerError(503, "provider_state_unavailable", "The accepted mutation could not be committed.")
		}
	}
	return projected, nil
}

func (c retainedExchange) project(ctx context.Context, a retainedAnswer, resource *resources.Resource) ([]byte, *fault.Error) {
	projected, projectErr := c.server.ProjectRetainedGeneric(ctx, resource, c.endpoint, a.parsed, a.rewrite)
	if projectErr == nil {
		return projected, nil
	}
	if c.endpoint.Action == "delete" {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider response contained an unbound resource identity.")
	}
	return nil, RetainedProjectionError(a.native.Exchange, projectErr)
}

func retainedDeleteIdentityMatches(e retainedapi.Endpoint, document oif.Document, resource, parent *resources.Resource) bool {
	identity, present := document.Root().Lookup(e.Identity)
	if !present {
		return true
	}
	expected := resource
	if e.Kind == "conversation_item" {
		expected = parent
	}
	id, valid := identity.Text()
	return valid && expected != nil && id == expected.UpstreamID
}

func (s *Server) PersistRetainedResult(ctx context.Context, r *resources.Resource, e retainedapi.Endpoint, source oif.Document) error {
	if r == nil || e.Action == "list" || e.Action == "delete" {
		return nil
	}
	// Create results were already committed with their submission identities.
	// Empty item creations intentionally leave a deleted placeholder.
	if e.Action != "create" {
		save := s.Deps.Resources.SaveRetainedResult
		if e.Metered && (e.Action == "update" || e.Action == "patch") {
			save = s.Deps.Resources.SaveRetainedMutationResult
		}
		if err := save(ctx, r); err != nil {
			return err
		}
	}
	if e.Operation == "fine_tuning" {
		return s.Deps.Resources.DiscoverRetainedModels(ctx, r, source)
	}
	return nil
}
