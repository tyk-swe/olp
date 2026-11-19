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

type retainedAnswer struct {
	server   *Server
	x        *Call
	endpoint retainedapi.Endpoint
	resource *resources.Resource
	parent   *resources.Resource
	fence    *resources.Lifecycle
	deletion bool

	native       *PinnedResponse
	body         []byte
	commit       context.Context
	parsed       oif.Document
	parseErr     error
	rewrite      retainedapi.PreparedRewrite
	referenceErr error
	creation     *resources.PreparedCreation
}

func (s *Server) retainedDispatch(w http.ResponseWriter, r *http.Request, x *Call, p *Pin, e retainedapi.Endpoint, resource, parent *resources.Resource, endpoint string, body []byte, contentType string, lifecycle *resources.Lifecycle, fail func(*fault.Error)) int {
	a := &retainedAnswer{server: s, x: x, endpoint: e, resource: resource, parent: parent, fence: lifecycle}
	a.deletion = e.Action == "delete" && resource.Kind == "context_cache"
	if a.deletion {
		opened, err := s.Deps.Resources.BeginDeletion(r.Context(), resource)
		a.fence = opened
		if err != nil {
			s.settleFence(r.Context(), a.fence.Reject)
			fail(fault.ServerError(503, "provider_state_unavailable", "The deletion intent could not be committed."))
			return 503
		}
		defer s.settleFence(r.Context(), a.fence.Close)
	}

	native, failure := s.PinnedDo(r.Context(), x, p, e.Method, endpoint, body, contentType)
	if failure != nil {
		refusal := a.refused(r.Context(), failure)
		fail(refusal)
		return refusal.Status
	}
	defer native.Body.Close()
	response, readErr := native.ReadBody(s.retainedResponseLimit())
	if readErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an unsupported resource response."))
		return 502
	}
	commit, cancel := ResourceCommitContext(r.Context())
	defer cancel()
	a.native, a.body, a.commit = native, response, commit
	a.parse()
	if e.Action == "delete" {
		written, refusal := a.forget(w)
		if refusal != nil {
			fail(refusal)
			return refusal.Status
		}
		if written {
			return native.StatusCode
		}
	}
	if e.Action == "content" {
		return a.deliverContent(w)
	}
	projected, refusal := a.keep(r.Context())
	if refusal != nil {
		fail(refusal)
		return refusal.Status
	}
	s.WriteStateJSONStatus(w, x, projected, native.StatusCode, native.Exchange)
	return native.StatusCode
}

func (a *retainedAnswer) refused(ctx context.Context, failure *AttemptFailure) *fault.Error {
	e, isCreate := a.endpoint, a.endpoint.Action == "create"
	if a.resource != nil && (isCreate || a.deletion || e.Metered && (e.Action == "update" || e.Action == "patch")) && retainedRejectedWithoutWork(failure) {
		commit, cancel := ResourceCommitContext(ctx)
		rejectErr := a.fence.Reject(commit)
		cancel()
		if rejectErr != nil {
			return fault.ServerError(503, "provider_state_unavailable", "Provider rejection could not be committed.")
		}
	}
	if a.resource != nil && !isCreate && failure.Status == http.StatusNotFound {
		commit, cancel := ResourceCommitContext(ctx)
		defer cancel()
		if removeErr := a.server.Deps.Resources.Tombstone(commit, a.resource.ID); removeErr == nil {
			return fault.NotFoundError("not_found", "No resource with this identifier exists for this key.")
		}
	}
	return retainedPublicError(UpstreamError(failure), failure.Status)
}

func (a *retainedAnswer) parse() {
	if a.endpoint.Action == "content" {
		return
	}
	document := a.body
	if a.endpoint.Action == "delete" && a.native.StatusCode == http.StatusNoContent {
		document = []byte(`{}`)
	}
	a.parsed, a.parseErr = oif.ParseJSON(document, oif.Limits{MaxBytes: retainedBodyLimit})
	if a.parseErr == nil && a.parsed.Root().Kind() == oif.Object {
		a.rewrite, a.referenceErr = retainedapi.PrepareRewrite(a.parsed, retainedResponseRefs(a.endpoint, a.parsed))
	}
}

func (a *retainedAnswer) forget(w http.ResponseWriter) (bool, *fault.Error) {
	s := a.server
	if !retainedDeleteIdentityMatches(a.endpoint, a.parsed, a.resource, a.parent) {
		a.referenceErr = resources.ErrContract
	}
	if a.parseErr == nil && a.parsed.Root().Kind() == oif.Object && a.referenceErr == nil {
		_ = observeRetainedUsage(a.native.Exchange, a.parsed, false)
		a.native.Exchange.Complete(ClassSuccess)
	}
	if putErr := s.Deps.Resources.Tombstone(a.commit, a.resource.ID); putErr != nil {
		return false, fault.ServerError(503, "provider_state_unavailable", "Deletion was accepted but its local record could not be updated.")
	}
	if a.deletion {
		if err := a.fence.AcceptMutation(a.commit); err != nil {
			return false, fault.ServerError(503, "provider_state_unavailable", "The accepted deletion could not be committed.")
		}
	}
	if a.native.StatusCode != http.StatusNoContent {
		return false, nil
	}
	observation, observeErr := resources.ObserveRetained(a.resource, a.endpoint, a.parsed, s.Now())
	if observeErr != nil || s.Deps.Resources.RecordRetainedObservation(a.commit, a.resource, observation, nil) != nil {
		return false, fault.ServerError(503, "provider_state_unavailable", "Deletion evidence could not be committed.")
	}
	s.WriteStateJSONStatus(w, a.x, nil, a.native.StatusCode, a.native.Exchange)
	return true, nil
}

func (a *retainedAnswer) deliverContent(w http.ResponseWriter) int {
	s, x, native, response := a.server, a.x, a.native, a.body
	_ = observeRetainedUsage(native.Exchange, oif.Document{}, false)
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

func (a *retainedAnswer) keep(ctx context.Context) ([]byte, *fault.Error) {
	if refusal := a.identified(); refusal != nil {
		return nil, refusal
	}
	if refusal := a.bind(); refusal != nil {
		return nil, refusal
	}
	return a.record(ctx)
}

func (a *retainedAnswer) identified() *fault.Error {
	e := a.endpoint
	if a.parseErr != nil || a.parsed.Root().Kind() != oif.Object {
		return fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid resource object.")
	}
	if e.Action != "create" && e.Action != "list" && e.Action != "delete" && !(e.Action == "cancel" && e.Surface == "native") {
		identity, _ := a.parsed.Root().Lookup(e.Identity)
		if id, ok := identity.Text(); !ok || id != a.resource.UpstreamID {
			return fault.ServerError(502, "provider_protocol_error", "The provider returned a different resource identity.")
		}
	}
	return nil
}

func (a *retainedAnswer) bind() *fault.Error {
	s, e := a.server, a.endpoint
	if e.Action != "create" {
		if e.Action != "list" && e.Action != "delete" {
			if err := resources.EnrichRetained(a.resource, e, a.parsed, s.Now()); err != nil {
				return fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed.")
			}
		}
		return nil
	}
	ids, idErr := retainedCreateIDs(e, a.body)
	if idErr != nil {
		return fault.ServerError(502, "provider_protocol_error", "The provider response omitted a valid resource identity.")
	}
	prepared, prepareErr := a.fence.PrepareCreated(resources.CreationResult{
		IDs: ids, Observation: &resources.CreationObservation{Endpoint: e, Document: a.parsed, At: s.Now()},
	})
	if errors.Is(prepareErr, resources.ErrRetainedMetadata) {
		return fault.ServerError(502, "provider_protocol_error", "The provider returned invalid resource billing metadata.")
	}
	if prepareErr != nil {
		a.native.Exchange.Interrupt(ClassProtocol, true)
		return fault.ServerError(503, "provider_state_unavailable", "The provider accepted creation but its resource identities could not be committed.")
	}
	a.creation, a.resource = prepared, prepared.Resource()
	return nil
}

func (a *retainedAnswer) record(ctx context.Context) ([]byte, *fault.Error) {
	s, e, native := a.server, a.endpoint, a.native
	observation, observeErr := resources.ObserveRetained(a.resource, e, a.parsed, s.Now())
	if a.creation != nil {
		if err := a.creation.Commit(a.commit); err != nil {
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
	if s.persistRetainedResult(a.commit, a.resource, e, a.parsed) != nil {
		native.Exchange.Interrupt(ClassProtocol, true)
		return nil, fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed.")
	}
	projected, refusal := a.project(ctx)
	if refusal != nil {
		return nil, refusal
	}
	if e.Action != "delete" {
		_ = observeRetainedUsage(native.Exchange, a.parsed, false)
		native.Exchange.Complete(ClassSuccess)
	}
	if s.Deps.Resources.RecordRetainedObservation(a.commit, a.resource, observation, nil) != nil {
		return nil, fault.ServerError(503, "provider_state_unavailable", "Resource usage evidence could not be committed.")
	}
	if e.Metered && (e.Action == "update" || e.Action == "patch") {
		if err := a.fence.AcceptMutation(a.commit); err != nil {
			return nil, fault.ServerError(503, "provider_state_unavailable", "The accepted mutation could not be committed.")
		}
	}
	return projected, nil
}

func (a *retainedAnswer) project(ctx context.Context) ([]byte, *fault.Error) {
	projected, projectErr := a.server.projectRetainedGeneric(ctx, a.resource, a.endpoint, a.parsed, a.rewrite)
	if projectErr == nil {
		return projected, nil
	}
	if a.endpoint.Action == "delete" {
		return nil, fault.ServerError(502, "provider_protocol_error", "The provider response contained an unbound resource identity.")
	}
	return nil, retainedProjectionError(a.native.Exchange, projectErr)
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
