package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

// RM5 generic retained families share the RM5.9 lifecycle contracts:
// owner-scoped gateway IDs, pinned route/provider/credential affinity,
// submission fences for creates, and no local content retention. Each
// family keeps its native wire shape; only identity fields are rewritten.

func retainedSurfaceFormat(surface string) wire.Format {
	switch surface {
	case "anthropic":
		return wire.AnthropicFile
	case "gemini", "native":
		return wire.GeminiFile
	}
	return wire.File
}

func (s *Server) registerRetainedGeneric(mux *http.ServeMux) {
	for _, endpoint := range retainedapi.Endpoints() {
		if endpoint.Operation == "agent_resource" {
			continue
		}
		e := endpoint
		mux.HandleFunc(e.Method+" "+e.Pattern, func(w http.ResponseWriter, r *http.Request) { s.retainedGeneric(w, r, e) })
	}
	mux.HandleFunc("POST /v1/vector_stores/{id}/search", s.retainedSearch)
	mux.HandleFunc("POST /v1/responses/compact", s.retainedCompact)
}

// upstreamPath maps a gateway pattern to the provider-relative path by
// substituting upstream IDs and stripping the gateway surface prefix.
func retainedUpstreamPath(e retainedapi.Endpoint, ids map[string]string) string {
	path := e.Pattern
	for name, id := range ids {
		if e.Surface == "gemini" {
			id = strings.TrimPrefix(id, "cachedContents/")
		}
		if e.Surface == "native" {
			if i := strings.LastIndex(id, "/tuningJobs/"); i >= 0 {
				id = id[i+len("/tuningJobs/"):]
			}
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(id))
	}
	switch e.Surface {
	case "anthropic":
		path = strings.TrimPrefix(path, "/anthropic/v1/")
	case "gemini":
		path = strings.TrimPrefix(path, "/gemini/v1beta/")
	case "native":
		path = strings.TrimPrefix(path, "/native/vertex-tuning/")
		path = "tuningJobs" + strings.TrimPrefix(path, "jobs")
		if e.Action == "cancel" {
			path = strings.TrimSuffix(path, "/cancel") + ":cancel"
		}
	default:
		path = strings.TrimPrefix(path, "/v1/")
	}
	return path
}

func (s *Server) retainedGeneric(w http.ResponseWriter, r *http.Request, e retainedapi.Endpoint) {
	format := retainedSurfaceFormat(e.Surface)
	x, authority, done := s.stateBegin(w, r, format)
	if done {
		return
	}
	defer s.release(r.Context())
	x.operationOverride = e.Operation
	x.mode = "unary"
	x.actor = "api_key_resource"
	x.retainedHousekeeping = slices.Contains([]string{"list", "get", "delete", "cancel", "content"}, e.Action)
	var p *pin
	status := http.StatusInternalServerError
	defer func() { s.finish(x, nil, status); s.retainedSettle(r.Context(), x, p) }()
	fail := func(err *Error) {
		if e.Action == "create" {
			c := *err
			c.NoRetry = true
			err = &c
		}
		x.failure = err
		status = err.Status
		writeSurfaceError(w, err, e.Surface)
	}
	if !authority.Policy.AllowProviderState {
		fail(providerStateForbidden())
		return
	}
	if e.Action == "list" && len(e.Ancestors) == 0 {
		status = s.listRetainedGeneric(w, r, x, authority, e, fail)
		return
	}
	body, source, contentType, bodyErr := s.readRetainedBody(r, e)
	if bodyErr != nil {
		fail(bodyErr)
		return
	}

	// Changing only a store's display metadata cannot extend paid storage.
	if e.Kind == "vector_store" && e.Action == "update" {
		displayOnly := true
		for _, field := range source.Root().Members() {
			if field.Name != "name" && field.Name != "metadata" {
				displayOnly = false
			}
		}
		if displayOnly {
			e.Metered = false
			x.retainedHousekeeping = true
		}
	}
	// Resolve ancestors (parents) in path order; foreign parents are missing.
	var parent *resources.Resource
	ids := map[string]string{}
	for i, kind := range e.Ancestors {
		got, getErr := s.Resources.Get(r.Context(), kind, x.keyID, r.PathValue("parent"+strconv.Itoa(i)))
		if getErr != nil {
			fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
		if parent != nil && (got.ParentID == nil || *got.ParentID != parent.UUID) {
			fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
		if _, _, parentErr := s.resolveResource(r.Context(), x, authority, got, e.Operation, retainedHousekeeping); parentErr != nil {
			fail(parentErr)
			return
		}
		parent = got
		ids["parent"+strconv.Itoa(i)] = got.UpstreamID
	}
	if e.Kind == "vector_store_file" && len(e.Ancestors) == 2 && e.Ancestors[1] == "vector_store_batch" {
		// A batch filters the store's attachment collection. Its file IDs
		// continue to belong to the store, so the same attachment remains
		// addressable through /vector_stores/{store}/files/{file}.
		var parentErr error
		parent, parentErr = s.Resources.GetByUUID(r.Context(), *parent.ParentID, x.keyID)
		if parentErr != nil || parent.Kind != "vector_store" {
			fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
	}

	var route *routing.Route
	var resource *resources.Resource
	var err *Error

	if e.Action == "create" {
		slug := strings.TrimSpace(r.Header.Get(stateRouteHeader))
		var selected *resources.Resource
		if parent != nil {
			slug = parent.RouteSlug
			selected = parent
		}
		if slug == "" {
			fail(invalidRequest("missing_required_parameter", "X-OLP-Route is required.", nil))
			return
		}
		idempotency := r.Header.Get("Idempotency-Key")
		if idempotency == "" || len(idempotency) > 128 {
			fail(invalidRequest("missing_required_parameter", "Idempotency-Key is required for resource creation.", nil))
			return
		}
		scope := slug
		if parent != nil {
			scope += "\x00" + parent.ID
		}
		digest, derr := s.retainedBodyDigest(e, scope, body, contentType)
		if derr != nil {
			fail(derr)
			return
		}
		prior, lookupErr := s.Resources.LookupLifecycle(r.Context(), x.keyID, e.Operation, idempotency, digest)
		if errors.Is(lookupErr, resources.ErrSubmissionConflict) {
			fail(serverError(409, "idempotency_conflict", "This submission identity is already bound to another request."))
			return
		}
		if lookupErr == nil {
			x.route = &routing.Route{Slug: prior.RouteSlug, RevisionID: prior.RouteRevisionID}
			w.Header().Set("X-OLP-Resource-ID", prior.ID)
			fail(serverError(409, "submission_exists", "This submission already exists; inspect X-OLP-Resource-ID instead of recreating it."))
			return
		}
		if !errors.Is(lookupErr, resources.ErrNotFound) {
			fail(serverError(503, "provider_state_unavailable", "The submission could not be read."))
			return
		}
		candidate, ok := x.snapshot().Routes[slug]
		if !ok {
			fail(modelNotFound(slug))
			return
		}
		route = &candidate
		x.route = route
		if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
			fail(permissionError("route_forbidden", "The API key cannot use this route."))
			return
		}
		if !slices.Contains(route.Operations, e.Operation) {
			fail(policyUnavailable("operation_unavailable", "The route does not allow this operation."))
			return
		}
		if err = x.guardrailSurfaceGate(route); err != nil {
			fail(err)
			return
		}
		// Resources referenced by the creation body own the pin that
		// must serve it; resolve them before unconstrained selection.
		bound := selected
		if bound == nil && source.Root().Kind() == oif.Object {
			referenced, refErr := s.referencedResources(r.Context(), x, source, e)
			if refErr != nil {
				fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
				return
			}
			for _, candidate := range referenced {
				if bound == nil {
					bound = candidate
					continue
				}
				if !samePin(candidate, bound) {
					fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
					return
				}
			}
		}
		p, err = s.selectBoundPinSurface(r.Context(), x, route, e.Operation, e.Surface, "unary", bound, func(provider *routing.Provider, model string) bool {
			return provider.CredentialSource != "caller" && provider.Connector().Supports(e.Operation, e.Surface, "unary") && provider.Supports(model, e.Operation, e.Surface, "unary")
		})
		if err != nil {
			fail(err)
			return
		}
		if source.Root().Kind() == oif.Object {
			rewritten, rwErr := s.rewriteRetainedRequest(r.Context(), x, authority, p, route, source, e)
			if rwErr != nil {
				fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
				return
			}
			var encodeErr error
			body, contentType, encodeErr = encodeRetainedRequestBody(e, rewritten, contentType)
			if encodeErr != nil {
				fail(invalidRequest("invalid_request", "The resource upload could not be encoded.", nil))
				return
			}
		}

		ctx, cancel := s.stateDeadline(r.Context(), route)
		defer cancel()
		if err := s.admitRetainedRequest(ctx, x, authority, p, route, e.Metered && (e.Action == "create" || e.Action == "update" || e.Action == "patch")); err != nil {
			fail(err)
			return
		}
		query, queryErr := s.retainedQuery(ctx, x, e, parent, r.URL.Query())
		if queryErr != nil {
			fail(queryErr)
			return
		}
		endpoint, urlErr := p.provider.Connector().ResourceURL(p.model, retainedUpstreamPath(e, ids), query)
		if urlErr != nil {
			fail(serverError(502, "provider_protocol_error", "The retained endpoint is unavailable."))
			return
		}
		fields, inputErr := retainedInputMetadata(p, route, x.keyID, e, source, parent)
		if inputErr != nil {
			fail(invalidRequest("unsupported_parameter", inputErr.Error(), nil))
			return
		}
		meta, _ := json.Marshal(fields)
		version := retainedapi.Revision
		candidateRes := &resources.Resource{Kind: e.Kind, APIKeyID: x.keyID, RouteSlug: route.Slug, ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: route.RevisionID, TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID, Metadata: meta, ContractVersion: &version}
		if parent != nil {
			// A child shares its parent's serving revision so listing,
			// projection, and cursor checks keep one identity even
			// after the route is republished.
			candidateRes.ParentID = &parent.UUID
			candidateRes.RouteRevisionID = parent.RouteRevisionID
		}
		var fresh bool
		var prepareErr error
		if err := s.prepareRetainedAccounting(x, p, route, candidateRes, e, source); err != nil {
			fail(err)
			return
		}
		resource, fresh, prepareErr = s.Resources.PrepareLifecycle(ctx, candidateRes, e.Operation, idempotency, digest)
		if errors.Is(prepareErr, resources.ErrSubmissionConflict) {
			fail(serverError(409, "idempotency_conflict", "This submission identity is already bound to another request."))
			return
		}
		if prepareErr != nil {
			fail(serverError(503, "provider_state_unavailable", "The submission could not be persisted."))
			return
		}
		w.Header().Set("X-OLP-Resource-ID", resource.ID)
		if !fresh {
			fail(serverError(409, "submission_exists", "This submission already exists; inspect X-OLP-Resource-ID instead of recreating it."))
			return
		}
		body, prepareErr = retainedCorrelationBody(e, resource, body)
		if prepareErr != nil {
			fail(invalidRequest("invalid_request", "The resource correlation metadata could not be bound.", nil))
			return
		}
		if prepareErr = s.Resources.DispatchLifecycle(ctx, resource.UUID); prepareErr != nil {
			fail(serverError(503, "provider_state_unavailable", "The submission could not be fenced."))
			return
		}
		defer func() {
			commitCtx, cancel := resourceCommitContext(r.Context())
			defer cancel()
			_ = s.Resources.UnknownLifecycle(commitCtx, resource.UUID)
		}()
		status = s.retainedDispatch(w, r.WithContext(ctx), x, p, e, resource, endpoint, body, contentType, true, fail)
		return
	}

	// Non-create: resolve the addressed resource (child-aware).
	if e.Action == "list" { // child list: proxy the parent's collection
		if parent == nil {
			fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
		resource = parent
	} else {
		var getErr error
		resource, getErr = s.Resources.Get(r.Context(), e.Kind, x.keyID, r.PathValue("id"))
		if getErr != nil {
			fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
		if parent != nil && (resource.ParentID == nil || *resource.ParentID != parent.UUID) {
			fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
	}
	if resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
		fail(pinUnavailable())
		return
	}
	use := retainedRetrieval
	if e.Action == "delete" || e.Action == "cancel" || e.Action == "list" {
		use = retainedHousekeeping
	}
	p, route, err = s.resolveResource(r.Context(), x, authority, resource, e.Operation, use)
	if err != nil {
		fail(err)
		return
	}
	x.route = route
	if !p.provider.Connector().Supports(e.Operation, e.Surface, "unary") {
		fail(pinUnavailable())
		return
	}
	if e.Action != "delete" {
		if err = x.guardrailSurfaceGate(route); err != nil {
			fail(err)
			return
		}
	}
	if resource.State == "submitting" || resource.State == "submission_unknown" || resource.State == "not_accepted" {
		fail(serverError(409, "submission_unknown", "Creation has not been reconciled; this request cannot be resubmitted."))
		return
	}
	if source.Root().Kind() == oif.Object {
		inputParent := parent
		if e.Kind == "eval" && e.Action == "update" {
			inputParent = resource
		}
		fields, inputErr := retainedInputMetadata(p, route, x.keyID, e, source, inputParent)
		if inputErr != nil {
			fail(invalidRequest("unsupported_parameter", inputErr.Error(), nil))
			return
		}
		if e.Operation == "eval" && e.Action == "update" {
			var metadata map[string]json.RawMessage
			_ = json.Unmarshal(resource.Metadata, &metadata)
			metadata["authorized_models"], _ = json.Marshal(fields["authorized_models"])
			resource.Metadata, _ = json.Marshal(metadata)
		}
		rewritten, rwErr := s.rewriteRetainedRequest(r.Context(), x, authority, p, route, source, e)
		if rwErr != nil {
			fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
			return
		}
		body = rewritten.Bytes()
		body, rwErr = retainedCorrelationBody(e, resource, body)
		if rwErr != nil {
			fail(invalidRequest("invalid_request", "The resource correlation metadata could not be bound.", nil))
			return
		}
	}
	ids["id"] = resource.UpstreamID
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	if err := s.admitRetainedRequest(ctx, x, authority, p, route, e.Metered && (e.Action == "create" || e.Action == "update" || e.Action == "patch")); err != nil {
		fail(err)
		return
	}
	query, queryErr := s.retainedQuery(ctx, x, e, parent, r.URL.Query())
	if queryErr != nil {
		fail(queryErr)
		return
	}
	endpoint, urlErr := p.provider.Connector().ResourceURL(p.model, retainedUpstreamPath(e, ids), query)
	if urlErr != nil {
		fail(serverError(502, "provider_protocol_error", "The retained endpoint is unavailable."))
		return
	}
	mutation, fenceErr := s.fenceRetainedMutation(ctx, r, x, e, resource, body)
	if fenceErr != nil {
		fail(fenceErr)
		return
	}
	defer mutation()
	status = s.retainedDispatch(w, r.WithContext(ctx), x, p, e, resource, endpoint, body, contentType, false, fail)
}

// retainedDispatch performs the pinned call and projects identities.
func (s *Server) retainedDispatch(w http.ResponseWriter, r *http.Request, x *call, p *pin, e retainedapi.Endpoint, resource *resources.Resource, endpoint string, body []byte, contentType string, isCreate bool, fail func(*Error)) int {
	native, failure := s.pinnedDo(r.Context(), x, p, e.Method, endpoint, body, contentType)
	if failure != nil {
		if resource != nil && (isCreate || e.Metered && (e.Action == "update" || e.Action == "patch")) && retainedRejectedWithoutWork(failure) {
			commit, cancel := resourceCommitContext(r.Context())
			rejectErr := s.Resources.RejectRetained(commit, resource, r.Header.Get("Idempotency-Key"), e.Operation+":"+e.Action, isCreate)
			cancel()
			if rejectErr != nil {
				fail(serverError(503, "provider_state_unavailable", "Provider rejection could not be committed."))
				return 503
			}
		}
		if resource != nil && !isCreate && failure.status == http.StatusNotFound {
			commitCtx, cancel := resourceCommitContext(r.Context())
			defer cancel()
			if removeErr := s.Resources.Tombstone(commitCtx, resource.ID); removeErr == nil {
				fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
				return http.StatusNotFound
			}
		}
		public := retainedPublicError(upstreamError(failure), failure.status)
		fail(public)
		return public.Status
	}
	defer native.Body.Close()
	response, readErr := readBounded(native.Body, s.retainedResponseLimit())
	if readErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		fail(serverError(502, "provider_protocol_error", "The provider returned an unsupported resource response."))
		return 502
	}
	commitCtx, commitCancel := resourceCommitContext(r.Context())
	defer commitCancel()
	if e.Action == "delete" {
		if putErr := s.Resources.Tombstone(commitCtx, resource.ID); putErr != nil {
			fail(serverError(503, "provider_state_unavailable", "Deletion was accepted but its local record could not be updated."))
			return 503
		}
		if native.StatusCode == http.StatusNoContent {
			deleted, _ := oif.ParseJSON([]byte(`{}`), oif.Limits{})
			if err := s.observeRetainedSettlement(commitCtx, resource, e, deleted); err != nil {
				fail(serverError(503, "provider_state_unavailable", "Deletion evidence could not be committed."))
				return 503
			}
			_ = observeRetainedUsage(x, oif.Document{}, false)
			x.completePinned()
			s.writeStateJSONStatus(w, x, nil, native.StatusCode)
			return native.StatusCode
		}
	}
	if e.Action == "content" {
		_ = observeRetainedUsage(x, oif.Document{}, false)
		x.completePinned()
		// Binary delivery finalizes like the JSON path: a successful
		// write commits the envelope, a failed one cancels it.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
		w.Header().Set("Content-Type", "application/octet-stream")
		x.responseHeaders(w.Header(), false)
		w.WriteHeader(native.StatusCode)
		n, writeErr := w.Write(response)
		if writeErr == nil && n == len(response) {
			writeErr = http.NewResponseController(w).Flush()
		}
		if writeErr != nil || n != len(response) {
			x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The resource result could not be delivered.")
			if len(x.facts) > 0 {
				x.facts[len(x.facts)-1].Class = classCancelled
			}
			s.finish(x, &outcome{err: x.failure, committed: true, cancelled: true}, 0)
			return native.StatusCode
		}
		if n > 0 {
			x.delivered(s.now())
		}
		s.finish(x, &outcome{committed: true}, native.StatusCode)
		return native.StatusCode
	}
	parsed, parseErr := oif.ParseJSON(response, oif.Limits{MaxBytes: retainedBodyLimit})
	if parseErr != nil || parsed.Root().Kind() != oif.Object {
		fail(serverError(502, "provider_protocol_error", "The provider returned an invalid resource object."))
		return 502
	}
	if !isCreate && e.Action != "list" && e.Action != "delete" && !(e.Action == "cancel" && e.Surface == "native") {
		identity, _ := parsed.Root().Lookup(e.Identity)
		if id, ok := identity.Text(); !ok || id != resource.UpstreamID {
			fail(serverError(502, "provider_protocol_error", "The provider returned a different resource identity."))
			return 502
		}
	}
	if isCreate {
		ids, idErr := retainedCreateIDs(e, response)
		if idErr != nil {
			fail(serverError(502, "provider_protocol_error", "The provider response omitted a valid resource identity."))
			return 502
		}
		resource.State = "active"
		var children []*resources.Resource
		if len(ids) == 0 {
			resource.UpstreamID, resource.State = "empty:"+resource.UUID.String(), resources.StateDeleted
		} else {
			resource.UpstreamID = ids[0]
			for _, id := range ids[1:] {
				child := *resource
				child.UUID, child.ID, child.UpstreamID = [16]byte{}, "", id
				child.SubmissionID = nil
				children = append(children, &child)
			}
		}
		if err := resources.EnrichRetained(resource, e, parsed, s.now()); err != nil {
			fail(serverError(502, "provider_protocol_error", "The provider returned invalid resource billing metadata."))
			return 502
		}
		if putErr := s.Resources.CompleteLifecycleChildren(commitCtx, resource, children); putErr != nil {
			fail(serverError(503, "provider_state_unavailable", "The provider accepted creation but its resource identities could not be committed."))
			return 503
		}
	}
	if err := s.retainedNativeResult(commitCtx, resource, e, parsed, true); err != nil {
		fail(serverError(503, "provider_state_unavailable", "Resource billing metadata could not be committed."))
		return 503
	}
	projected, projectErr := s.projectRetainedGeneric(r.Context(), resource, e, response)
	if projectErr != nil {
		fail(serverError(502, "provider_protocol_error", "The provider response contained an unbound resource identity."))
		return 502
	}
	if err := s.observeRetainedSettlement(commitCtx, resource, e, parsed); err != nil {
		fail(serverError(503, "provider_state_unavailable", "Resource usage evidence could not be committed."))
		return 503
	}
	if e.Metered && (e.Action == "update" || e.Action == "patch") {
		if err := s.Resources.AcceptRetainedMutation(commitCtx, resource.UUID, r.Header.Get("Idempotency-Key"), e.Operation+":"+e.Action); err != nil {
			fail(serverError(503, "provider_state_unavailable", "The accepted mutation could not be committed."))
			return 503
		}
	}
	_ = observeRetainedUsage(x, parsed, false)
	x.completePinned()
	s.writeStateJSONStatus(w, x, projected, native.StatusCode)
	return native.StatusCode
}

func (s *Server) listRetainedGeneric(w http.ResponseWriter, r *http.Request, x *call, a access.Authority, e retainedapi.Endpoint, fail func(*Error)) int {
	pageSizeParam, cursorParam := "limit", "after"
	if e.Surface == "gemini" || e.Surface == "native" {
		pageSizeParam, cursorParam = "pageSize", "pageToken"
	}
	if e.Surface == "anthropic" {
		cursorParam = "after_id"
		if e.Operation == "skill" {
			// Skills paginate with the page/next_page token pair.
			cursorParam = "page"
		}
	}
	for name, values := range r.URL.Query() {
		if name == "beta" && e.Operation == "skill" && len(values) == 1 && values[0] == "true" {
			continue
		}
		if (name != pageSizeParam && name != cursorParam && !(name == "order" && e.Surface == "openai")) || len(values) != 1 {
			fail(invalidRequest("invalid_parameter", "Unsupported or repeated list parameter.", &name))
			return 400
		}
	}
	order := r.URL.Query().Get("order")
	if order != "" && order != "asc" && order != "desc" {
		fail(invalidRequest("invalid_parameter", "order must be asc or desc.", nil))
		return 400
	}
	limit := 20
	if raw := r.URL.Query().Get(pageSizeParam); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			fail(invalidRequest("invalid_parameter", "Page size must be between 1 and 100.", nil))
			return 400
		}
		limit = parsed
	}
	ready := make([]retainedListEntry, 0, limit+1)
	cursor := r.URL.Query().Get(cursorParam)
	exhausted := false
	var pending []*resources.Resource
	pendingLast := false
	var overall time.Duration
	admitted := make(map[string]bool)
	routeCostsAdmitted := make(map[string]bool)
	// fill scans authorized bindings until ready holds more than
	// target entries or the owner's rows are exhausted. Each new
	// candidate is policy-checked as it is collected so refilled
	// candidates carry the same prices and guardrails. A fetched
	// batch is marked last only once all of its rows are consumed.
	fill := func(ctx context.Context, target int) int {
		for len(ready) <= target && !exhausted {
			if len(pending) == 0 {
				rows, err := s.Resources.ListLifecycleOrdered(ctx, e.Kind, x.keyID, 101, cursor, order == "asc")
				if err != nil {
					if errors.Is(err, resources.ErrNotFound) {
						fail(invalidRequest("invalid_parameter", "The page token is unavailable for this owner.", nil))
						return 400
					}
					fail(serverError(503, "provider_state_unavailable", "Provider state is unavailable."))
					return 503
				}
				pending, pendingLast = rows, len(rows) < 101
				if len(pending) == 0 {
					exhausted = true
					break
				}
			}
			consumed := 0
			for _, resource := range pending {
				consumed++
				cursor = resource.ID
				if resource.State == "submitting" || resource.State == "submission_unknown" || resource.State == "not_accepted" || resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
					continue
				}
				p, route, resolveErr := s.resolveResource(ctx, x, a, resource, e.Operation, retainedRetrieval)
				if resolveErr != nil {
					if resolveErr.Status != http.StatusNotFound && resolveErr.Status != http.StatusConflict {
						fail(resolveErr)
						return resolveErr.Status
					}
					continue
				}
				x.route = route
				if gateErr := x.guardrailSurfaceGate(route); gateErr != nil {
					fail(gateErr)
					return gateErr.Status
				}
				if policyErr := s.retainedAccountingPolicy(x, p, false); policyErr != nil {
					fail(policyErr)
					return policyErr.Status
				}
				entry := retainedListEntry{resource: resource, pin: p, route: route, freeControl: x.retainedFreeControl}
				timeout := time.Duration(route.OverallTimeout) * time.Millisecond
				if len(ready) == 0 || timeout < overall {
					overall = timeout
				}
				ready = append(ready, entry)
				if len(ready) > target {
					break
				}
			}
			pending = pending[consumed:]
			if len(pending) == 0 && pendingLast {
				exhausted = true
			}
		}
		return 0
	}
	if status := fill(r.Context(), limit); status != 0 {
		return status
	}
	items := make([]json.RawMessage, 0, limit)
	ctx := r.Context()
	x.retainedFreeControl = retainedPageFreeControl(ready, limit)
	callerCostsAdmitted := !x.retainedFreeControl
	costChecked := false
	// Route leases stay separate from the caller's page lease: each
	// route's cost budget is grown and settled with only its own
	// reads, never the whole page total.
	routeLeases := make(map[string]*limits.Lease)
	routeReserved := make(map[string]usage.Cost)
	routeReservedCharge := make(map[string]usage.Cost)
	routeActual := make(map[string]usage.Cost)
	routeActualCharge := make(map[string]usage.Cost)
	routeDispatched := make(map[string]bool)
	defer func() {
		zero := int64(0)
		for slug, lease := range routeLeases {
			if actual, ok := routeActual[slug]; ok {
				lease.SetActualCost(actual.String(), routeActualCharge[slug].String())
			}
			settleKey(ctx, lease, routeDispatched[slug], &zero, s.log)
		}
	}()
	admitRoute := func(entry retainedListEntry) int {
		if admitted[entry.route.Slug] && (entry.freeControl || routeCostsAdmitted[entry.route.Slug]) {
			return 0
		}
		authority := x.admissionAuthority(a)
		if admitted[entry.route.Slug] {
			// Earlier free reads reserved only this route's rates.
			authority = retainedCostAuthority(authority)
		}
		lease, gateErr := s.Admission.reserveRouteLimits(ctx, &authority, entry.route.Slug, resourceEstimate, overall, costReservation{}, &costChecked)
		if gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
		if lease != nil {
			if prior := routeLeases[entry.route.Slug]; prior != nil {
				prior.Attach(lease)
			} else {
				routeLeases[entry.route.Slug] = lease
			}
		}
		admitted[entry.route.Slug] = true
		if !entry.freeControl {
			routeCostsAdmitted[entry.route.Slug] = true
		}
		return 0
	}
	if len(ready) > 0 {
		// One deadline covers the page and every lease held for its reads.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overall)
		defer cancel()
		// Clearing the collection route keeps reserveState to per-key
		// limits so the per-route pass reserves each route once.
		x.route = nil
		if gateErr := s.reserveState(ctx, x, a, overall); gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
	}
	responseBytes := 256
	readIdx := 0
	var lastItem *resources.Resource
	// Reads refill after tombstones so a page never returns empty
	// with a continuation while live bindings remain.
	for {
		if len(items) >= limit || readIdx >= len(ready) && exhausted {
			break
		}
		if readIdx >= len(ready) {
			if status := fill(ctx, len(ready)); status != 0 {
				return status
			}
			if readIdx >= len(ready) {
				break
			}
			continue
		}
		entry := ready[readIdx]
		readIdx++
		x.retainedFreeControl = entry.freeControl
		x.route = entry.route
		if !entry.freeControl && !callerCostsAdmitted {
			if failure := s.admitRetainedPageCost(ctx, x, a, overall); failure != nil {
				fail(failure)
				return failure.Status
			}
			callerCostsAdmitted = true
		}
		if status := admitRoute(entry); status != 0 {
			return status
		}
		resource, p, route := entry.resource, entry.pin, entry.route
		x.route = route
		bound := x.attemptCostBound(p.attempt)
		charge := x.attemptChargeBound(p.attempt, bound)
		if costErr := s.reserveRetainedCost(ctx, x, p, route); costErr != nil {
			fail(costErr)
			return costErr.Status
		}
		if lease := routeLeases[route.Slug]; lease != nil && !(bound.IsZero() && charge.IsZero()) {
			total := routeReserved[route.Slug].Add(bound)
			totalCharge := routeReservedCharge[route.Slug].Add(charge)
			if costErr := s.growCost(ctx, lease, total.String(), totalCharge.String(), x.request.accountingID(), time.Duration(route.OverallTimeout)*time.Millisecond); costErr != nil {
				fail(costErr)
				return costErr.Status
			}
			routeReserved[route.Slug] = total
			routeReservedCharge[route.Slug] = totalCharge
		}
		if gateErr := s.gateRetainedSlot(ctx, x, p, route); gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
		endpoint, urlErr := p.provider.Connector().ResourceURL(p.model, retainedUpstreamPath(retainedapi.Endpoint{Family: e.Family, Action: "get", Method: "GET", Pattern: e.Path + "/{id}"}, map[string]string{"id": resource.UpstreamID}), nil)
		if urlErr != nil {
			s.settlePinHold(ctx, x, p.hold, nil)
			fail(pinUnavailable())
			return 409
		}
		native, callErr := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if callErr != nil {
			s.settlePinHold(ctx, x, p.hold, nil)
			if callErr.status == http.StatusNotFound {
				commitCtx, commitCancel := resourceCommitContext(ctx)
				removeErr := s.Resources.Tombstone(commitCtx, resource.ID)
				commitCancel()
				if removeErr != nil {
					fail(serverError(503, "provider_state_unavailable", "The deleted resource could not be reconciled."))
					return 503
				}
				continue
			}
			public := retainedPublicError(upstreamError(callErr), callErr.status)
			fail(public)
			return public.Status
		}
		routeDispatched[route.Slug] = true
		routeActual[route.Slug] = routeActual[route.Slug].Add(bound)
		routeActualCharge[route.Slug] = routeActualCharge[route.Slug].Add(charge)
		body, readErr := readBounded(native.Body, s.retainedResponseLimit())
		native.Body.Close()
		if readErr == nil {
			objectEndpoint := e
			objectEndpoint.Action = "get"
			ids, idErr := retainedCreateIDs(objectEndpoint, body)
			if idErr != nil || len(ids) != 1 || ids[0] != resource.UpstreamID {
				readErr = resources.ErrContract
			}
		}
		var projected []byte
		if readErr == nil {
			projected, readErr = s.projectRetainedGeneric(ctx, resource, retainedapi.Endpoint{Family: e.Family, Action: "get"}, body)
		}
		if readErr == nil {
			source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: retainedBodyLimit})
			if parseErr == nil {
				objectEndpoint := retainedapi.Endpoint{Family: e.Family, Action: "get"}
				commitCtx, commitCancel := resourceCommitContext(ctx)
				persistErr := s.retainedNativeResult(commitCtx, resource, objectEndpoint, source, true)
				if persistErr == nil {
					persistErr = s.observeRetainedSettlement(commitCtx, resource, objectEndpoint, source)
				}
				commitCancel()
				if persistErr != nil {
					fail(serverError(503, "provider_state_unavailable", "Resource billing metadata could not be committed."))
					return 503
				}
				_ = observeRetainedUsage(x, source, false)
			}
			x.completePinned()
			x.finalizePinned(nil, s.now())
		}
		zero := int64(0)
		s.settlePinHold(ctx, x, p.hold, &zero)
		if readErr != nil {
			fail(serverError(502, "provider_protocol_error", "The provider returned an invalid resource."))
			return 502
		}
		responseBytes += len(projected) + 1
		if responseBytes > retainedBodyLimit {
			fail(serverError(502, "provider_protocol_error", "The resource page exceeds the supported response size; request a smaller page."))
			return 502
		}
		items = append(items, projected)
		lastItem = resource
	}
	hasMore := readIdx < len(ready) || !exhausted
	response := map[string]any{e.Collection: items}
	if hasMore && lastItem != nil {
		if e.Surface == "gemini" || e.Surface == "native" {
			response["nextPageToken"] = lastItem.ID
		}
	}
	if e.Operation == "skill" {
		// The Skills auto-pager follows next_page, not has_more.
		if hasMore && lastItem != nil {
			response["next_page"] = lastItem.ID
		} else {
			response["next_page"] = nil
		}
	}
	if e.Surface == "openai" || e.Surface == "anthropic" {
		response["has_more"] = hasMore
		if e.Surface == "openai" {
			response["object"] = "list"
		}
		if len(items) > 0 {
			first, _ := oif.ParseJSON(items[0], oif.Limits{MaxBytes: retainedBodyLimit})
			last, _ := oif.ParseJSON(items[len(items)-1], oif.Limits{MaxBytes: retainedBodyLimit})
			f, _ := first.Root().Lookup(e.Identity)
			l, _ := last.Root().Lookup(e.Identity)
			response["first_id"], _ = f.Text()
			response["last_id"], _ = l.Text()
		}
	}
	body, encodeErr := encodeRetainedPage(response)
	if encodeErr != nil {
		fail(encodeErr)
		return encodeErr.Status
	}
	s.writeStateJSONStatus(w, x, body, http.StatusOK)
	return 200
}

// retainedSearch serves RM5.2 native search and RM5.3 query-only bindings
// through the one authorized retrieval path. It creates no resource.
func (s *Server) retainedSearch(w http.ResponseWriter, r *http.Request) {
	if id, err := uuid.Parse(r.PathValue("id")); err == nil {
		r.SetPathValue("binding_id", id.String())
		s.queryRetrieval(w, r)
		return
	}
	s.retainedProxy(w, r, "retrieval", "vector_stores/{id}/search", "vector_store")
}

// retainedCompact serves RM5.4 native compaction as its own priced call.
func (s *Server) retainedCompact(w http.ResponseWriter, r *http.Request) {
	s.retainedProxy(w, r, "generation", "responses/compact", "")
}

func (s *Server) retainedProxy(w http.ResponseWriter, r *http.Request, operation, upstreamTemplate, storeKind string) {
	x, authority, done := s.stateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.release(r.Context())
	x.operationOverride = operation
	x.mode = "unary"
	x.actor = "api_key_resource"
	x.retainedMetered = true
	var p *pin
	status := http.StatusInternalServerError
	defer func() { s.finish(x, nil, status); s.retainedSettle(r.Context(), x, p) }()
	fail := func(err *Error) {
		x.failure = err
		status = err.Status
		writeSurfaceError(w, err, "openai")
	}
	if !authority.Policy.AllowProviderState {
		fail(providerStateForbidden())
		return
	}
	body, err := s.readBody(r)
	if err != nil {
		fail(err)
		return
	}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.cfg.MaxBodyBytes)})
	if parseErr != nil || source.Root().Kind() != oif.Object {
		fail(invalidRequest("invalid_request", "A valid JSON object is required.", nil))
		return
	}
	codec := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "retrieval", Surface: "openai"}}
	if storeKind == "" {
		codec.Operation = "compaction"
	}
	if len(r.URL.Query()) != 0 {
		fail(invalidRequest("invalid_parameter", "This operation does not support query parameters.", nil))
		return
	}
	var route *routing.Route
	var resource *resources.Resource
	upstreamPath := upstreamTemplate
	if storeKind != "" {
		var getErr error
		resource, getErr = s.Resources.Get(r.Context(), storeKind, x.keyID, r.PathValue("id"))
		if getErr != nil {
			fail(notFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
		var resolveErr *Error
		// Searching a retained store starts new billable work, so it
		// must satisfy current new-work restrictions rather than the
		// historical retrieval contract that created the binding.
		p, route, resolveErr = s.resolveResource(r.Context(), x, authority, resource, operation, retainedNewWork)
		if resolveErr != nil {
			fail(resolveErr)
			return
		}
		upstreamPath = strings.Replace(upstreamTemplate, "{id}", url.PathEscape(resource.UpstreamID), 1)
	} else {
		slug := strings.TrimSpace(r.Header.Get(stateRouteHeader))
		if slug == "" {
			fail(invalidRequest("missing_required_parameter", "X-OLP-Route is required.", nil))
			return
		}
		candidate, ok := x.snapshot().Routes[slug]
		if !ok {
			fail(modelNotFound(slug))
			return
		}
		route = &candidate
		x.route = route
		if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
			fail(permissionError("route_forbidden", "The API key cannot use this route."))
			return
		}
		if !slices.Contains(route.Operations, operation) {
			fail(policyUnavailable("operation_unavailable", "The route does not allow this operation."))
			return
		}
		if gateErr := x.guardrailSurfaceGate(route); gateErr != nil {
			fail(gateErr)
			return
		}
		// Price the work before selection reserves the provider hold:
		// selecting first would admit the slot against the floored
		// minimum estimate regardless of the actual input size.
		x.estimate = int64(len(body))
		// Retained references own the pin that must serve this call;
		// resolve them first so selection is bound to their target
		// and credential slot instead of rejecting them on affinity.
		var candidates []*resources.Resource
		if value, present := source.Root().Lookup("previous_response_id"); present {
			if id, isText := value.Text(); isText && id != "" {
				resolved, _, readErr := s.readResponseResource(r.Context(), x.keyID, id)
				if readErr != nil {
					fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
					return
				}
				candidates = append(candidates, resolved)
			}
		}
		for _, fileID := range retainedFileIDs(source.Root()) {
			resolved, _, readErr := s.readDurable(r.Context(), durableKind(resources.KindFile, fileID), x.keyID, fileID)
			if readErr != nil {
				fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
				return
			}
			candidates = append(candidates, resolved)
		}
		var bound *resources.Resource
		for _, candidate := range candidates {
			if bound == nil {
				bound = candidate
				continue
			}
			if !samePin(candidate, bound) {
				fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
				return
			}
		}
		var pinErr *Error
		p, pinErr = s.selectBoundPinSurface(r.Context(), x, route, operation, "openai", "unary", bound, func(provider *routing.Provider, model string) bool {
			return provider.CredentialSource != "caller" && (provider.Kind == "openai" || provider.Kind == "azure_openai") && provider.Connector().Supports(operation, "openai", "unary")
		})
		if pinErr != nil {
			fail(pinErr)
			return
		}
	}
	x.route = route
	if gateErr := x.guardrailSurfaceGate(route); gateErr != nil {
		fail(gateErr)
		return
	}
	rewritten, rewriteErr := s.rewriteRetainedRequest(r.Context(), x, authority, p, route, source, codec)
	if rewriteErr != nil {
		fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
		return
	}
	body = rewritten.Bytes()
	x.estimate = int64(len(body))
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	if err := s.admitRetainedRequest(ctx, x, authority, p, route, false); err != nil {
		fail(err)
		return
	}
	endpoint, urlErr := p.provider.Connector().ResourceURL(p.model, upstreamPath, nil)
	if urlErr != nil {
		fail(serverError(502, "provider_protocol_error", "The retained endpoint is unavailable."))
		return
	}
	native, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, body, "application/json")
	if failure != nil {
		fail(retainedPublicError(upstreamError(failure), failure.status))
		return
	}
	defer native.Body.Close()
	response, readErr := readBounded(native.Body, s.retainedResponseLimit())
	if readErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		fail(serverError(502, "provider_protocol_error", "The provider returned an unsupported resource response."))
		return
	}
	responseSource, parseErr := oif.ParseJSON(response, oif.Limits{MaxBytes: retainedBodyLimit})
	if parseErr != nil || responseSource.Root().Kind() != oif.Object {
		fail(serverError(502, "provider_protocol_error", "The provider returned an invalid response."))
		return
	}
	if usageErr := observeRetainedUsage(x, responseSource, storeKind == ""); usageErr != nil {
		fail(serverError(502, "provider_protocol_error", "The provider returned invalid usage."))
		return
	}
	// Compaction has no parent resource; project its returned
	// references against the serving pin's owner-scoped identity.
	projectAs := resource
	if projectAs == nil && p != nil && route != nil {
		projectAs = &resources.Resource{APIKeyID: x.keyID, RouteSlug: route.Slug, ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: route.RevisionID, TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID}
	}
	projected, projectErr := s.projectRetainedGeneric(ctx, projectAs, codec, response)
	if projectErr != nil {
		fail(serverError(502, "provider_protocol_error", "The provider response contained an unbound resource reference."))
		return
	}
	response = projected
	status = native.StatusCode
	x.completePinned()
	s.writeStateJSONStatus(w, x, response, status)
}

func retainedRejectedWithoutWork(failure *attemptFailure) bool {
	if failure == nil {
		return false
	}
	if failure.acceptance == upstream.NotSent {
		return true
	}
	if failure.acceptance != upstream.Terminal || failure.accepted {
		return false
	}
	switch failure.status {
	case 400, 401, 403, 404, 405, 413, 415, 422, 429:
		return true
	}
	return false
}
