package kernel

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

var anthropicCustomID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (s *Server) anthropicListBatches(w http.ResponseWriter, r *http.Request) {
	s.anthropicListResources(w, r, resources.KindAnthropicBatch, wire.AnthropicBatch)
}

func (s *Server) anthropicCreateBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.anthropicBegin(w, r, wire.AnthropicBatch)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	body, err := s.ReadResourceBody(w, r)
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	requestedRoute := strings.TrimSpace(r.Header.Get(StateRouteHeader))
	if requestedRoute == "" {
		requestedRoute, _ = anthropicCommonModel(body)
	}
	route, err := s.NativeStateRoute(x, authority, r, requestedRoute)
	if err != nil {
		s.Refuse(x, w, err)
		return
	}
	if _, _, _, validationErr := validateAnthropicBatchRequest(body, route.Slug, "", ""); validationErr != nil {
		s.Refuse(x, w, validationErr)
		return
	}
	p, _, e := s.anthropicSelectPinWithFiles(r.Context(), x, authority, route, body)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	if x.Route != nil {
		route = x.Route
	}
	translated, fileIDs, customIDs, e := validateAnthropicBatchRequest(body, route.Slug, p.Model, p.Provider.Connector().Model(p.Model))
	if e != nil {
		s.ReleaseHold(r.Context(), p.Hold)
		s.Refuse(x, w, e)
		return
	}
	if len(fileIDs) > 0 {
		rewrites := map[string]string{}
		for _, id := range fileIDs {
			file, getErr := s.Deps.Resources.Get(r.Context(), resources.KindAnthropicFile, authority.ID, id)
			if getErr != nil || file.RouteSlug != route.Slug {
				s.ReleaseHold(r.Context(), p.Hold)
				s.Refuse(x, w, fault.NotFoundError("not_found", "No file with this identifier exists for this key."))
				return
			}
			rewrites[id] = file.UpstreamID
		}
		var rewriteErr error
		translated, rewriteErr = replaceAnthropicFileReferences(translated, rewrites)
		if rewriteErr != nil {
			s.ReleaseHold(r.Context(), p.Hold)
			s.Refuse(x, w, fault.InvalidRequest("invalid_request", "A referenced file could not be bound.", nil))
			return
		}
	}
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if e = s.UnboundedPinLimits(x.AdmissionAuthority(authority), p, route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	if e = x.GuardrailSurfaceGate(route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	if e = s.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	if len(fileIDs) > 0 {
		x.PinnedHeaders = anthropicFilesHeaders()
	}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if parseErr != nil {
		s.Refuse(x, w, fault.InvalidRequest("invalid_request", "The request body is invalid.", nil))
		return
	}
	endpoint, urlErr := ResourceURL(p.Provider.Connector(), p.Model, "messages/batches", nil)
	if urlErr != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."))
		return
	}
	expected := int64(len(customIDs))
	submission, e := s.BeginBatch(ctx, x, authority.ID, p, BatchIntent{
		Kind: resources.KindAnthropicBatch, Adapter: connectors.BatchAnthropic, Surface: batches.SurfaceAnthropic,
		Operation: wire.LineGeneration, Window: 24 * time.Hour, Expected: &expected,
		Submission: batches.AnthropicSubmission{}, Source: source, Canonical: source.Bytes(), Effective: translated,
	})
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	id := submission.Resource.UUID
	result, exchange, e := s.SendBatch(ctx, x, p, id, endpoint, translated)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	view, projectionErr := projectAnthropicBatch(result, resources.LocalID(resources.KindAnthropicBatch, id), s.GatewayOrigin(r), p.Model)
	if projectionErr != nil {
		s.Refuse(x, w, s.BatchSubmissionUnknown(ctx, id, "The provider returned a malformed batch object. The batch may have been accepted."))
		return
	}
	exchange.Complete(ClassSuccess)
	if _, e = s.ActivateBatch(ctx, submission, AcceptedBatch{UpstreamID: view.upstreamID, State: view.state, Metadata: view.metadata}); e != nil {
		s.Refuse(x, w, e)
		return
	}
	s.WriteStateJSON(w, x, view.body, exchange)
}

func anthropicCommonModel(body []byte) (string, bool) {
	var object struct {
		Requests []struct {
			Params struct {
				Model string `json:"model"`
			} `json:"params"`
		} `json:"requests"`
	}
	if json.Unmarshal(body, &object) != nil || len(object.Requests) == 0 {
		return "", false
	}
	model := object.Requests[0].Params.Model
	for _, request := range object.Requests[1:] {
		if request.Params.Model != model {
			return "", false
		}
	}
	return model, model != ""
}

func (s *Server) anthropicSelectPinWithFiles(ctx context.Context, x *Call, authority keyauthority.Authority, route *routing.Route, body []byte) (*Pin, []*resources.Resource, *fault.Error) {
	refs, refErr := anthropicBatchFileReferences(body)
	if refErr != nil {
		return nil, nil, fault.InvalidRequest("invalid_request", "The request contains an invalid file source.", nil)
	}
	ids := uniqueStrings(refs)
	files := make([]*resources.Resource, 0, len(ids))
	for _, id := range ids {
		file, err := s.Deps.Resources.Get(ctx, resources.KindAnthropicFile, authority.ID, id)
		if err != nil || file.RouteSlug != route.Slug {
			return nil, nil, fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		p, err := s.NativeStatePin(ctx, x, route)
		return p, files, err
	}
	if !sameAnthropicAffinity(files) {
		return nil, nil, fault.InvalidRequest("invalid_request", "Referenced files must share the same provider affinity.", nil)
	}
	p, pinnedRoute, err := s.ResolveResource(ctx, x, authority, files[0], "batch", RetainedNewWork)
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

func anthropicBatchFileReferences(body []byte) ([]string, error) {
	var request struct {
		Requests []anthropicBatchRequest `json:"requests"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	refs := []string{}
	for _, item := range request.Requests {
		itemRefs, err := anthropicFileReferences(item.Params)
		if err != nil {
			return nil, err
		}
		refs = append(refs, itemRefs...)
	}
	return uniqueStrings(refs), nil
}

func (s *Server) anthropicBatchCall(w http.ResponseWriter, r *http.Request, use RetainedUse, op func(context.Context, *Call, keyauthority.Authority, *resources.Resource, *Pin) *fault.Error) {
	x, authority, done := s.anthropicBegin(w, r, wire.AnthropicBatch)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	res, err := s.Deps.Resources.Get(r.Context(), resources.KindAnthropicBatch, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No message batch with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The batch mapping could not be read."))
		return
	}
	p, route, e := s.ResolveResource(r.Context(), x, authority, res, "batch", use)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	if !p.Provider.Connector().Supports("batch", "anthropic", "unary") || !p.Provider.Supports(p.Model, "batch", "anthropic", "unary") {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No message batch with this identifier exists for this key."))
		return
	}
	x.Route = route
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if e = s.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	if e = op(ctx, x, authority, res, p); e != nil {
		s.Refuse(x, w, e)
	}
}

func (s *Server) anthropicBatchRequest(w http.ResponseWriter, r *http.Request, use RetainedUse, method string, cancel bool) {
	s.anthropicBatchCall(w, r, use, func(ctx context.Context, x *Call, _ keyauthority.Authority, res *resources.Resource, p *Pin) *fault.Error {
		path := "messages/batches/" + url.PathEscape(res.UpstreamID)
		if cancel {
			path += "/cancel"
		}
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, path, nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, method, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				return fault.NotFoundError("not_found", "No message batch with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := resp.ReadBody(s.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		view, err := projectAnthropicBatch(body, res.ID, s.GatewayOrigin(r), ResourceModel(res))
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
		}
		if view.upstreamID != res.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a mismatched batch identity.")
		}
		resp.Exchange.Complete(ClassSuccess)
		if err := s.Deps.Resources.Update(ctx, res.ID, view.state, view.metadata, nil); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The batch refresh could not be committed.")
		}

		s.WriteStateJSON(w, x, view.body, resp.Exchange)
		return nil
	})
}

func (s *Server) anthropicGetBatch(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchRequest(w, r, RetainedRetrieval, http.MethodGet, false)
}

func (s *Server) anthropicCancelBatch(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchRequest(w, r, RetainedHousekeeping, http.MethodPost, true)
}

func (s *Server) anthropicDeleteBatch(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchCall(w, r, RetainedHousekeeping, func(ctx context.Context, x *Call, _ keyauthority.Authority, res *resources.Resource, p *Pin) *fault.Error {
		if err := s.RequireBatchEvidenceSettled(ctx, res.UUID); err != nil {
			return err
		}
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, "messages/batches/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				_ = s.Deps.Resources.Tombstone(ctx, res.ID)
				return fault.NotFoundError("not_found", "No message batch with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		body, readErr := resp.ReadBody(s.Config.MaxResponseBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		resp.Exchange.Complete(ClassSuccess)
		if err := s.Deps.Resources.Tombstone(ctx, res.ID); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The batch mapping could not be deleted.")
		}

		view, projectErr := projectAnthropicBatch(body, res.ID, s.GatewayOrigin(r), ResourceModel(res))
		if projectErr != nil {
			s.WriteStateJSON(w, x, MustJSON(map[string]string{"id": res.ID, "type": "message_batch_deleted"}), resp.Exchange)
			return nil
		}
		s.WriteStateJSON(w, x, view.body, resp.Exchange)
		return nil
	})
}

func (s *Server) anthropicBatchResults(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, _ keyauthority.Authority, res *resources.Resource, p *Pin) *fault.Error {
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, "messages/batches/"+url.PathEscape(res.UpstreamID)+"/results", nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			return UpstreamError(failure)
		}
		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/jsonl"
		}
		artifact, err := resp.StageBody(ctx, s.Deps.Staging, "batch-results", contentType, s.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The results exceeded the configured response bound.")
		}
		resp.Exchange.Complete(ClassSuccess)
		defer s.Deps.Staging.Remove(artifact.Handle)
		opened, err := s.Deps.Staging.Open(artifact.Handle)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The staged results could not be read.")
		}
		defer opened.File.Close()
		SetFileDownloadHeaders(w.Header(), contentType, artifact.ContentLength)
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The results response could not be bounded.")
		}

		x.ResponseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := CopyResourceContent(w, opened.File, artifact.ContentLength)
		if copyErr != nil || written != artifact.ContentLength {
			resp.Exchange.Delivery(DeliveryState(written))
			x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The results response ended before all bytes were delivered.")
			s.Finish(x, &Outcome{Err: x.Failure, Committed: true, Cancelled: true}, x.Failure.Status)
			return nil
		}
		if written > 0 {
			x.Delivered(s.Now())
		}
		resp.Exchange.Delivery(usage.ClientTerminal)
		s.Finish(x, &Outcome{Committed: true}, http.StatusOK)
		return nil
	})
}

type anthropicBatchRequest struct {
	CustomID string          `json:"custom_id"`
	Params   json.RawMessage `json:"params"`
}

func validateAnthropicBatchRequest(body []byte, routeSlug, targetModel, boundModel string) ([]byte, []string, []string, *fault.Error) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil || source.Root().Kind() != oif.Object || len(source.Root().Members()) != 1 || source.Root().Members()[0].Name != "requests" {
		return nil, nil, nil, fault.InvalidRequest("invalid_request", "The request body must contain only requests.", nil)
	}
	requestsValue, present := source.Root().Lookup("requests")
	elements := requestsValue.Elements()
	if !present || requestsValue.Kind() != oif.Array || len(elements) < 1 || len(elements) > 100000 {
		return nil, nil, nil, fault.InvalidRequest("invalid_request", "requests must contain between 1 and 100000 entries.", nil)
	}
	var requests []anthropicBatchRequest
	if json.Unmarshal(body, &struct {
		Requests *[]anthropicBatchRequest `json:"requests"`
	}{Requests: &requests}) != nil || len(requests) != len(elements) {
		return nil, nil, nil, fault.InvalidRequest("invalid_request", "Every request must contain custom_id and params.", nil)
	}
	seen := make(map[string]struct{}, len(requests))
	fileIDs := []string{}
	model := routeSlug
	for i, request := range requests {
		members := elements[i].Members()
		if elements[i].Kind() != oif.Object || len(members) != 2 {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d] must contain only custom_id and params.", i), nil)
		}
		memberNames := map[string]bool{}
		for _, member := range members {
			memberNames[member.Name] = true
		}
		if !memberNames["custom_id"] || !memberNames["params"] {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d] must contain custom_id and params.", i), nil)
		}
		if !anthropicCustomID.MatchString(request.CustomID) {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].custom_id is invalid.", i), nil)
		}
		if _, exists := seen[request.CustomID]; exists {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].custom_id is duplicated.", i), nil)
		}
		seen[request.CustomID] = struct{}{}
		params, parseErr := oif.ParseJSON(request.Params, oif.Limits{MaxBytes: len(request.Params)})
		if parseErr != nil || params.Root().Kind() != oif.Object {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params must be an object.", i), nil)
		}
		if stream, ok := params.Root().Lookup("stream"); ok && stream.Raw() == "true" {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params.stream is unsupported.", i), nil)
		}
		modelValue, hasModel := params.Root().Lookup("model")
		requestModel, _ := modelValue.Text()
		parseModel := routeSlug
		if parseModel == "" {
			parseModel = requestModel
		}
		if parsed, parseErr := protocols.Parse(wire.Anthropic, request.Params, parseModel); parseErr != nil || parsed == nil {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params is invalid.", i), nil)
		}
		if routeSlug == "" {
			if !hasModel || requestModel == "" || i > 0 && model != requestModel {
				return nil, nil, nil, fault.InvalidRequest("invalid_request", "A single common params.model or X-OLP-Route is required.", nil)
			}
			model = requestModel
		}
		if requestModel == "" || targetModel != "" && requestModel != routeSlug && requestModel != targetModel {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params.model does not match the route.", i), nil)
		}
		refs, refErr := anthropicFileReferences(request.Params)
		if refErr != nil {
			return nil, nil, nil, fault.InvalidRequest("invalid_request", fmt.Sprintf("requests[%d].params contains an invalid file source.", i), nil)
		}
		fileIDs = append(fileIDs, refs...)
	}
	bound, err := bindAnthropicBatchModel(requests, cmp.Or(boundModel, targetModel))
	if err != nil {
		return nil, nil, nil, fault.InvalidRequest("invalid_request", "Every request params must be an object.", nil)
	}
	return MustJSON(struct {
		Requests []anthropicBatchRequest `json:"requests"`
	}{Requests: bound}), uniqueStrings(fileIDs), requestsCustomIDs(requests), nil
}

func bindAnthropicBatchModel(requests []anthropicBatchRequest, model string) ([]anthropicBatchRequest, error) {
	bound := make([]anthropicBatchRequest, len(requests))
	for i, request := range requests {
		var params map[string]json.RawMessage
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, err
		}
		if model != "" {
			params["model"] = MustJSON(model)
		}
		bound[i] = anthropicBatchRequest{CustomID: request.CustomID, Params: MustJSON(params)}
	}
	return bound, nil
}

func requestsCustomIDs(requests []anthropicBatchRequest) []string {
	out := make([]string, len(requests))
	for i := range requests {
		out[i] = requests[i].CustomID
	}
	return out
}

type anthropicBatchView struct {
	body       []byte
	upstreamID string
	state      string
	metadata   json.RawMessage
}

var errMalformedAnthropicBatch = errors.New("malformed Anthropic batch")

func projectAnthropicBatch(body []byte, localID, origin, model string) (anthropicBatchView, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return anthropicBatchView{}, errMalformedAnthropicBatch
	}
	var fields struct {
		ID               string  `json:"id"`
		ProcessingStatus string  `json:"processing_status"`
		ResultsURL       *string `json:"results_url"`
	}
	if json.Unmarshal(body, &fields) != nil || fields.ID == "" || len(fields.ID) > 512 {
		return anthropicBatchView{}, errMalformedAnthropicBatch
	}
	if fields.ResultsURL != nil {
		if _, err := url.Parse(*fields.ResultsURL); err != nil {
			return anthropicBatchView{}, errMalformedAnthropicBatch
		}
	}
	projected := maps.Clone(object)
	projected["id"] = MustJSON(localID)
	if fields.ResultsURL != nil {
		projected["results_url"] = MustJSON(origin + "/anthropic/v1/messages/batches/" + url.PathEscape(localID) + "/results")
	}
	stored := maps.Clone(projected)
	delete(stored, "id")
	stored["upstream_model"] = MustJSON(model)
	return anthropicBatchView{body: MustJSON(projected), upstreamID: fields.ID, state: fields.ProcessingStatus, metadata: MustJSON(stored)}, nil
}
