package kernel

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

func (q *retainedRequest) change() {
	s := q.server
	resource, err := q.addressed()
	if err != nil {
		q.fail(err)
		return
	}
	route, err := q.resolve(resource)
	if err != nil {
		q.fail(err)
		return
	}
	if err := q.rewriteChange(route, resource); err != nil {
		q.fail(err)
		return
	}
	q.ids["id"] = resource.UpstreamID
	ctx, cancel := s.StateDeadline(q.r.Context(), route)
	defer cancel()
	if err := s.admitRetainedRequest(ctx, q.x, q.authority, q.pin, route, q.metersWork()); err != nil {
		q.fail(err)
		return
	}
	endpoint, err := q.upstreamURL(ctx)
	if err != nil {
		q.fail(err)
		return
	}
	mutation, err := s.fenceRetainedMutation(ctx, q.r, q.x, q.endpoint, resource, q.body)
	if err != nil {
		q.fail(err)
		return
	}
	defer s.settleFence(ctx, mutation.Close)
	q.status = s.retainedDispatch(q.w, q.r.WithContext(ctx), q.x, q.pin, q.endpoint, resource, q.parent, endpoint, q.body, q.contentType, mutation, q.fail)
}

func (q *retainedRequest) addressed() (*resources.Resource, *fault.Error) {
	resource, err := q.owned()
	if err != nil {
		return nil, err
	}
	if resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
		return nil, PinUnavailable()
	}
	return resource, nil
}

func (q *retainedRequest) owned() (*resources.Resource, *fault.Error) {
	if q.endpoint.Action == "list" {
		if q.parent == nil {
			return nil, retainedMissing()
		}
		return q.parent, nil
	}
	lifecycle, err := q.server.Deps.Resources.OpenOwned(q.r.Context(), q.endpoint.Kind, q.x.keyID, q.r.PathValue("id"))
	if err != nil {
		return nil, retainedMissing()
	}
	resource := lifecycle.Resource()
	if q.parent != nil && (resource.ParentID == nil || *resource.ParentID != q.parent.UUID) {
		return nil, retainedMissing()
	}
	return resource, nil
}

func (q *retainedRequest) use() RetainedUse {
	switch e := q.endpoint; {
	case e.Metered && (e.Action == "update" || e.Action == "patch"):
		return RetainedNewWork
	case e.Action == "delete" || e.Action == "cancel" || e.Action == "list":
		return RetainedHousekeeping
	}
	return RetainedRetrieval
}

func (q *retainedRequest) resolve(resource *resources.Resource) (*routing.Route, *fault.Error) {
	e := q.endpoint
	pin, route, err := q.server.ResolveResource(q.r.Context(), q.x, q.authority, resource, e.Operation, q.use())
	q.pin = pin
	if err != nil {
		return nil, err
	}
	q.x.Route = route
	if !pin.Provider.Connector().Supports(e.Operation, e.Surface, "unary") {
		return nil, PinUnavailable()
	}
	if e.Action != "delete" {
		if err := q.x.GuardrailSurfaceGate(route); err != nil {
			return nil, err
		}
	}
	if resource.State == "submitting" || resource.State == "submission_unknown" || resource.State == "not_accepted" {
		return nil, fault.ServerError(409, "submission_unknown", "Creation has not been reconciled; this request cannot be resubmitted.")
	}
	return route, nil
}

func (q *retainedRequest) rewriteChange(route *routing.Route, resource *resources.Resource) *fault.Error {
	if q.source.Root().Kind() != oif.Object {
		return nil
	}
	e := q.endpoint
	inputParent := q.parent
	if e.Kind == "eval" && e.Action == "update" {
		inputParent = resource
	}
	fields, err := retainedInputMetadata(q.pin, route, q.x.keyID, e, q.source, inputParent)
	if err != nil {
		return fault.InvalidRequest("unsupported_parameter", err.Error(), nil)
	}
	if e.Operation == "eval" && e.Action == "update" {
		var metadata map[string]json.RawMessage
		_ = json.Unmarshal(resource.Metadata, &metadata)
		metadata["authorized_models"], _ = json.Marshal(fields["authorized_models"])
		metadata["serving_models"], _ = json.Marshal(fields["serving_models"])
		resource.Metadata, _ = json.Marshal(metadata)
	}
	rewritten, err := q.server.rewriteRetainedRequest(q.r.Context(), q.x, q.authority, q.pin, route, q.source, e)
	if err != nil {
		return retainedReferenceUnavailable()
	}
	body, err := retainedCorrelationBody(e, resource, rewritten.Bytes())
	if err != nil {
		return fault.InvalidRequest("invalid_request", "The resource correlation metadata could not be bound.", nil)
	}
	q.body = body
	return nil
}
