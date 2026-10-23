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

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
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

func retainedRequestRefs(e retainedapi.Endpoint) map[string]string {
	refs := map[string]string{}
	switch e.Operation {
	case "fine_tuning":
		refs["training_file"] = resources.KindFile
		refs["validation_file"] = resources.KindFile
	case "vector_store", "container":
		refs["file_ids"] = resources.KindFile
		refs["file_id"] = resources.KindFile
	case "eval":
		refs["file_id"] = resources.KindFile
	case "conversation", "compaction":
		refs["vector_store_id"] = "vector_store"
		refs["file_id"] = resources.KindFile
		refs["previous_response_id"] = resources.KindResponse
		refs["call_id"] = retainedapi.Literal
		refs["item_id"] = retainedapi.Literal
		refs["id"] = retainedapi.Literal
	case "retrieval":
		refs["file_id"] = resources.KindFile
	case "context_cache":
		refs["file_uri"] = resources.KindGeminiFile
		refs["fileUri"] = resources.KindGeminiFile
	}
	return refs
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
		p, err = s.selectBoundPinSurface(r.Context(), x, route, e.Operation, e.Surface, "unary", selected, func(provider *routing.Provider, model string) bool {
			return provider.CredentialSource != "caller" && provider.Connector().Supports(e.Operation, e.Surface, "unary") && provider.Supports(model, e.Operation, e.Surface, "unary")
		})
		if err != nil {
			fail(err)
			return
		}
		if source.Root().Kind() == oif.Object {
			rewritten, rwErr := s.rewriteRetainedRequest(r.Context(), x, authority, p, route, source, e)
			if rwErr == nil {
				rewritten, rwErr = bindRetainedModel(rewritten, e, p)
			}
			if rwErr != nil {
				fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
				return
			}
			body = rewritten.Bytes()
		}

		ctx, cancel := s.stateDeadline(r.Context(), route)
		defer cancel()
		if err := s.retainedAccountingPolicy(x, p, route, e.Metered && (e.Action == "create" || e.Action == "update" || e.Action == "patch")); err != nil {
			fail(err)
			return
		}
		if err = s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
			fail(err)
			return
		}
		if costErr := s.reserveRetainedCost(ctx, x, p, route); costErr != nil {
			fail(costErr)
			return
		}
		if err = s.gateRetainedSlot(ctx, x, p, route); err != nil {
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
		meta, _ := json.Marshal(map[string]string{"upstream_model": p.model, "contract_revision": retainedapi.Revision})
		version := retainedapi.Revision
		candidateRes := &resources.Resource{Kind: e.Kind, APIKeyID: x.keyID, RouteSlug: route.Slug, ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, RouteRevisionID: route.RevisionID, TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID, Metadata: meta, ContractVersion: &version}
		if parent != nil {
			candidateRes.ParentID = &parent.UUID
		}
		var fresh bool
		var prepareErr error
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
	if resource.State == "submitting" || resource.State == "submission_unknown" {
		fail(serverError(409, "submission_unknown", "Creation has not been reconciled; this request cannot be resubmitted."))
		return
	}
	if source.Root().Kind() == oif.Object {
		rewritten, rwErr := s.rewriteRetainedRequest(r.Context(), x, authority, p, route, source, e)
		if rwErr == nil {
			rewritten, rwErr = bindRetainedModel(rewritten, e, p)
		}
		if rwErr != nil {
			fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
			return
		}
		body = rewritten.Bytes()
	}
	ids["id"] = resource.UpstreamID
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	if err := s.retainedAccountingPolicy(x, p, route, e.Metered && (e.Action == "create" || e.Action == "update" || e.Action == "patch")); err != nil {
		fail(err)
		return
	}
	if err = s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
		fail(err)
		return
	}
	if costErr := s.reserveRetainedCost(ctx, x, p, route); costErr != nil {
		fail(costErr)
		return
	}
	if err = s.gateRetainedSlot(ctx, x, p, route); err != nil {
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
	status = s.retainedDispatch(w, r.WithContext(ctx), x, p, e, resource, endpoint, body, contentType, false, fail)
}

// retainedDispatch performs the pinned call and projects identities.
func (s *Server) retainedDispatch(w http.ResponseWriter, r *http.Request, x *call, p *pin, e retainedapi.Endpoint, resource *resources.Resource, endpoint string, body []byte, contentType string, isCreate bool, fail func(*Error)) int {
	native, failure := s.pinnedDo(r.Context(), x, p, e.Method, endpoint, body, contentType)
	if failure != nil {
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
			_ = observeRetainedUsage(x, oif.Document{}, false)
			x.completePinned()
			s.writeStateJSONStatus(w, x, nil, native.StatusCode)
			return native.StatusCode
		}
	}
	if e.Action == "content" {
		_ = observeRetainedUsage(x, oif.Document{}, false)
		x.completePinned()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(native.StatusCode)
		_, _ = w.Write(response)
		return native.StatusCode
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
		if putErr := s.Resources.CompleteLifecycleChildren(commitCtx, resource, children); putErr != nil {
			fail(serverError(503, "provider_state_unavailable", "The provider accepted creation but its resource identities could not be committed."))
			return 503
		}
	}
	projected, projectErr := s.projectRetainedGeneric(r.Context(), resource, e, response)
	if projectErr != nil {
		fail(serverError(502, "provider_protocol_error", "The provider response contained an unbound resource identity."))
		return 502
	}
	parsed, parseErr := oif.ParseJSON(response, oif.Limits{MaxBytes: retainedBodyLimit})
	if parseErr == nil {
		_ = observeRetainedUsage(x, parsed, false)
	}
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
	type listedResource struct {
		resource *resources.Resource
		pin      *pin
		route    *routing.Route
	}
	ready := make([]listedResource, 0, limit+1)
	cursor := r.URL.Query().Get(cursorParam)
	// Page over authorized bindings. Revoked or unresolved rows cannot leak
	// through continuation tokens or leave an otherwise populated page empty.
	for len(ready) <= limit {
		rows, err := s.Resources.ListLifecycleOrdered(r.Context(), e.Kind, x.keyID, 101, cursor, order == "asc")
		if err != nil {
			if errors.Is(err, resources.ErrNotFound) {
				fail(invalidRequest("invalid_parameter", "The page token is unavailable for this owner.", nil))
				return 400
			}
			fail(serverError(503, "provider_state_unavailable", "Provider state is unavailable."))
			return 503
		}
		for _, resource := range rows {
			cursor = resource.ID
			if resource.State == "submitting" || resource.State == "submission_unknown" || resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
				continue
			}
			p, route, resolveErr := s.resolveResource(r.Context(), x, a, resource, e.Operation, retainedRetrieval)
			if resolveErr != nil {
				if resolveErr.Status != http.StatusNotFound && resolveErr.Status != http.StatusConflict {
					fail(resolveErr)
					return resolveErr.Status
				}
				continue
			}
			ready = append(ready, listedResource{resource, p, route})
			if len(ready) > limit {
				break
			}
		}
		if len(rows) < 101 {
			break
		}
	}
	hasMore := len(ready) > limit
	if hasMore {
		ready = ready[:limit]
	}
	items := make([]json.RawMessage, 0, len(ready))
	var overall time.Duration
	for index, entry := range ready {
		x.route = entry.route
		if gateErr := x.guardrailSurfaceGate(entry.route); gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
		if err := s.retainedAccountingPolicy(x, entry.pin, entry.route, false); err != nil {
			fail(err)
			return err.Status
		}
		timeout := time.Duration(entry.route.OverallTimeout) * time.Millisecond
		if index == 0 || timeout < overall {
			overall = timeout
		}
	}
	ctx := r.Context()
	if len(ready) > 0 {
		// One deadline covers the page and every lease held for its reads.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overall)
		defer cancel()
		// The policy loop leaves x.route on the last entry; clearing it
		// keeps reserveState to per-key limits so the per-route pass
		// below reserves each route exactly once.
		x.route = nil
		if gateErr := s.reserveState(ctx, x, a, overall); gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
		authority := x.admissionAuthority(a)
		admitted := make(map[string]bool)
		costChecked := false
		for _, entry := range ready {
			if admitted[entry.route.Slug] {
				continue
			}
			lease, gateErr := s.Admission.reserveRouteLimits(ctx, &authority, entry.route.Slug, resourceEstimate, overall, costReservation{}, &costChecked)
			if gateErr != nil {
				fail(gateErr)
				return gateErr.Status
			}
			if lease != nil {
				if x.lease == nil {
					x.lease = lease
				} else {
					x.lease.Attach(lease)
				}
			}
			admitted[entry.route.Slug] = true
		}
	}
	responseBytes := 256
	for _, entry := range ready {
		resource, p, route := entry.resource, entry.pin, entry.route
		x.route = route
		if costErr := s.reserveRetainedCost(ctx, x, p, route); costErr != nil {
			fail(costErr)
			return costErr.Status
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
	}
	response := map[string]any{e.Collection: items}
	if hasMore && len(ready) > 0 {
		if e.Surface == "gemini" || e.Surface == "native" {
			response["nextPageToken"] = ready[len(ready)-1].resource.ID
		}
	}
	if e.Operation == "skill" {
		// The Skills auto-pager follows next_page, not has_more.
		if hasMore && len(ready) > 0 {
			response["next_page"] = ready[len(ready)-1].resource.ID
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
			if candidate.ProviderID != bound.ProviderID || candidate.ProviderRevisionID != bound.ProviderRevisionID || candidate.TargetID != bound.TargetID || candidate.SlotID != bound.SlotID || !sameCredential(candidate.CredentialID, bound.CredentialID) {
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
	if rewriteErr == nil {
		rewritten, rewriteErr = bindRetainedModel(rewritten, codec, p)
	}
	if rewriteErr != nil {
		fail(invalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
		return
	}
	body = rewritten.Bytes()
	x.estimate = int64(len(body))
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	if err := s.retainedAccountingPolicy(x, p, route, false); err != nil {
		fail(err)
		return
	}
	if err = s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); err != nil {
		fail(err)
		return
	}
	if costErr := s.reserveRetainedCost(ctx, x, p, route); costErr != nil {
		fail(costErr)
		return
	}
	if err = s.gateRetainedSlot(ctx, x, p, route); err != nil {
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
