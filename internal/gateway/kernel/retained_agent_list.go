package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

type agentListing struct {
	server    *Server
	r         *http.Request
	x         *Call
	authority keyauthority.Authority
	endpoint  retainedapi.Endpoint
}

type agentListed struct {
	resource *resources.Resource
	pin      *Pin
	route    *routing.Route
}

type agentItem struct {
	body     []byte
	exchange *PinnedExchange
	lost     bool
}

func (s *Server) listRetainedLifecycle(w http.ResponseWriter, r *http.Request, x *Call, a keyauthority.Authority, e retainedapi.Endpoint, fail func(*fault.Error)) int {
	l := agentListing{server: s, r: r, x: x, authority: a, endpoint: e}
	rows, more, err := l.rows()
	if err != nil {
		fail(err)
		return err.Status
	}
	ready, overall, err := l.ready(rows)
	if err != nil {
		fail(err)
		return err.Status
	}
	ctx, cancel := agentPageDeadline(r.Context(), len(ready), overall)
	defer cancel()
	if err := l.reserve(ctx, ready, overall); err != nil {
		fail(err)
		return err.Status
	}
	items, delivered, err := l.read(ctx, ready)
	if err != nil {
		fail(err)
		return err.Status
	}
	response := map[string]any{e.Collection: items}
	if more && len(rows) > 0 {
		response["next_page_token"] = rows[len(rows)-1].ID
	}
	body, err := encodeRetainedPage(response)
	if err != nil {
		fail(err)
		return err.Status
	}
	s.WriteStateJSONStatus(w, x, body, http.StatusOK, delivered...)
	return http.StatusOK
}

func agentPageSize(raw string) (int, *fault.Error) {
	if raw == "" {
		return 20, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < 1 || size > 100 {
		return 0, fault.InvalidRequest("invalid_parameter", "page_size must be between 1 and 100.", nil)
	}
	return size, nil
}

func (l agentListing) rows() ([]*resources.Resource, bool, *fault.Error) {
	query := l.r.URL.Query()
	size, refusal := agentPageSize(query.Get("page_size"))
	if refusal != nil {
		return nil, false, refusal
	}
	rows, err := l.server.Deps.Resources.ListLifecycle(l.r.Context(), l.endpoint.Kind, l.x.keyID, size+1, query.Get("page_token"))
	if errors.Is(err, resources.ErrNotFound) {
		return nil, false, fault.InvalidRequest("invalid_parameter", "The page token is unavailable for this owner.", nil)
	}
	if err != nil {
		return nil, false, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider state is unavailable.")
	}
	if len(rows) > size {
		return rows[:size], true, nil
	}
	return rows, false, nil
}

func (l agentListing) ready(rows []*resources.Resource) ([]agentListed, time.Duration, *fault.Error) {
	ready := make([]agentListed, 0, len(rows))
	var overall time.Duration
	for _, resource := range rows {
		entry, readable, err := l.resolve(resource)
		if err != nil {
			return nil, 0, err
		}
		if !readable {
			continue
		}
		ready = append(ready, entry)
		timeout := time.Duration(entry.route.OverallTimeout) * time.Millisecond
		if len(ready) == 1 || timeout < overall {
			overall = timeout
		}
	}
	return ready, overall, nil
}

func (l agentListing) resolve(resource *resources.Resource) (agentListed, bool, *fault.Error) {
	if RetainedUnsettled(resource.State) || resource.ContractVersion == nil || *resource.ContractVersion != retainedapi.Revision {
		return agentListed{}, false, nil
	}
	pin, route, err := l.server.ResolveResource(l.r.Context(), l.x, l.authority, resource, l.endpoint.Operation, RetainedRetrieval)
	if err != nil {
		if err.Status != http.StatusNotFound && err.Status != http.StatusConflict {
			return agentListed{}, false, err
		}
		return agentListed{}, false, nil
	}
	if err := l.x.GuardrailSurfaceGate(route); err != nil {
		return agentListed{}, false, err
	}
	if err := l.server.retainedControlPolicy(l.x, pin, route); err != nil {
		return agentListed{}, false, err
	}
	return agentListed{resource: resource, pin: pin, route: route}, true, nil
}

// One deadline covers the page and every lease held for its reads.
func agentPageDeadline(ctx context.Context, reads int, overall time.Duration) (context.Context, context.CancelFunc) {
	if reads == 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, overall)
}

func (l agentListing) reserve(ctx context.Context, ready []agentListed, overall time.Duration) *fault.Error {
	if len(ready) == 0 {
		return nil
	}
	s, x := l.server, l.x
	if err := s.ReserveState(ctx, x, l.authority, overall); err != nil {
		return err
	}
	authority := x.AdmissionAuthority(l.authority)
	admitted := make(map[string]bool)
	costChecked := false
	for _, entry := range ready {
		if admitted[entry.route.Slug] {
			continue
		}
		lease, err := s.Deps.Admission.ReserveRouteLimits(ctx, &authority, entry.route.Slug, ResourceEstimate, overall, quota.CostReservation{}, &costChecked)
		if err != nil {
			return err
		}
		if lease != nil && x.lease == nil {
			x.lease = lease
		} else if lease != nil {
			x.lease.Join(lease)
		}
		admitted[entry.route.Slug] = true
	}
	return nil
}

func (l agentListing) read(ctx context.Context, ready []agentListed) ([]json.RawMessage, []*PinnedExchange, *fault.Error) {
	items := make([]json.RawMessage, 0, len(ready))
	var delivered []*PinnedExchange
	size := 256
	for _, entry := range ready {
		item, err := l.item(ctx, entry)
		if err != nil {
			return nil, nil, err
		}
		if item.lost {
			continue
		}
		size += len(item.body) + 1
		if size > retainedBodyLimit {
			return nil, nil, fault.ServerError(502, "provider_protocol_error", "The resource page exceeds the supported response size; request a smaller page.")
		}
		items = append(items, item.body)
		delivered = append(delivered, item.exchange)
	}
	return items, delivered, nil
}

func (l agentListing) item(ctx context.Context, entry agentListed) (agentItem, *fault.Error) {
	s, pin := l.server, entry.pin
	if err := s.gateRetainedSlot(ctx, l.x, pin, entry.route); err != nil {
		return agentItem{}, err
	}
	endpoint, urlErr := pin.Provider.Connector().ResourceURL(pin.Model, "agents/"+url.PathEscape(entry.resource.UpstreamID), nil)
	if urlErr != nil {
		s.settlePinnedHold(ctx, pin.Hold)
		return agentItem{}, PinUnavailable()
	}
	native, failure := s.PinnedDo(ctx, l.x, pin, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		s.settlePinnedHold(ctx, pin.Hold)
		return l.unread(ctx, entry.resource, failure)
	}
	body, err := l.accept(ctx, entry.resource, native)
	s.settlePinnedHold(ctx, pin.Hold)
	if err != nil {
		return agentItem{}, err
	}
	return agentItem{body: body, exchange: native.Exchange}, nil
}

func (l agentListing) unread(ctx context.Context, resource *resources.Resource, failure *AttemptFailure) (agentItem, *fault.Error) {
	if failure.Status != http.StatusNotFound {
		return agentItem{}, RetainedPublicError(UpstreamError(failure), failure.Status)
	}
	commitCtx, cancel := ResourceCommitContext(ctx)
	defer cancel()
	if err := l.server.Deps.Resources.Tombstone(commitCtx, resource.ID); err != nil {
		return agentItem{}, fault.ServerError(503, "provider_state_unavailable", "The deleted resource could not be reconciled.")
	}
	return agentItem{lost: true}, nil
}

func (l agentListing) accept(ctx context.Context, resource *resources.Resource, native *PinnedResponse) ([]byte, *fault.Error) {
	invalid := fault.ServerError(502, "provider_protocol_error", "The provider returned an invalid resource.")
	body, readErr := native.ReadBody(l.server.RetainedResponseLimit())
	native.Body.Close()
	if readErr != nil {
		return nil, invalid
	}
	if id, valid := NativeResourceID(body, retainedBodyLimit); !valid || id != resource.UpstreamID {
		return nil, invalid
	}
	rewrite, referenceErr := prepareRetainedAgent(body)
	if referenceErr != nil {
		return nil, invalid
	}
	projected, projectErr := l.server.projectRetainedAgent(ctx, resource, rewrite)
	if projectErr != nil {
		return nil, retainedProjectionError(native.Exchange, projectErr)
	}
	native.Exchange.CompleteRetainedControl()
	return projected, nil
}
