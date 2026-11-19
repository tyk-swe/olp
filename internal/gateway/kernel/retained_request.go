package kernel

import (
	"context"
	"net/http"
	"strconv"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

type retainedRequest struct {
	server      *Server
	w           http.ResponseWriter
	r           *http.Request
	endpoint    retainedapi.Endpoint
	x           *Call
	authority   keyauthority.Authority
	pin         *Pin
	status      int
	body        []byte
	source      oif.Document
	contentType string
	parent      *resources.Resource
	ids         map[string]string
}

func (s *Server) retainedGeneric(w http.ResponseWriter, r *http.Request, e retainedapi.Endpoint) {
	x, authority, done := s.StateBegin(w, r, retainedSurfaceFormat(e.Surface))
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	x.operationOverride = e.Operation
	x.mode = "unary"
	x.actor = "api_key_resource"
	x.retained = retainedOperationForAction(e.Action)
	q := &retainedRequest{
		server: s, w: w, r: r, endpoint: e, x: x, authority: authority,
		status: http.StatusInternalServerError, ids: map[string]string{},
	}
	defer q.finish()
	q.serve()
}

func (q *retainedRequest) finish() {
	q.server.Finish(q.x, nil, q.status)
	q.server.retainedSettle(q.r.Context(), q.x, q.pin)
}

func (q *retainedRequest) fail(err *fault.Error) {
	if q.endpoint.Action == "create" {
		c := *err
		c.NoRetry = true
		err = &c
	}
	q.x.Failure = err
	q.status = err.Status
	fault.WriteSurfaceError(q.w, err, q.endpoint.Surface)
}

func retainedMissing() *fault.Error {
	return fault.NotFoundError("not_found", "No resource with this identifier exists for this key.")
}

func retainedReferenceUnavailable() *fault.Error {
	return fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil)
}

func (q *retainedRequest) serve() {
	if !q.authority.Policy.AllowProviderState {
		q.fail(providerStateForbidden())
		return
	}
	if q.endpoint.Action == "list" && len(q.endpoint.Ancestors) == 0 {
		q.status = q.server.listRetainedGeneric(q.w, q.r, q.x, q.authority, q.endpoint, q.fail)
		return
	}
	if err := q.read(); err != nil {
		q.fail(err)
		return
	}
	if err := q.resolveAncestors(); err != nil {
		q.fail(err)
		return
	}
	if q.endpoint.Action == "create" {
		q.create()
		return
	}
	q.change()
}

func (q *retainedRequest) read() *fault.Error {
	body, source, contentType, err := q.server.readRetainedBody(q.r, q.endpoint)
	if err != nil {
		return err
	}
	q.body, q.source, q.contentType = body, source, contentType
	if q.changesOnlyDisplayFields() {
		q.endpoint.Metered = false
		q.x.retained = retainedOperation{use: RetainedHousekeeping}
	}
	return nil
}

func (q *retainedRequest) changesOnlyDisplayFields() bool {
	if q.endpoint.Kind != "vector_store" || q.endpoint.Action != "update" {
		return false
	}
	for _, field := range q.source.Root().Members() {
		if field.Name != "name" && field.Name != "metadata" {
			return false
		}
	}
	return true
}

func (q *retainedRequest) resolveAncestors() *fault.Error {
	ctx := q.r.Context()
	for i, kind := range q.endpoint.Ancestors {
		name := "parent" + strconv.Itoa(i)
		got, err := q.server.Deps.Resources.Get(ctx, kind, q.x.keyID, q.r.PathValue(name))
		if err != nil {
			return retainedMissing()
		}
		if q.parent != nil && (got.ParentID == nil || *got.ParentID != q.parent.UUID) {
			return retainedMissing()
		}
		if _, _, err := q.server.ResolveResource(ctx, q.x, q.authority, got, q.endpoint.Operation, RetainedHousekeeping); err != nil {
			return err
		}
		q.parent = got
		q.ids[name] = got.UpstreamID
	}
	if !q.addressesBatchFile() {
		return nil
	}
	store, err := q.server.Deps.Resources.GetByUUID(ctx, *q.parent.ParentID, q.x.keyID)
	if err != nil || store.Kind != "vector_store" {
		return retainedMissing()
	}
	q.parent = store
	return nil
}

func (q *retainedRequest) addressesBatchFile() bool {
	e := q.endpoint
	return e.Kind == "vector_store_file" && len(e.Ancestors) == 2 && e.Ancestors[1] == "vector_store_batch"
}

func (q *retainedRequest) metersWork() bool {
	e := q.endpoint
	return e.Metered && (e.Action == "create" || e.Action == "update" || e.Action == "patch")
}

func (q *retainedRequest) upstreamURL(ctx context.Context) (string, *fault.Error) {
	query, err := q.server.retainedQuery(ctx, q.x, q.endpoint, q.parent, q.r.URL.Query())
	if err != nil {
		return "", err
	}
	endpoint, urlErr := q.pin.Provider.Connector().ResourceURL(q.pin.Model, retainedUpstreamPath(q.endpoint, q.ids), query)
	if urlErr != nil {
		return "", fault.ServerError(502, "provider_protocol_error", "The retained endpoint is unavailable.")
	}
	return endpoint, nil
}
