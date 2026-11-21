package bedrock

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type turn struct {
	surface
	w         http.ResponseWriter
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	operation string
	mode      string
	upstream  string
}

func (t turn) refuse(e *fault.Error) { t.kernel.Refuse(t.x, t.w, e) }

func (s surface) serve(w http.ResponseWriter, r *http.Request, family wire.Format, operation, mode, upstreamOp string) {
	x, authority, ok := s.kernel.EnterBedrock(w, r, family, mode)
	if !ok {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	t := turn{surface: s, w: w, r: r, x: x, authority: authority, operation: operation, mode: mode, upstream: upstreamOp}
	route, e := t.route()
	if e != nil {
		t.refuse(e)
		return
	}
	body, e := s.kernel.Gate.ReadBody(r)
	if e != nil {
		t.refuse(e)
		return
	}
	// Bedrock inference is metered like any other generation: the request
	// size bounds the reservation and the reported usage settles it.
	x.SizedBody(len(body))
	defer s.kernel.SettleCaps(r.Context(), x)
	pin, e := t.pin(route)
	if e != nil {
		t.refuse(e)
		return
	}
	defer s.kernel.SettleBedrock(r.Context(), x, pin)
	ctx, cancel, e := t.reserve(route)
	if e != nil {
		t.refuse(e)
		return
	}
	defer cancel()
	if operation == "generation" {
		s.kernel.ObserveAdmittedGeneration(x, authority)
	}
	t.answer(ctx, pin, body)
}

func (t turn) route() (*routing.Route, *fault.Error) {
	slug := t.r.PathValue("model")
	if !routeidentity.ValidSlug(slug) {
		return nil, fault.InvalidRequest("invalid_value", "model must name a published route slug.", nil)
	}
	route, ok := t.x.Snapshot().FindRoute(slug)
	if !ok {
		return nil, fault.ModelNotFound(slug)
	}
	t.x.Route = &route
	if t.x.Strict() {
		// A release may have changed between the dispatch wrapper and begin.
		// Never let that race serve strict work through the transformed resource path.
		return nil, fault.InvalidRequest("target_capability", "This interaction requires the compiled generation runner; retry against the current route.", nil)
	}
	if !t.authority.Allows("inference", route.Slug, route.ProjectID, t.kernel.Now()) {
		return nil, fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
	}
	if !slices.Contains(route.Operations, t.operation) {
		return nil, fault.InvalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow this operation.", nil)
	}
	if e := t.x.GuardrailSurfaceGate(&route); e != nil {
		return nil, e
	}
	return &route, nil
}

func (t turn) pin(route *routing.Route) (*kernel.Pin, *fault.Error) {
	return t.kernel.SelectPinSurface(t.r.Context(), t.x, route, t.operation, "bedrock", t.mode, func(provider *routing.Provider, model string) bool {
		return qualified(provider, model, t.operation, t.mode)
	})
}

func (t turn) reserve(route *routing.Route) (context.Context, context.CancelFunc, *fault.Error) {
	overall := time.Duration(route.OverallTimeout) * time.Millisecond
	if e := t.kernel.ReserveState(t.r.Context(), t.x, t.authority, overall); e != nil {
		return nil, nil, e
	}
	ctx, cancel := t.kernel.StateDeadline(t.r.Context(), route)
	if e := t.kernel.ReserveFallbackCost(ctx, t.x); e != nil {
		cancel()
		return nil, nil, e
	}
	return ctx, cancel, nil
}

func (t turn) answer(ctx context.Context, pin *kernel.Pin, body []byte) {
	endpoint := strings.TrimRight(pin.Provider.Endpoint, "/") + "/model/" + url.PathEscape(pin.Provider.Connector().Model(pin.Model)) + "/" + t.upstream
	if _, err := t.kernel.Egress.ValidateEndpoint(endpoint); err != nil {
		t.refuse(fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy."))
		return
	}
	resp, failure := t.kernel.BedrockCall(ctx, t.x, pin, endpoint, body, t.mode == "streaming")
	if failure != nil {
		t.refuse(kernel.UpstreamError(failure))
		return
	}
	defer resp.Body.Close()
	if t.mode == "streaming" {
		t.kernel.RelayBedrockStream(ctx, t.w, t.x, resp)
		return
	}
	result, err := resp.ReadBody(t.kernel.Config.MaxResponseBytes)
	if err != nil {
		t.refuse(fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."))
		return
	}
	if e := t.kernel.ValidateBedrockResult(resp.Exchange, pin, t.upstream, result); e != nil {
		t.refuse(e)
		return
	}
	resp.Exchange.Complete(kernel.ClassSuccess)
	t.writeResult(resp, result)
}

func (t turn) writeResult(resp *kernel.PinnedResponse, result []byte) {
	w, x := t.w, t.x
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	x.ResponseHeaders(w.Header(), false)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	n, writeErr := w.Write(result)
	if writeErr == nil && n == len(result) {
		writeErr = rc.Flush()
	}
	if writeErr != nil || n != len(result) {
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The native result could not be delivered.")
		resp.Exchange.Delivery(kernel.DeliveryState(n))
		t.kernel.Finish(x, &kernel.Outcome{Err: x.Failure, Committed: true, Cancelled: true}, 0)
		return
	}
	x.Delivered(t.kernel.Now())
	resp.Exchange.Delivery(usage.ClientTerminal)
	t.kernel.Finish(x, &kernel.Outcome{Committed: true}, http.StatusOK)
}
