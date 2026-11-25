package retained

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

type retainedListQuery struct {
	limit     int
	cursor    string
	ascending bool
	route     string
}

type retainedListing struct {
	server    surface
	x         *kernel.Call
	authority keyauthority.Authority
	endpoint  retainedapi.Endpoint
	query     retainedListQuery
}

type retainedScan struct {
	cursor    string
	pending   []*resources.Resource
	lastBatch bool
	exhausted bool
	ready     []kernel.RetainedListEntry
	overall   time.Duration
}

type retainedPage struct {
	items     []json.RawMessage
	delivered []*kernel.PinnedExchange
	last      *resources.Resource
	more      bool
}

type retainedListItem struct {
	resource  *resources.Resource
	projected json.RawMessage
	exchange  *kernel.PinnedExchange
}

type retainedAccepted struct {
	source      oif.Document
	observation resources.RetainedObservation
	projected   json.RawMessage
}

func (s surface) listRetainedGeneric(w http.ResponseWriter, r *http.Request, x *kernel.Call, a keyauthority.Authority, e retainedapi.Endpoint, fail func(*fault.Error)) int {
	query, refusal := readRetainedListQuery(r, e)
	if refusal != nil {
		fail(refusal)
		return refusal.Status
	}
	l := retainedListing{server: s, x: x, authority: a, endpoint: e, query: query}
	scan, refusal := l.fill(r.Context(), retainedScan{cursor: query.cursor, ready: make([]kernel.RetainedListEntry, 0, query.limit+1)}, query.limit)
	if refusal != nil {
		fail(refusal)
		return refusal.Status
	}
	ledger := kernel.NewRetainedPageLedger(s.kernel, x, a, !kernel.RetainedPageFreeControl(scan.ready, query.limit))
	ctx, cancel := retainedPageDeadline(r.Context(), scan)
	defer ledger.Settle(ctx)
	defer cancel()
	if len(scan.ready) > 0 {
		if gateErr := s.kernel.ReserveRetainedPage(ctx, x, a, scan.ready, query.limit, scan.overall); gateErr != nil {
			fail(gateErr)
			return gateErr.Status
		}
	}
	page, refusal := l.collect(ctx, ledger, scan)
	if refusal != nil {
		fail(refusal)
		return refusal.Status
	}
	body, encodeErr := encodeRetainedPage(page.document(e))
	if encodeErr != nil {
		fail(encodeErr)
		return encodeErr.Status
	}
	s.kernel.WriteStateJSONStatus(w, x, body, http.StatusOK, page.delivered...)
	return http.StatusOK
}

func retainedListParameters(e retainedapi.Endpoint) (size, cursor string) {
	switch {
	case e.Surface == "gemini" || e.Surface == "native":
		return "pageSize", "pageToken"
	case e.Surface == "anthropic" && e.Operation == "skill":
		return "limit", "page"
	case e.Surface == "anthropic":
		return "limit", "after_id"
	}
	return "limit", "after"
}

func readRetainedListQuery(r *http.Request, e retainedapi.Endpoint) (retainedListQuery, *fault.Error) {
	sizeParam, cursorParam := retainedListParameters(e)
	asked := r.URL.Query()
	for name, values := range asked {
		if name == "beta" && e.Operation == "skill" && len(values) == 1 && values[0] == "true" {
			continue
		}
		if name == "key" && e.Surface == "gemini" && len(values) == 1 && values[0] != "" {
			continue
		}
		if (name != sizeParam && name != cursorParam && !(name == "order" && e.Surface == "openai")) || len(values) != 1 {
			return retainedListQuery{}, fault.InvalidRequest("invalid_parameter", "Unsupported or repeated list parameter.", &name)
		}
	}
	order := asked.Get("order")
	if order != "" && order != "asc" && order != "desc" {
		return retainedListQuery{}, fault.InvalidRequest("invalid_parameter", "order must be asc or desc.", nil)
	}
	limit, refusal := retainedPageSize(asked.Get(sizeParam))
	if refusal != nil {
		return retainedListQuery{}, refusal
	}
	return retainedListQuery{limit: limit, cursor: asked.Get(cursorParam), ascending: order == "asc", route: strings.TrimSpace(r.Header.Get(kernel.StateRouteHeader))}, nil
}

func retainedPageSize(raw string) (int, *fault.Error) {
	if raw == "" {
		return 20, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 || parsed > 100 {
		return 0, fault.InvalidRequest("invalid_parameter", "Page size must be between 1 and 100.", nil)
	}
	return parsed, nil
}

func retainedPageDeadline(ctx context.Context, scan retainedScan) (context.Context, context.CancelFunc) {
	if len(scan.ready) == 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, scan.overall)
}

// fill reads the owner's rows until the scan holds more than target readable
// entries or the rows run out. Each entry is policy-checked as it is collected,
// so entries added by a refill carry the same prices and guardrails.
func (l retainedListing) fill(ctx context.Context, from retainedScan, target int) (retainedScan, *fault.Error) {
	scan := from
	for len(scan.ready) <= target && !scan.exhausted {
		next, refusal := l.advance(ctx, scan, target)
		if refusal != nil {
			return scan, refusal
		}
		scan = next
	}
	return scan, nil
}

func (l retainedListing) advance(ctx context.Context, from retainedScan, target int) (retainedScan, *fault.Error) {
	scan := from
	if len(scan.pending) == 0 {
		rows, err := l.server.kernel.Deps.Resources.ListLifecycleOrdered(ctx, l.endpoint.Kind, l.x.KeyID(), 101, scan.cursor, l.query.ascending, l.query.route)
		if errors.Is(err, resources.ErrNotFound) {
			return scan, fault.InvalidRequest("invalid_parameter", "The page token is unavailable for this owner.", nil)
		}
		if err != nil {
			return scan, fault.ServerError(503, "provider_state_unavailable", "Provider state is unavailable.")
		}
		scan.pending, scan.lastBatch = rows, len(rows) < 101
		if len(rows) == 0 {
			scan.exhausted = true
			return scan, nil
		}
	}
	consumed := 0
	for _, resource := range scan.pending {
		consumed++
		scan.cursor = resource.ID
		entry, readable, refusal := l.candidate(ctx, resource)
		if refusal != nil {
			return scan, refusal
		}
		if !readable {
			continue
		}
		timeout := time.Duration(entry.Route.OverallTimeout) * time.Millisecond
		if len(scan.ready) == 0 || timeout < scan.overall {
			scan.overall = timeout
		}
		scan.ready = append(scan.ready, entry)
		if len(scan.ready) > target {
			break
		}
	}
	scan.pending = scan.pending[consumed:]
	scan.exhausted = len(scan.pending) == 0 && scan.lastBatch
	return scan, nil
}

func (l retainedListing) candidate(ctx context.Context, resource *resources.Resource) (kernel.RetainedListEntry, bool, *fault.Error) {
	s, x := l.server, l.x
	if kernel.RetainedUnsettled(resource.State) || resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
		return kernel.RetainedListEntry{}, false, nil
	}
	p, route, resolveErr := s.kernel.ResolveResource(ctx, x, l.authority, resource, l.endpoint.Operation, kernel.RetainedRetrieval)
	if resolveErr != nil {
		if resolveErr.Status != http.StatusNotFound && resolveErr.Status != http.StatusConflict {
			return kernel.RetainedListEntry{}, false, resolveErr
		}
		return kernel.RetainedListEntry{}, false, nil
	}
	x.Route = route
	if gateErr := x.GuardrailSurfaceGate(route); gateErr != nil {
		return kernel.RetainedListEntry{}, false, gateErr
	}
	admission, policyErr := s.kernel.PrepareRetainedAdmission(x, p, false)
	if policyErr != nil {
		return kernel.RetainedListEntry{}, false, policyErr
	}
	return kernel.RetainedListEntry{Resource: resource, Pin: p, Route: route, Admission: admission}, true, nil
}

// collect reads entries until the page is full, refilling the scan after a
// provider lost a resource so a page never returns empty with a continuation
// while live resources remain.
func (l retainedListing) collect(ctx context.Context, ledger *kernel.RetainedPageLedger, from retainedScan) (retainedPage, *fault.Error) {
	scan, page, next, size := from, retainedPage{items: make([]json.RawMessage, 0, l.query.limit)}, 0, 256
	for len(page.items) < l.query.limit && (next < len(scan.ready) || !scan.exhausted) {
		if next >= len(scan.ready) {
			refilled, refusal := l.fill(ctx, scan, len(scan.ready))
			if refusal != nil {
				return page, refusal
			}
			scan = refilled
			if next >= len(scan.ready) {
				break
			}
			continue
		}
		entry := scan.ready[next]
		next++
		item, gone, refusal := l.read(ctx, ledger, entry, scan.overall)
		if refusal != nil {
			return page, refusal
		}
		if gone {
			continue
		}
		size += len(item.projected) + 1
		if size > kernel.RetainedBodyLimit {
			return page, fault.ServerError(502, "provider_protocol_error", "The resource page exceeds the supported response size; request a smaller page.")
		}
		page.items = append(page.items, item.projected)
		page.delivered = append(page.delivered, item.exchange)
		page.last = item.resource
	}
	page.more = next < len(scan.ready) || !scan.exhausted
	return page, nil
}

// read admits one entry, reads it from its provider and returns what the page
// shows for it. gone reports a resource its provider no longer has.
func (l retainedListing) read(ctx context.Context, ledger *kernel.RetainedPageLedger, entry kernel.RetainedListEntry, overall time.Duration) (retainedListItem, bool, *fault.Error) {
	s, x, resource, p, route := l.server, l.x, entry.Resource, entry.Pin, entry.Route
	x.BindRetainedAdmission(p, entry.Admission)
	x.Route = route
	cost, refusal := ledger.Reserve(ctx, entry, overall)
	if refusal != nil {
		return retainedListItem{}, false, refusal
	}
	if gateErr := s.kernel.GateRetainedSlot(ctx, x, p, route); gateErr != nil {
		return retainedListItem{}, false, gateErr
	}
	native, gone, refusal := l.ask(ctx, entry)
	if refusal != nil {
		s.kernel.SettlePinnedHold(ctx, p.Hold)
		return retainedListItem{}, false, refusal
	}
	if gone {
		s.kernel.SettlePinnedHold(ctx, p.Hold)
		return retainedListItem{}, true, l.forget(ctx, resource)
	}
	ledger.Answered(route.Slug, native.Exchange)
	accepted, refusal := l.accept(ctx, resource, native)
	s.kernel.SettlePinnedHold(ctx, p.Hold)
	if refusal != nil {
		return retainedListItem{}, false, refusal
	}
	ledger.Spent(route.Slug, cost)
	if refusal := l.commit(ctx, resource, accepted); refusal != nil {
		return retainedListItem{}, false, refusal
	}
	return retainedListItem{resource: resource, projected: accepted.projected, exchange: native.Exchange}, false, nil
}

func (l retainedListing) ask(ctx context.Context, entry kernel.RetainedListEntry) (*kernel.PinnedResponse, bool, *fault.Error) {
	e, p := l.endpoint, entry.Pin
	object := retainedapi.Endpoint{Family: e.Family, Action: "get", Method: "GET", Pattern: e.Path + "/{id}"}
	endpoint, urlErr := p.Provider.Connector().ResourceURL(p.Model, retainedUpstreamPath(object, map[string]string{"id": entry.Resource.UpstreamID}), nil)
	if urlErr != nil {
		return nil, false, kernel.PinUnavailable()
	}
	native, callErr := l.server.kernel.PinnedDo(ctx, l.x, p, http.MethodGet, endpoint, nil, "")
	if callErr == nil {
		return native, false, nil
	}
	if callErr.Status == http.StatusNotFound {
		return nil, true, nil
	}
	return nil, false, kernel.RetainedPublicError(kernel.UpstreamError(callErr), callErr.Status)
}

func (l retainedListing) forget(ctx context.Context, resource *resources.Resource) *fault.Error {
	commitCtx, commitCancel := kernel.ResourceCommitContext(ctx)
	defer commitCancel()
	if err := l.server.kernel.Deps.Resources.Tombstone(commitCtx, resource.ID); err != nil {
		return fault.ServerError(503, "provider_state_unavailable", "The deleted resource could not be reconciled.")
	}
	return nil
}

func (l retainedListing) accept(ctx context.Context, resource *resources.Resource, native *kernel.PinnedResponse) (retainedAccepted, *fault.Error) {
	s, e := l.server, l.endpoint
	invalid := fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid resource.")
	uncommitted := fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed.")
	body, readErr := native.ReadBody(s.kernel.RetainedResponseLimit())
	native.Body.Close()
	if readErr != nil || !retainedNames(e, body, resource.UpstreamID) {
		return retainedAccepted{}, invalid
	}
	object := retainedapi.Endpoint{Family: e.Family, Action: "get"}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: kernel.RetainedBodyLimit})
	if parseErr != nil {
		return retainedAccepted{}, invalid
	}
	if err := resources.EnrichRetained(resource, object, source, s.kernel.Now()); err != nil {
		return retainedAccepted{}, uncommitted
	}
	observation, observeErr := resources.ObserveRetained(resource, object, source, s.kernel.Now())
	if observeErr != nil {
		return retainedAccepted{}, uncommitted
	}
	rewrite, referenceErr := retainedapi.PrepareRewrite(source, kernel.RetainedResponseRefs(object, source))
	if referenceErr != nil {
		return retainedAccepted{}, invalid
	}
	projected, projectErr := s.kernel.ProjectRetainedGeneric(ctx, resource, object, source, rewrite)
	if projectErr != nil {
		return retainedAccepted{}, kernel.RetainedProjectionError(native.Exchange, projectErr)
	}
	_ = kernel.ObserveRetainedUsage(native.Exchange, source, false)
	native.Exchange.Complete(kernel.ClassSuccess)
	return retainedAccepted{source: source, observation: observation, projected: projected}, nil
}

func retainedNames(e retainedapi.Endpoint, body []byte, upstreamID string) bool {
	object := e
	object.Action = "get"
	ids, err := kernel.RetainedCreateIDs(object, body)
	return err == nil && len(ids) == 1 && ids[0] == upstreamID
}

func (l retainedListing) commit(ctx context.Context, resource *resources.Resource, accepted retainedAccepted) *fault.Error {
	s := l.server
	object := retainedapi.Endpoint{Family: l.endpoint.Family, Action: "get"}
	commitCtx, commitCancel := kernel.ResourceCommitContext(ctx)
	defer commitCancel()
	if err := s.kernel.PersistRetainedResult(commitCtx, resource, object, accepted.source); err != nil {
		return fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed.")
	}
	if err := s.kernel.Deps.Resources.RecordRetainedObservation(commitCtx, resource, accepted.observation, nil); err != nil {
		return fault.ServerError(503, "provider_state_unavailable", "Resource billing metadata could not be committed.")
	}
	return nil
}

func (p retainedPage) document(e retainedapi.Endpoint) map[string]any {
	response := map[string]any{e.Collection: p.items}
	continues := p.more && p.last != nil
	if continues && (e.Surface == "gemini" || e.Surface == "native") {
		response["nextPageToken"] = p.last.ID
	}
	if e.Operation == "skill" {
		// The Skills auto-pager follows next_page, not has_more.
		response["next_page"] = nil
		if continues {
			response["next_page"] = p.last.ID
		}
	}
	if e.Surface != "openai" && e.Surface != "anthropic" {
		return response
	}
	response["has_more"] = p.more
	if e.Surface == "openai" {
		response["object"] = "list"
	}
	if len(p.items) > 0 {
		response["first_id"] = retainedItemIdentity(p.items[0], e.Identity)
		response["last_id"] = retainedItemIdentity(p.items[len(p.items)-1], e.Identity)
	}
	return response
}

func retainedItemIdentity(item json.RawMessage, field string) string {
	parsed, _ := oif.ParseJSON(item, oif.Limits{MaxBytes: kernel.RetainedBodyLimit})
	identity, _ := parsed.Root().Lookup(field)
	text, _ := identity.Text()
	return text
}
