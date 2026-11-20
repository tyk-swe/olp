package kernel

import (
	"encoding/json"
	"maps"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

func (q retainedRequest) change() {
	s, end := q.server, q.reply.end
	resource, err := q.addressed()
	if err != nil {
		q.reply.fail(err)
		return
	}
	pin, route, err := q.resolve(resource)
	if err != nil {
		q.reply.fail(err)
		return
	}
	body, err := q.rewriteChange(pin, route, resource)
	if err != nil {
		q.reply.fail(err)
		return
	}
	ctx, cancel := s.StateDeadline(q.r.Context(), route)
	defer cancel()
	if err := s.AdmitRetainedRequest(ctx, q.x, q.authority, pin, route, q.metersWork()); err != nil {
		q.reply.fail(err)
		return
	}
	endpoint, err := q.upstreamURL(ctx, pin, addressing(q.ancestors, resource.UpstreamID))
	if err != nil {
		q.reply.fail(err)
		return
	}
	mutation, err := s.fenceRetainedMutation(ctx, q.r, q.endpoint, resource, body)
	if err != nil {
		q.reply.fail(err)
		return
	}
	defer s.settleFence(ctx, mutation.Close)
	end.status = s.retainedDispatch(q.reply.w, q.r.WithContext(ctx), q.x, pin, q.endpoint, resource, q.parent, endpoint, body, q.input.contentType, mutation, q.reply.fail)
}

func addressing(ancestors map[string]string, upstreamID string) map[string]string {
	ids := maps.Clone(ancestors)
	ids["id"] = upstreamID
	return ids
}

func (q retainedRequest) addressed() (*resources.Resource, *fault.Error) {
	resource, err := q.owned()
	if err != nil {
		return nil, err
	}
	if resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
		return nil, PinUnavailable()
	}
	return resource, nil
}

func (q retainedRequest) owned() (*resources.Resource, *fault.Error) {
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

func (q retainedRequest) use() RetainedUse {
	switch e := q.endpoint; {
	case e.Metered && (e.Action == "update" || e.Action == "patch"):
		return RetainedNewWork
	case e.Action == "delete" || e.Action == "cancel" || e.Action == "list":
		return RetainedHousekeeping
	}
	return RetainedRetrieval
}

func (q retainedRequest) resolve(resource *resources.Resource) (*Pin, *routing.Route, *fault.Error) {
	e := q.endpoint
	pin, route, err := q.server.ResolveResource(q.r.Context(), q.x, q.authority, resource, e.Operation, q.use())
	q.reply.end.pin = pin
	if err != nil {
		return nil, nil, err
	}
	q.x.Route = route
	if !pin.Provider.Connector().Supports(e.Operation, e.Surface, "unary") {
		return nil, nil, PinUnavailable()
	}
	if e.Action != "delete" {
		if err := q.x.GuardrailSurfaceGate(route); err != nil {
			return nil, nil, err
		}
	}
	if RetainedUnsettled(resource.State) {
		return nil, nil, fault.ServerError(409, "submission_unknown", "Creation has not been reconciled; this request cannot be resubmitted.")
	}
	return pin, route, nil
}

func (q retainedRequest) rewriteChange(pin *Pin, route *routing.Route, resource *resources.Resource) ([]byte, *fault.Error) {
	if q.input.source.Root().Kind() != oif.Object {
		return q.input.body, nil
	}
	e := q.endpoint
	inputParent := q.parent
	if e.Kind == "eval" && e.Action == "update" {
		inputParent = resource
	}
	fields, err := retainedInputMetadata(pin, route, q.x.keyID, e, q.input.source, inputParent)
	if err != nil {
		return nil, fault.InvalidRequest("unsupported_parameter", err.Error(), nil)
	}
	if e.Operation == "eval" && e.Action == "update" {
		var metadata map[string]json.RawMessage
		_ = json.Unmarshal(resource.Metadata, &metadata)
		metadata["authorized_models"], _ = json.Marshal(fields["authorized_models"])
		metadata["serving_models"], _ = json.Marshal(fields["serving_models"])
		resource.Metadata, _ = json.Marshal(metadata)
	}
	rewritten, err := q.server.rewriteRetainedRequest(q.r.Context(), q.x, q.authority, pin, route, q.input.source, e)
	if err != nil {
		return nil, retainedReferenceUnavailable()
	}
	body, err := retainedCorrelationBody(e, resource, rewritten.Bytes())
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", "The resource correlation metadata could not be bound.", nil)
	}
	return body, nil
}
