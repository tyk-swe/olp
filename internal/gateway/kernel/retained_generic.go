package kernel

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
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

// Generic retained families share the managed-agent lifecycle contracts:
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

func (s *Server) retainedDispatch(w http.ResponseWriter, r *http.Request, x *Call, p *Pin, e retainedapi.Endpoint, resource, parent *resources.Resource, endpoint string, body []byte, contentType string, lifecycle *resources.Lifecycle, fail func(*fault.Error)) int {
	isCreate := e.Action == "create"
	deletion := e.Action == "delete" && resource.Kind == "context_cache"
	if deletion {
		deletionLifecycle, err := s.Deps.Resources.BeginDeletion(r.Context(), resource)
		lifecycle = deletionLifecycle
		if err != nil {
			s.settleFence(r.Context(), lifecycle.Reject)
			fail(fault.ServerError(503, "provider_state_unavailable", "The deletion intent could not be committed."))
			return 503
		}
		defer s.settleFence(r.Context(), lifecycle.Close)
	}

	native, failure := s.PinnedDo(r.Context(), x, p, e.Method, endpoint, body, contentType)
	if failure != nil {
		if resource != nil && (isCreate || deletion || e.Metered && (e.Action == "update" || e.Action == "patch")) && retainedRejectedWithoutWork(failure) {
			commit, cancel := ResourceCommitContext(r.Context())
			rejectErr := lifecycle.Reject(commit)
			cancel()
			if rejectErr != nil {
				fail(fault.ServerError(503, "provider_state_unavailable", "Provider rejection could not be committed."))
				return 503
			}
		}
		if resource != nil && !isCreate && failure.Status == http.StatusNotFound {
			commitCtx, cancel := ResourceCommitContext(r.Context())
			defer cancel()
			if removeErr := s.Deps.Resources.Tombstone(commitCtx, resource.ID); removeErr == nil {
				fail(fault.NotFoundError("not_found", "No resource with this identifier exists for this key."))
				return http.StatusNotFound
			}
		}
		public := retainedPublicError(UpstreamError(failure), failure.Status)
		fail(public)
		return public.Status
	}
	defer native.Body.Close()
	response, readErr := native.ReadBody(s.retainedResponseLimit())
	if readErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an unsupported resource response."))
		return 502
	}
	commitCtx, commitCancel := ResourceCommitContext(r.Context())
	defer commitCancel()
	var parsed oif.Document
	var parseErr error
	var rewrite retainedapi.PreparedRewrite
	var referenceErr error
	if e.Action != "content" {
		document := response
		if e.Action == "delete" && native.StatusCode == http.StatusNoContent {
			document = []byte(`{}`)
		}
		parsed, parseErr = oif.ParseJSON(document, oif.Limits{MaxBytes: retainedBodyLimit})
		if parseErr == nil && parsed.Root().Kind() == oif.Object {
			rewrite, referenceErr = retainedapi.PrepareRewrite(parsed, retainedResponseRefs(e, parsed))
		}
	}
	if e.Action == "delete" {
		if !retainedDeleteIdentityMatches(e, parsed, resource, parent) {
			referenceErr = resources.ErrContract
		}
		if parseErr == nil && parsed.Root().Kind() == oif.Object && referenceErr == nil {
			_ = observeRetainedUsage(native.Exchange, parsed, false)
			native.Exchange.Complete(ClassSuccess)
		}
		if putErr := s.Deps.Resources.Tombstone(commitCtx, resource.ID); putErr != nil {
			fail(fault.ServerError(503, "provider_state_unavailable", "Deletion was accepted but its local record could not be updated."))
			return 503
		}
		if deletion {
			if err := lifecycle.AcceptMutation(commitCtx); err != nil {
				fail(fault.ServerError(503, "provider_state_unavailable", "The accepted deletion could not be committed."))
				return 503
			}
		}
		if native.StatusCode == http.StatusNoContent {
			observation, observeErr := resources.ObserveRetained(resource, e, parsed, s.Now())
			if observeErr != nil || s.Deps.Resources.RecordRetainedObservation(commitCtx, resource, observation, nil) != nil {
				fail(fault.ServerError(503, "provider_state_unavailable", "Deletion evidence could not be committed."))
				return 503
			}
			s.WriteStateJSONStatus(w, x, nil, native.StatusCode, native.Exchange)
			return native.StatusCode
		}
	}
	if e.Action == "content" {
		_ = observeRetainedUsage(native.Exchange, oif.Document{}, false)
		native.Exchange.Complete(ClassSuccess)
		// Binary delivery finalizes like the JSON path: a successful
		// write commits the envelope, a failed one cancels it.
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
	if parseErr != nil || parsed.Root().Kind() != oif.Object {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid resource object."))
		return 502
	}
	if !isCreate && e.Action != "list" && e.Action != "delete" && !(e.Action == "cancel" && e.Surface == "native") {
		identity, _ := parsed.Root().Lookup(e.Identity)
		if id, ok := identity.Text(); !ok || id != resource.UpstreamID {
			fail(fault.ServerError(502, "provider_protocol_error", "The provider returned a different resource identity."))
			return 502
		}
	}
	var creation *resources.PreparedCreation
	if isCreate {
		ids, idErr := retainedCreateIDs(e, response)
		if idErr != nil {
			fail(fault.ServerError(502, "provider_protocol_error", "The provider response omitted a valid resource identity."))
			return 502
		}
		prepared, prepareErr := lifecycle.PrepareCreated(resources.CreationResult{
			IDs: ids, Observation: &resources.CreationObservation{Endpoint: e, Document: parsed, At: s.Now()},
		})
		if errors.Is(prepareErr, resources.ErrRetainedMetadata) {
			fail(fault.ServerError(502, "provider_protocol_error", "The provider returned invalid resource billing metadata."))
			return 502
		}
		if prepareErr != nil {
			native.Exchange.Interrupt(ClassProtocol, true)
			fail(fault.ServerError(503, "provider_state_unavailable", "The provider accepted creation but its resource identities could not be committed."))
			return 503
		}
		creation, resource = prepared, prepared.Resource()
	} else if e.Action != "list" && e.Action != "delete" {
		if err := resources.EnrichRetained(resource, e, parsed, s.Now()); err != nil {
			fail(fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed."))
			return 503
		}
	}
	observation, observeErr := resources.ObserveRetained(resource, e, parsed, s.Now())
	if creation != nil {
		if err := creation.Commit(commitCtx); err != nil {
			if referenceErr == nil && observeErr == nil {
				native.Exchange.Interrupt(ClassProtocol, true)
			}
			fail(fault.ServerError(503, "provider_state_unavailable", "The provider accepted creation but its resource identities could not be committed."))
			return 503
		}
	}
	if referenceErr != nil {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider response contained an unbound resource identity."))
		return 502
	}
	if observeErr != nil {
		fail(fault.ServerError(503, "provider_state_unavailable", "Resource usage evidence could not be committed."))
		return 503
	}
	if s.persistRetainedResult(commitCtx, resource, e, parsed) != nil {
		native.Exchange.Interrupt(ClassProtocol, true)
		fail(fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed."))
		return 503
	}
	projected, projectErr := s.projectRetainedGeneric(r.Context(), resource, e, parsed, rewrite)
	if projectErr != nil {
		if e.Action == "delete" {
			fail(fault.ServerError(502, "provider_protocol_error", "The provider response contained an unbound resource identity."))
			return 502
		}
		public := retainedProjectionError(native.Exchange, projectErr)
		fail(public)
		return public.Status
	}
	if e.Action != "delete" {
		_ = observeRetainedUsage(native.Exchange, parsed, false)
		native.Exchange.Complete(ClassSuccess)
	}
	if s.Deps.Resources.RecordRetainedObservation(commitCtx, resource, observation, nil) != nil {
		fail(fault.ServerError(503, "provider_state_unavailable", "Resource usage evidence could not be committed."))
		return 503
	}
	if e.Metered && (e.Action == "update" || e.Action == "patch") {
		if err := lifecycle.AcceptMutation(commitCtx); err != nil {
			fail(fault.ServerError(503, "provider_state_unavailable", "The accepted mutation could not be committed."))
			return 503
		}
	}
	s.WriteStateJSONStatus(w, x, projected, native.StatusCode, native.Exchange)
	return native.StatusCode
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

func (s *Server) listRetainedGeneric(w http.ResponseWriter, r *http.Request, x *Call, a keyauthority.Authority, e retainedapi.Endpoint, fail func(*fault.Error)) int {
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
		if name == "key" && e.Surface == "gemini" && len(values) == 1 && values[0] != "" {
			continue
		}
		if (name != pageSizeParam && name != cursorParam && !(name == "order" && e.Surface == "openai")) || len(values) != 1 {
			fail(fault.InvalidRequest("invalid_parameter", "Unsupported or repeated list parameter.", &name))
			return 400
		}
	}
	order := r.URL.Query().Get("order")
	if order != "" && order != "asc" && order != "desc" {
		fail(fault.InvalidRequest("invalid_parameter", "order must be asc or desc.", nil))
		return 400
	}
	limit := 20
	if raw := r.URL.Query().Get(pageSizeParam); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			fail(fault.InvalidRequest("invalid_parameter", "Page size must be between 1 and 100.", nil))
			return 400
		}
		limit = parsed
	}
	ready := make([]retainedListEntry, 0, limit+1)
	requestedRoute := strings.TrimSpace(r.Header.Get(StateRouteHeader))
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
				rows, err := s.Deps.Resources.ListLifecycleOrdered(ctx, e.Kind, x.keyID, 101, cursor, order == "asc", requestedRoute)
				if err != nil {
					if errors.Is(err, resources.ErrNotFound) {
						fail(fault.InvalidRequest("invalid_parameter", "The page token is unavailable for this owner.", nil))
						return 400
					}
					fail(fault.ServerError(503, "provider_state_unavailable", "Provider state is unavailable."))
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
				p, route, resolveErr := s.ResolveResource(ctx, x, a, resource, e.Operation, RetainedRetrieval)
				if resolveErr != nil {
					if resolveErr.Status != http.StatusNotFound && resolveErr.Status != http.StatusConflict {
						fail(resolveErr)
						return resolveErr.Status
					}
					continue
				}
				x.Route = route
				if gateErr := x.GuardrailSurfaceGate(route); gateErr != nil {
					fail(gateErr)
					return gateErr.Status
				}
				admission, policyErr := s.prepareRetainedAdmission(x, p, false)
				if policyErr != nil {
					fail(policyErr)
					return policyErr.Status
				}
				entry := retainedListEntry{resource: resource, pin: p, route: route, admission: admission}
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
	callerCostsAdmitted := !retainedPageFreeControl(ready, limit)
	costChecked := false
	// Route leases stay separate from the caller's page lease: each
	// route's cost budget is grown and settled with only its own
	// reads, never the whole page total.
	routeLeases := make(map[string]*limitadmission.Lease)
	routeReserved := make(map[string]rates.Cost)
	routeReservedCharge := make(map[string]rates.Cost)
	routeActual := make(map[string]rates.Cost)
	routeActualCharge := make(map[string]rates.Cost)
	lastRouteRead := make(map[string]*PinnedExchange)
	defer func() {
		zero := int64(0)
		for slug, lease := range routeLeases {
			read := lastRouteRead[slug]
			if read != nil && !read.fact().BillingUncertain {
				lease.SetActualCost(routeActual[slug].String(), routeActualCharge[slug].String())
			}
			quota.SettleKey(ctx, lease, read != nil, &zero, s.Log)
		}
	}()
	admitRoute := func(entry retainedListEntry) int {
		if admitted[entry.route.Slug] && (entry.admission.billing == retainedFree || routeCostsAdmitted[entry.route.Slug]) {
			return 0
		}
		authority := entry.admission.authority(a, x.callerCostExempt())
		authority.Attribution = x.attribution
		if admitted[entry.route.Slug] {
			// Earlier free reads reserved only this route's rates.
			authority = retainedCostAuthority(authority)
		}
		lease, gateErr := s.Deps.Admission.ReserveRouteLimits(ctx, &authority, entry.route.Slug, resourceEstimate, overall, quota.CostReservation{}, &costChecked)
		if gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
		if lease != nil {
			if prior := routeLeases[entry.route.Slug]; prior != nil {
				prior.Join(lease)
			} else {
				routeLeases[entry.route.Slug] = lease
			}
		}
		admitted[entry.route.Slug] = true
		if entry.admission.billing != retainedFree {
			routeCostsAdmitted[entry.route.Slug] = true
		}
		return 0
	}
	if len(ready) > 0 {
		// One deadline covers the page and every lease held for its reads.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overall)
		defer cancel()
		if gateErr := s.reserveRetainedPage(ctx, x, a, ready, limit, overall); gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
	}
	var delivered []*PinnedExchange
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
		x.retainedAdmission = &entry.admission
		entry.pin.attempt = entry.admission.attempt
		x.Route = entry.route
		if entry.admission.billing != retainedFree && !callerCostsAdmitted {
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
		x.Route = route
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
		endpoint, urlErr := p.Provider.Connector().ResourceURL(p.Model, retainedUpstreamPath(retainedapi.Endpoint{Family: e.Family, Action: "get", Method: "GET", Pattern: e.Path + "/{id}"}, map[string]string{"id": resource.UpstreamID}), nil)
		if urlErr != nil {
			s.settlePinnedHold(ctx, p.Hold)
			fail(PinUnavailable())
			return 409
		}
		native, callErr := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if callErr != nil {
			s.settlePinnedHold(ctx, p.Hold)
			if callErr.Status == http.StatusNotFound {
				commitCtx, commitCancel := ResourceCommitContext(ctx)
				removeErr := s.Deps.Resources.Tombstone(commitCtx, resource.ID)
				commitCancel()
				if removeErr != nil {
					fail(fault.ServerError(503, "provider_state_unavailable", "The deleted resource could not be reconciled."))
					return 503
				}
				continue
			}
			public := retainedPublicError(UpstreamError(callErr), callErr.Status)
			fail(public)
			return public.Status
		}
		lastRouteRead[route.Slug] = native.Exchange
		body, readErr := native.ReadBody(s.retainedResponseLimit())
		native.Body.Close()
		if readErr == nil {
			objectEndpoint := e
			objectEndpoint.Action = "get"
			ids, idErr := retainedCreateIDs(objectEndpoint, body)
			if idErr != nil || len(ids) != 1 || ids[0] != resource.UpstreamID {
				readErr = resources.ErrContract
			}
		}
		if readErr != nil {
			s.settlePinnedHold(ctx, p.Hold)
			fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid resource."))
			return 502
		}
		objectEndpoint := retainedapi.Endpoint{Family: e.Family, Action: "get"}
		source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: retainedBodyLimit})
		if parseErr != nil {
			s.settlePinnedHold(ctx, p.Hold)
			fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid resource."))
			return 502
		}
		if err := resources.EnrichRetained(resource, objectEndpoint, source, s.Now()); err != nil {
			s.settlePinnedHold(ctx, p.Hold)
			fail(fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed."))
			return 503
		}
		observation, observeErr := resources.ObserveRetained(resource, objectEndpoint, source, s.Now())
		if observeErr != nil {
			s.settlePinnedHold(ctx, p.Hold)
			fail(fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed."))
			return 503
		}
		rewrite, referenceErr := retainedapi.PrepareRewrite(source, retainedResponseRefs(objectEndpoint, source))
		if referenceErr != nil {
			s.settlePinnedHold(ctx, p.Hold)
			fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid resource."))
			return 502
		}
		projected, projectErr := s.projectRetainedGeneric(ctx, resource, objectEndpoint, source, rewrite)
		if projectErr != nil {
			public := retainedProjectionError(native.Exchange, projectErr)
			s.settlePinnedHold(ctx, p.Hold)
			fail(public)
			return public.Status
		}
		_ = observeRetainedUsage(native.Exchange, source, false)
		native.Exchange.Complete(ClassSuccess)
		s.settlePinnedHold(ctx, p.Hold)
		routeActual[route.Slug] = routeActual[route.Slug].Add(bound)
		routeActualCharge[route.Slug] = routeActualCharge[route.Slug].Add(charge)
		commitCtx, commitCancel := ResourceCommitContext(ctx)
		persistErr := s.persistRetainedResult(commitCtx, resource, objectEndpoint, source)
		if persistErr == nil {
			persistErr = s.Deps.Resources.RecordRetainedObservation(commitCtx, resource, observation, nil)
		}
		commitCancel()
		if persistErr != nil {
			fail(fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed."))
			return 503
		}
		responseBytes += len(projected) + 1
		if responseBytes > retainedBodyLimit {
			fail(fault.ServerError(502, "provider_protocol_error", "The resource page exceeds the supported response size; request a smaller page."))
			return 502
		}
		items = append(items, projected)
		delivered = append(delivered, native.Exchange)
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
	s.WriteStateJSONStatus(w, x, body, http.StatusOK, delivered...)
	return 200
}

// retainedSearch serves native vector-store search and query-only bindings
// through the one authorized retrieval path. It creates no resource.
func (s *Server) retainedSearch(w http.ResponseWriter, r *http.Request) {
	if id, err := uuid.Parse(r.PathValue("id")); err == nil {
		r.SetPathValue("binding_id", id.String())
		s.queryRetrieval(w, r)
		return
	}
	s.retainedProxy(w, r, "retrieval", "vector_stores/{id}/search", "vector_store")
}

// retainedCompact serves native compaction as its own priced call.
func (s *Server) retainedCompact(w http.ResponseWriter, r *http.Request) {
	s.retainedProxy(w, r, "generation", "responses/compact", "")
}

func (s *Server) retainedProxy(w http.ResponseWriter, r *http.Request, operation, upstreamTemplate, storeKind string) {
	x, authority, done := s.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	x.operationOverride = operation
	x.mode = "unary"
	x.actor = "api_key_resource"
	x.retained = retainedOperation{use: RetainedNewWork, billing: retainedMeteredWork}
	var p *Pin
	status := http.StatusInternalServerError
	defer func() { s.Finish(x, nil, status); s.retainedSettle(r.Context(), x, p) }()
	fail := func(err *fault.Error) {
		x.Failure = err
		status = err.Status
		fault.WriteSurfaceError(w, err, "openai")
	}
	if !authority.Policy.AllowProviderState {
		fail(providerStateForbidden())
		return
	}
	body, err := s.Gate.ReadBody(r)
	if err != nil {
		fail(err)
		return
	}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.Config.MaxBodyBytes)})
	if parseErr != nil || source.Root().Kind() != oif.Object {
		fail(fault.InvalidRequest("invalid_request", "A valid JSON object is required.", nil))
		return
	}
	codec := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "retrieval", Surface: "openai"}}
	if storeKind == "" {
		codec.Operation = "compaction"
	}
	if len(r.URL.Query()) != 0 {
		fail(fault.InvalidRequest("invalid_parameter", "This operation does not support query parameters.", nil))
		return
	}
	var route *routing.Route
	var resource *resources.Resource
	upstreamPath := upstreamTemplate
	if storeKind != "" {
		var getErr error
		resource, getErr = s.Deps.Resources.Get(r.Context(), storeKind, x.keyID, r.PathValue("id"))
		if getErr != nil {
			fail(fault.NotFoundError("not_found", "No resource with this identifier exists for this key."))
			return
		}
		var resolveErr *fault.Error
		// Searching a retained store starts new billable work, so it
		// must satisfy current new-work restrictions rather than the
		// historical retrieval contract that created the binding.
		p, route, resolveErr = s.ResolveResource(r.Context(), x, authority, resource, operation, RetainedNewWork)
		if resolveErr != nil {
			fail(resolveErr)
			return
		}
		upstreamPath = strings.Replace(upstreamTemplate, "{id}", url.PathEscape(resource.UpstreamID), 1)
	} else {
		slug := strings.TrimSpace(r.Header.Get(StateRouteHeader))
		if slug == "" {
			fail(fault.InvalidRequest("missing_required_parameter", "X-OLP-Route is required.", nil))
			return
		}
		candidate, ok := x.Snapshot().FindRoute(slug)
		if !ok {
			fail(fault.ModelNotFound(slug))
			return
		}
		route = &candidate
		x.Route = route
		if !authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
			fail(fault.PermissionError("route_forbidden", "The API key cannot use this route."))
			return
		}
		if !slices.Contains(route.Operations, operation) {
			fail(PolicyUnavailable("operation_unavailable", "The route does not allow this operation."))
			return
		}
		if gateErr := x.GuardrailSurfaceGate(route); gateErr != nil {
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
					fail(fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
					return
				}
				candidates = append(candidates, resolved)
			}
		}
		for _, fileID := range retainedFileIDs(source.Root()) {
			resolved, _, readErr := s.readDurable(r.Context(), durableKind(resources.KindFile, fileID), x.keyID, fileID)
			if readErr != nil {
				fail(fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
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
				fail(fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
				return
			}
		}
		var pinErr *fault.Error
		p, pinErr = s.selectBoundPinSurface(r.Context(), x, route, operation, "openai", "unary", bound, func(provider *routing.Provider, model string) bool {
			return provider.CredentialSource != "caller" && (provider.Kind == "openai" || provider.Kind == "azure_openai") && provider.Connector().Supports(operation, "openai", "unary")
		})
		if pinErr != nil {
			fail(pinErr)
			return
		}
	}
	x.Route = route
	if gateErr := x.GuardrailSurfaceGate(route); gateErr != nil {
		fail(gateErr)
		return
	}
	rewritten, rewriteErr := s.rewriteRetainedRequest(r.Context(), x, authority, p, route, source, codec)
	if rewriteErr != nil {
		fail(fault.InvalidRequest("invalid_resource_reference", "A resource reference is unavailable for this owner and target.", nil))
		return
	}
	body = rewritten.Bytes()
	x.estimate = int64(len(body))
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	if err := s.admitRetainedRequest(ctx, x, authority, p, route, false); err != nil {
		fail(err)
		return
	}
	endpoint, urlErr := p.Provider.Connector().ResourceURL(p.Model, upstreamPath, nil)
	if urlErr != nil {
		fail(fault.ServerError(502, "provider_protocol_error", "The retained endpoint is unavailable."))
		return
	}
	native, failure := s.PinnedDo(ctx, x, p, http.MethodPost, endpoint, body, "application/json")
	if failure != nil {
		fail(retainedPublicError(UpstreamError(failure), failure.Status))
		return
	}
	defer native.Body.Close()
	response, readErr := native.ReadBody(s.retainedResponseLimit())
	if readErr != nil || native.StatusCode < 200 || native.StatusCode >= 300 {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an unsupported resource response."))
		return
	}
	responseSource, parseErr := oif.ParseJSON(response, oif.Limits{MaxBytes: retainedBodyLimit})
	if parseErr != nil || responseSource.Root().Kind() != oif.Object {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid response."))
		return
	}
	generation := storeKind == ""
	if generation && observeRetainedUsage(native.Exchange, responseSource, true) != nil {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider returned invalid usage."))
		return
	}
	rewrite, referenceErr := retainedapi.PrepareRewrite(responseSource, retainedResponseRefs(codec, responseSource))
	if referenceErr != nil {
		fail(fault.ServerError(502, "provider_protocol_error", "The provider response contained an unbound resource reference."))
		return
	}
	// Compaction has no parent resource; project its returned
	// references against the serving pin's owner-scoped identity.
	projectAs := resource
	if projectAs == nil && p != nil && route != nil {
		projectAs = &resources.Resource{APIKeyID: x.keyID, RouteSlug: route.Slug, ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID, TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID}
	}
	projected, projectErr := s.projectRetainedGeneric(ctx, projectAs, codec, responseSource, rewrite)
	if projectErr != nil {
		fail(retainedProjectionError(native.Exchange, projectErr))
		return
	}
	if !generation {
		_ = observeRetainedUsage(native.Exchange, responseSource, false)
	}
	native.Exchange.Complete(ClassSuccess)
	response = projected
	status = native.StatusCode
	s.WriteStateJSONStatus(w, x, response, status, native.Exchange)
}

func retainedRejectedWithoutWork(failure *AttemptFailure) bool {
	if failure == nil {
		return false
	}
	if failure.Acceptance == upstream.NotSent {
		return true
	}
	if failure.Acceptance != upstream.Terminal || failure.accepted {
		return false
	}
	switch failure.Status {
	case 400, 401, 403, 404, 405, 413, 415, 422, 429:
		return true
	}
	return false
}
