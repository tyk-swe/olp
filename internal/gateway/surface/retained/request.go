package retained

import (
	"context"
	"net/http"
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

// retainedEnd is the only state a request writes as it proceeds. The deferred
// settlement reads it, so a step that panics still settles the pin it held.
type retainedEnd struct {
	pin    *kernel.Pin
	status int
}

type retainedReply struct {
	w       http.ResponseWriter
	x       *kernel.Call
	action  string
	surface string
	end     *retainedEnd
}

func (y retainedReply) fail(err *fault.Error) {
	refusal := err
	if y.action == "create" {
		final := *err
		final.NoRetry = true
		refusal = &final
	}
	y.x.Failure = refusal
	y.end.status = refusal.Status
	fault.WriteSurfaceError(y.w, refusal, y.surface)
}

type retainedInput struct {
	body        []byte
	source      oif.Document
	contentType string
}

type retainedRequest struct {
	server    surface
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	reply     retainedReply
	endpoint  retainedapi.Endpoint
	input     retainedInput
	parent    *resources.Resource
	ancestors map[string]string
}

func (s surface) retainedGeneric(w http.ResponseWriter, r *http.Request, e retainedapi.Endpoint) {
	x, authority, done := s.kernel.StateBegin(w, r, retainedSurfaceFormat(e.Surface))
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	x.BeginRetainedAction(e.Operation, e.Action)
	end := &retainedEnd{status: http.StatusInternalServerError}
	defer func() {
		s.kernel.Finish(x, nil, end.status)
		s.kernel.SettleRetained(r.Context(), x, end.pin)
	}()
	reply := retainedReply{w: w, x: x, action: e.Action, surface: e.Surface, end: end}
	if !authority.Policy.AllowProviderState {
		reply.fail(kernel.ProviderStateForbidden())
		return
	}
	if e.Action == "list" && len(e.Ancestors) == 0 {
		end.status = s.listRetainedGeneric(w, r, x, authority, e, reply.fail)
		return
	}
	q, err := s.readRetainedRequest(r, x, authority, e, reply)
	if err != nil {
		reply.fail(err)
		return
	}
	if e.Action == "create" {
		q.create()
		return
	}
	q.change()
}

func retainedMissing() *fault.Error {
	return fault.NotFoundError("not_found", "No resource with this identifier exists for this key.")
}

func retainedReferenceUnavailable() *fault.Error {
	return fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil)
}

func (s surface) readRetainedRequest(r *http.Request, x *kernel.Call, authority keyauthority.Authority, declared retainedapi.Endpoint, reply retainedReply) (retainedRequest, *fault.Error) {
	body, source, contentType, err := s.readRetainedBody(r, declared)
	if err != nil {
		return retainedRequest{}, err
	}
	displayOnly := changesOnlyDisplayFields(declared, source)
	if displayOnly {
		x.MarkRetainedDisplayChange()
	}
	e := meteredUnless(declared, displayOnly)
	parent, ancestors, err := s.retainedAncestry(r, x, authority, e)
	if err != nil {
		return retainedRequest{}, err
	}
	return retainedRequest{
		server: s, r: r, x: x, authority: authority, reply: reply, endpoint: e,
		input: retainedInput{body: body, source: source, contentType: contentType}, parent: parent, ancestors: ancestors,
	}, nil
}

func changesOnlyDisplayFields(e retainedapi.Endpoint, source oif.Document) bool {
	if e.Kind != "vector_store" || e.Action != "update" {
		return false
	}
	for _, field := range source.Root().Members() {
		if field.Name != "name" && field.Name != "metadata" {
			return false
		}
	}
	return true
}

func meteredUnless(e retainedapi.Endpoint, free bool) retainedapi.Endpoint {
	if !free {
		return e
	}
	unmetered := e
	unmetered.Metered = false
	return unmetered
}

func (s surface) retainedAncestry(r *http.Request, x *kernel.Call, authority keyauthority.Authority, e retainedapi.Endpoint) (*resources.Resource, map[string]string, *fault.Error) {
	ctx := r.Context()
	var parent *resources.Resource
	ids := map[string]string{}
	for i, kind := range e.Ancestors {
		name := "parent" + strconv.Itoa(i)
		got, err := s.store.Get(ctx, kind, x.KeyID(), r.PathValue(name))
		if err != nil {
			return nil, nil, retainedMissing()
		}
		if parent != nil && (got.ParentID == nil || *got.ParentID != parent.UUID) {
			return nil, nil, retainedMissing()
		}
		if _, _, err := s.kernel.ResolveResource(ctx, x, authority, got, e.Operation, kernel.RetainedHousekeeping); err != nil {
			return nil, nil, err
		}
		parent = got
		ids[name] = got.UpstreamID
	}
	if !addressesBatchFile(e) {
		return parent, ids, nil
	}
	store, err := s.store.GetByUUID(ctx, *parent.ParentID, x.KeyID())
	if err != nil || store.Kind != "vector_store" {
		return nil, nil, retainedMissing()
	}
	return store, ids, nil
}

func addressesBatchFile(e retainedapi.Endpoint) bool {
	return e.Kind == "vector_store_file" && len(e.Ancestors) == 2 && e.Ancestors[1] == "vector_store_batch"
}

func (q retainedRequest) metersWork() bool {
	e := q.endpoint
	return e.Metered && (e.Action == "create" || e.Action == "update" || e.Action == "patch")
}

func (q retainedRequest) upstreamURL(ctx context.Context, pin *kernel.Pin, ids map[string]string) (string, *fault.Error) {
	query, err := q.server.retainedQuery(ctx, q.x, q.endpoint, q.parent, q.r.URL.Query())
	if err != nil {
		return "", err
	}
	endpoint, urlErr := pin.Provider.Connector().ResourceURL(pin.Model, retainedUpstreamPath(q.endpoint, ids), query)
	if urlErr != nil {
		return "", fault.ServerError(502, "provider_protocol_error", "The retained endpoint is unavailable.")
	}
	return endpoint, nil
}
