package cloudbatches

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"net/http"

	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) control(w http.ResponseWriter, r *http.Request, cancel bool) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.CloudBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	res, record, adapter, e := s.stored(r, authority)
	if e != nil {
		s.fail(x, w, e)
		return
	}
	p, route, e := s.pin(r, x, authority, res, record.Dialect, cancel)
	if e != nil {
		s.fail(x, w, e)
		return
	}
	ctx, stop := s.kernel.StateDeadline(r.Context(), route)
	defer stop()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); err != nil {
		s.fail(x, w, err)
		return
	}
	cfg := p.Provider.Connector()
	if cancel {
		if e := s.stop(ctx, x, p, cfg, adapter, res.UpstreamID); e != nil {
			s.fail(x, w, e)
			return
		}
	}
	endpoint, urlErr := controlURL(cfg, adapter, res.UpstreamID, "")
	if urlErr != nil {
		s.fail(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The cloud control-plane URL could not be resolved."))
		return
	}
	response, attemptErr := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if attemptErr != nil {
		s.fail(x, w, kernel.UpstreamError(attemptErr))
		return
	}
	defer response.Body.Close()
	body, err := response.ReadBody(s.kernel.Config.MaxResponseBytes)
	if err != nil {
		s.fail(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The cloud job response could not be read."))
		return
	}
	job, err := upstream.DecodeCloudJob(adapter, body)
	if errors.Is(err, upstream.ErrProviderConflict) {
		s.fail(x, w, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The cloud provider returned an unknown job status."))
		return
	}
	if err != nil || job.ID != res.UpstreamID {
		s.fail(x, w, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The cloud provider returned a mismatched job identity."))
		return
	}
	response.Exchange.Complete(kernel.ClassSuccess)
	refreshed := record.WithJob(body)
	commit, release := kernel.ResourceCommitContext(ctx)
	defer release()
	if err := s.store.Update(commit, res.ID, string(job.Status), kernel.MustJSON(refreshed), nil); err != nil {
		s.fail(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The cloud job refresh could not be committed."))
		return
	}
	s.kernel.WriteStateJSON(w, x, project(job, res.ID, refreshed), response.Exchange)
}

func (s surface) stored(r *http.Request, authority keyauthority.Authority) (*resources.Resource, upstream.CloudRecord, connectors.BatchAdapter, *fault.Error) {
	res, err := s.store.Get(r.Context(), resources.KindCloudBatch, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		return nil, upstream.CloudRecord{}, "", fault.NotFoundError("not_found", "No cloud batch job with this ID exists for this key.")
	}
	if err != nil {
		return nil, upstream.CloudRecord{}, "", fault.ServerError(http.StatusInternalServerError, "internal_error", "The cloud batch mapping could not be read.")
	}
	record, recordErr := upstream.DecodeCloudRecord(res.Metadata)
	adapter, cloud := connectors.CloudBatchAdapter(record.Dialect)
	if recordErr != nil || !cloud {
		return nil, upstream.CloudRecord{}, "", fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The stored cloud batch mapping is malformed.")
	}
	if record.Dialect != r.PathValue("dialect") {
		return nil, upstream.CloudRecord{}, "", fault.NotFoundError("not_found", "No cloud batch job with this ID exists for this dialect.")
	}
	return res, record, adapter, nil
}

func (s surface) pin(r *http.Request, x *kernel.Call, authority keyauthority.Authority, res *resources.Resource, dialect string, cancel bool) (*kernel.Pin, *routing.Route, *fault.Error) {
	use := kernel.RetainedHousekeeping
	if !cancel {
		candidate, exists := x.Snapshot().FindRoute(res.RouteSlug)
		if !exists {
			return nil, nil, fault.NotFoundError("not_found", "The cloud batch route is unavailable.")
		}
		x.Route, use = &candidate, kernel.RetainedRetrieval
	}
	p, route, e := s.kernel.ResolveResource(r.Context(), x, authority, res, "batch", use)
	if e != nil {
		return nil, nil, e
	}
	if p.Provider.ProfileID != dialect {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		return nil, nil, fault.NotFoundError("not_found", "No cloud batch job with this ID exists for this dialect.")
	}
	x.Route = route
	if e := admissible(x, route, cancel); e != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		return nil, nil, e
	}
	return p, route, nil
}

func admissible(x *kernel.Call, route *routing.Route, cancel bool) *fault.Error {
	if route.Fidelity.Strict() {
		return kernel.PolicyUnavailable("target_capability", "Cloud batch jobs require a transformed route.")
	}
	if cancel {
		return nil
	}
	return x.GuardrailSurfaceGate(route)
}

func (s surface) stop(ctx context.Context, x *kernel.Call, p *kernel.Pin, cfg connectors.Config, adapter connectors.BatchAdapter, upstreamID string) *fault.Error {
	endpoint, err := controlURL(cfg, adapter, upstreamID, cancelAction(adapter))
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The cloud control-plane URL could not be resolved.")
	}
	response, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodPost, endpoint, []byte(`{}`), "application/json")
	if failure != nil {
		return kernel.UpstreamError(failure)
	}
	_ = response.Body.Close()
	response.Exchange.Complete(kernel.ClassSuccess)
	return nil
}

func (s surface) get(w http.ResponseWriter, r *http.Request) {
	s.control(w, r, false)
}

func (s surface) cancel(w http.ResponseWriter, r *http.Request) {
	s.control(w, r, true)
}

func (s surface) list(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.CloudBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	limit, after, err := kernel.ListArgs(r)
	if err != nil {
		s.fail(x, w, err)
		return
	}
	dialect := r.PathValue("dialect")
	if _, cloud := connectors.CloudBatchAdapter(dialect); !cloud {
		s.fail(x, w, fault.NotFoundError("not_found", "Unknown cloud batch dialect."))
		return
	}
	filtered, continuation, listErr := s.kernel.ListedResourcePage(authority.ID+"/"+dialect, "after", limit, after, func(cursor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error) {
		batch, err := s.store.ListCloudBatches(r.Context(), authority.ID, dialect, count, cursor)
		if errors.Is(err, resources.ErrNotFound) {
			return nil, fault.InvalidRequest("invalid_cursor", "after must identify a local cloud batch job.", new("after"))
		}
		if err != nil {
			return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "Cloud batches could not be listed.")
		}
		return batch, nil
	}, func(row *resources.Resource) *fault.Error {
		return s.kernel.AuthorizeListedResource(r.Context(), authority, row)
	})
	if listErr != nil {
		s.fail(x, w, listErr)
		return
	}
	data := make([]json.RawMessage, 0, len(filtered))
	for _, row := range filtered {
		item, itemErr := listed(row)
		if itemErr != nil {
			s.fail(x, w, itemErr)
			return
		}
		data = append(data, item)
	}
	var first, last any
	if len(filtered) > 0 {
		first, last = filtered[0].ID, filtered[len(filtered)-1].ID
	}
	if continuation != "" {
		last = continuation
	}
	s.kernel.WriteStateJSON(w, x, kernel.MustJSON(map[string]any{"object": "list", "data": data, "has_more": continuation != "", "first_id": first, "last_id": last}))
}
