package cloudbatches

import (
	"context"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) create(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.CloudBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	dialect, slug := r.PathValue("dialect"), strings.TrimSpace(r.Header.Get(kernel.StateRouteHeader))
	route, e := batchRoute(x, authority, slug, s.kernel.Now())
	if e != nil {
		s.fail(x, w, e)
		return
	}
	x.Route = route
	body, e := s.kernel.ReadResourceBody(w, r)
	if e != nil {
		s.fail(x, w, e)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	p, adapter, input, e := s.target(ctx, r, x, authority, route, body)
	if e != nil {
		s.fail(x, w, e)
		return
	}
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); err != nil {
		s.fail(x, w, err)
		return
	}
	source, err := ingress.ParseObjectRequest(body, oif.Limits{MaxBytes: int(s.kernel.Config.MaxBodyBytes)})
	if err != nil {
		s.fail(x, w, fault.InvalidRequest("invalid_json", "The request body must be one JSON object.", nil))
		return
	}
	plan, err := planJob(p, adapter, input)
	if err != nil {
		s.fail(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The cloud batch control-plane URL could not be resolved."))
		return
	}
	submission, e := s.kernel.BeginBatch(ctx, x, authority.ID, p, kernel.BatchIntent{
		ResourceID: plan.id, Kind: resources.KindCloudBatch, Adapter: adapter, Surface: batches.SurfaceNative,
		Operation: wire.LineGeneration, Window: plan.window, Submission: plan.submission,
		Source: source, Canonical: source.Bytes(), Effective: plan.body,
	})
	if e != nil {
		s.fail(x, w, e)
		return
	}
	result, exchange, e := s.kernel.SendBatch(ctx, x, p, plan.id, plan.endpoint, plan.body)
	if e != nil {
		s.fail(x, w, e)
		return
	}
	accepted, err := acceptedJob(adapter, result, s.kernel.Now())
	if err != nil {
		s.fail(x, w, s.kernel.BatchSubmissionUnknown(ctx, plan.id, "The cloud provider returned an invalid batch job; the job may have been accepted."))
		return
	}
	job, err := upstream.DecodeCloudJob(adapter, accepted)
	if err != nil {
		s.fail(x, w, s.kernel.BatchSubmissionUnknown(ctx, plan.id, "The cloud provider returned an invalid batch job; the job may have been accepted."))
		return
	}
	record := upstream.CloudRecord{
		Dialect: dialect, RouteModel: slug, UpstreamModel: p.Model,
		InputURI: input.InputURI, OutputURIPrefix: input.OutputURIPrefix, ProviderJob: accepted,
	}
	exchange.Complete(kernel.ClassSuccess)
	stored, e := s.kernel.ActivateBatch(ctx, submission, kernel.AcceptedBatch{UpstreamID: job.ID, State: string(job.Status), Metadata: kernel.MustJSON(record)})
	if e != nil {
		s.fail(x, w, e)
		return
	}
	s.kernel.WriteStateJSON(w, x, project(job, stored.ID, record), exchange)
}

func batchRoute(x *kernel.Call, authority keyauthority.Authority, slug string, now time.Time) (*routing.Route, *fault.Error) {
	if slug == "" {
		return nil, fault.InvalidRequest("missing_required_parameter", "X-OLP-Route must name a batch-enabled route.", nil)
	}
	route, ok := x.Snapshot().FindRoute(slug)
	if !ok || !slices.Contains(route.Operations, "batch") {
		return nil, fault.NotFoundError("not_found", "No matching batch route exists.")
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, now) {
		return nil, fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
	}
	return &route, nil
}

func (s surface) target(ctx context.Context, r *http.Request, x *kernel.Call, authority keyauthority.Authority, route *routing.Route, body []byte) (*kernel.Pin, connectors.BatchAdapter, request, *fault.Error) {
	dialect := r.PathValue("dialect")
	p, e := s.kernel.SelectPinSurface(ctx, x, route, "batch", "native", "unary", func(provider *routing.Provider, model string) bool {
		cfg := provider.Connector()
		return provider.ProfileID == dialect && cfg.Supports("batch", "native", "unary") && cfg.BatchStorage != nil
	})
	if e != nil {
		return nil, "", request{}, e
	}
	adapter, input, e := s.accepts(x, authority, p, route, dialect, body)
	if e != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		return nil, "", request{}, e
	}
	return p, adapter, input, nil
}

func (s surface) accepts(x *kernel.Call, authority keyauthority.Authority, p *kernel.Pin, route *routing.Route, dialect string, body []byte) (connectors.BatchAdapter, request, *fault.Error) {
	adapter, cloud := connectors.CloudBatchAdapter(dialect)
	if p.Provider.ProfileID != dialect || !cloud {
		return "", request{}, fault.NotFoundError("not_found", "No matching cloud batch dialect is available.")
	}
	if x.Strict() {
		return "", request{}, kernel.PolicyUnavailable("target_capability", "Cloud batch jobs require a transformed route.")
	}
	input, e := validate(body, adapter, p.Provider.BatchStorage)
	if e != nil {
		return "", request{}, e
	}
	if e := s.kernel.UnboundedPinLimits(x.AdmissionAuthority(authority), p, route); e != nil {
		return "", request{}, e
	}
	if e := x.GuardrailSurfaceGate(route); e != nil {
		return "", request{}, e
	}
	return adapter, input, nil
}
