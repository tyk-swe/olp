package anthropicstate

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/kernel"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) listBatches(w http.ResponseWriter, r *http.Request) {
	s.listResources(w, r, resources.KindAnthropicBatch, wire.AnthropicBatch)
}

func (s surface) createBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.begin(w, r, wire.AnthropicBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	body, err := s.kernel.ReadResourceBody(w, r)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	requestedRoute := strings.TrimSpace(r.Header.Get(kernel.StateRouteHeader))
	if requestedRoute == "" {
		requestedRoute, _ = commonModel(body)
	}
	route, err := s.kernel.NativeStateRoute(x, authority, r, requestedRoute)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	if _, _, _, validationErr := validateBatchRequest(body, route.Slug, "", ""); validationErr != nil {
		s.kernel.Refuse(x, w, validationErr)
		return
	}
	p, _, e := s.selectPinWithFiles(r.Context(), x, authority, route, body)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if x.Route != nil {
		route = x.Route
	}
	translated, fileIDs, customIDs, e := validateBatchRequest(body, route.Slug, p.Model, p.Provider.Connector().Model(p.Model))
	if e != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.kernel.Refuse(x, w, e)
		return
	}
	translated, e = s.bindFiles(r.Context(), authority.ID, route.Slug, translated, fileIDs)
	if e != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.kernel.Refuse(x, w, e)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if e = s.kernel.UnboundedPinLimits(x.AdmissionAuthority(authority), p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e = x.GuardrailSurfaceGate(route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e = s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if len(fileIDs) > 0 {
		x.PinnedHeaders = filesHeaders()
	}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if parseErr != nil {
		s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "The request body is invalid.", nil))
		return
	}
	endpoint, urlErr := kernel.ResourceURL(p.Provider.Connector(), p.Model, "messages/batches", nil)
	if urlErr != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."))
		return
	}
	expected := int64(len(customIDs))
	submission, e := s.kernel.BeginBatch(ctx, x, authority.ID, p, kernel.BatchIntent{
		Kind: resources.KindAnthropicBatch, Adapter: connectors.BatchAnthropic, Surface: batches.SurfaceAnthropic,
		Operation: wire.LineGeneration, Window: 24 * time.Hour, Expected: &expected,
		Submission: batches.AnthropicSubmission{}, Source: source, Canonical: source.Bytes(), Effective: translated,
	})
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	id := submission.Resource.UUID
	result, exchange, e := s.kernel.SendBatch(ctx, x, p, id, endpoint, translated)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	view, projectionErr := projectBatch(result, resources.LocalID(resources.KindAnthropicBatch, id), s.kernel.GatewayOrigin(r), p.Model)
	if projectionErr != nil {
		s.kernel.Refuse(x, w, s.kernel.BatchSubmissionUnknown(ctx, id, "The provider returned a malformed batch object. The batch may have been accepted."))
		return
	}
	exchange.Complete(kernel.ClassSuccess)
	if _, e = s.kernel.ActivateBatch(ctx, submission, kernel.AcceptedBatch{UpstreamID: view.upstreamID, State: view.state, Metadata: view.metadata}); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	s.kernel.WriteStateJSON(w, x, view.body, exchange)
}

func (s surface) bindFiles(ctx context.Context, keyID, routeSlug string, body []byte, fileIDs []string) ([]byte, *fault.Error) {
	if len(fileIDs) == 0 {
		return body, nil
	}
	rewrites := map[string]string{}
	for _, id := range fileIDs {
		file, err := s.kernel.Deps.Resources.Get(ctx, resources.KindAnthropicFile, keyID, id)
		if err != nil || file.RouteSlug != routeSlug {
			return nil, fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
		}
		rewrites[id] = file.UpstreamID
	}
	bound, err := replaceFileReferences(body, rewrites)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", "A referenced file could not be bound.", nil)
	}
	return bound, nil
}

func (s surface) selectPinWithFiles(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, route *routing.Route, body []byte) (*kernel.Pin, []*resources.Resource, *fault.Error) {
	refs, refErr := batchFileReferences(body)
	if refErr != nil {
		return nil, nil, fault.InvalidRequest("invalid_request", "The request contains an invalid file source.", nil)
	}
	ids := uniqueStrings(refs)
	files := make([]*resources.Resource, 0, len(ids))
	for _, id := range ids {
		file, err := s.kernel.Deps.Resources.Get(ctx, resources.KindAnthropicFile, authority.ID, id)
		if err != nil || file.RouteSlug != route.Slug {
			return nil, nil, fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		p, err := s.kernel.NativeStatePin(ctx, x, route)
		return p, files, err
	}
	if !sameAffinity(files) {
		return nil, nil, fault.InvalidRequest("invalid_request", "Referenced files must share the same provider affinity.", nil)
	}
	p, pinnedRoute, err := s.kernel.ResolveResource(ctx, x, authority, files[0], "batch", kernel.RetainedNewWork)
	if err != nil || pinnedRoute.Slug != route.Slug {
		return nil, nil, fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
	}
	for _, file := range files[1:] {
		if file.ProviderID != files[0].ProviderID || file.TargetID != files[0].TargetID || file.SlotID != files[0].SlotID || !equalCredentialID(file.CredentialID, files[0].CredentialID) {
			return nil, nil, fault.InvalidRequest("invalid_request", "Referenced files must share the same provider affinity.", nil)
		}
	}
	x.Route = pinnedRoute
	return p, files, nil
}

func (s surface) batchCall(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, op func(context.Context, *kernel.Call, keyauthority.Authority, *resources.Resource, *kernel.Pin) *fault.Error) {
	x, authority, done := s.begin(w, r, wire.AnthropicBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	res, err := s.kernel.Deps.Resources.Get(r.Context(), resources.KindAnthropicBatch, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No message batch with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The batch mapping could not be read."))
		return
	}
	p, route, e := s.kernel.ResolveResource(r.Context(), x, authority, res, "batch", use)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if !p.Provider.Connector().Supports("batch", "anthropic", "unary") || !p.Provider.Supports(p.Model, "batch", "anthropic", "unary") {
		s.kernel.Refuse(x, w, fault.NotFoundError("not_found", "No message batch with this identifier exists for this key."))
		return
	}
	x.Route = route
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if e = s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if e = op(ctx, x, authority, res, p); e != nil {
		s.kernel.Refuse(x, w, e)
	}
}

func (s surface) batchRequest(w http.ResponseWriter, r *http.Request, use kernel.RetainedUse, method string, cancel bool) {
	s.batchCall(w, r, use, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, res *resources.Resource, p *kernel.Pin) *fault.Error {
		path := "messages/batches/" + url.PathEscape(res.UpstreamID)
		if cancel {
			path += "/cancel"
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, path, nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, method, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				return fault.NotFoundError("not_found", "No message batch with this identifier exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		view, err := projectBatch(body, res.ID, s.kernel.GatewayOrigin(r), kernel.ResourceModel(res))
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
		}
		if view.upstreamID != res.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a mismatched batch identity.")
		}
		resp.Exchange.Complete(kernel.ClassSuccess)
		if err := s.kernel.Deps.Resources.Update(ctx, res.ID, view.state, view.metadata, nil); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The batch refresh could not be committed.")
		}

		s.kernel.WriteStateJSON(w, x, view.body, resp.Exchange)
		return nil
	})
}

func (s surface) getBatch(w http.ResponseWriter, r *http.Request) {
	s.batchRequest(w, r, kernel.RetainedRetrieval, http.MethodGet, false)
}

func (s surface) cancelBatch(w http.ResponseWriter, r *http.Request) {
	s.batchRequest(w, r, kernel.RetainedHousekeeping, http.MethodPost, true)
}

func (s surface) deleteBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, kernel.RetainedHousekeeping, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, res *resources.Resource, p *kernel.Pin) *fault.Error {
		if err := s.kernel.RequireBatchEvidenceSettled(ctx, res.UUID); err != nil {
			return err
		}
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "messages/batches/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.kernel.Deps.Resources.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No message batch with this identifier exists for this key.")
			}
			return kernel.UpstreamError(failure)
		}
		body, readErr := resp.ReadBody(s.kernel.Config.MaxResponseBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		resp.Exchange.Complete(kernel.ClassSuccess)
		if err := s.kernel.Deps.Resources.Tombstone(ctx, res.ID); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The batch mapping could not be deleted.")
		}

		view, projectErr := projectBatch(body, res.ID, s.kernel.GatewayOrigin(r), kernel.ResourceModel(res))
		if projectErr != nil {
			s.kernel.WriteStateJSON(w, x, kernel.MustJSON(map[string]string{"id": res.ID, "type": "message_batch_deleted"}), resp.Exchange)
			return nil
		}
		s.kernel.WriteStateJSON(w, x, view.body, resp.Exchange)
		return nil
	})
}

func (s surface) batchResults(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, kernel.RetainedRetrieval, func(ctx context.Context, x *kernel.Call, _ keyauthority.Authority, res *resources.Resource, p *kernel.Pin) *fault.Error {
		endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "messages/batches/"+url.PathEscape(res.UpstreamID)+"/results", nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.kernel.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			return kernel.UpstreamError(failure)
		}
		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/jsonl"
		}
		artifact, err := resp.StageBody(ctx, s.kernel.Deps.Staging, "batch-results", contentType, s.kernel.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The results exceeded the configured response bound.")
		}
		resp.Exchange.Complete(kernel.ClassSuccess)
		defer s.kernel.Deps.Staging.Remove(artifact.Handle)
		opened, err := s.kernel.Deps.Staging.Open(artifact.Handle)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The staged results could not be read.")
		}
		defer opened.File.Close()
		kernel.SetFileDownloadHeaders(w.Header(), contentType, artifact.ContentLength)
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The results response could not be bounded.")
		}

		s.kernel.DeliverResourceContent(context.Background(), w, x, resp.Exchange, opened.File, artifact.ContentLength, "The results response ended before all bytes were delivered.")
		return nil
	})
}
