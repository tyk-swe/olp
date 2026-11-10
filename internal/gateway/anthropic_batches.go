package gateway

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
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
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
	defer s.release(r.Context())
	body, err := s.readResourceBody(w, r)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	requestedRoute := strings.TrimSpace(r.Header.Get(stateRouteHeader))
	if requestedRoute == "" {
		requestedRoute, _ = anthropicCommonModel(body)
	}
	route, err := s.anthropicRoute(x, authority, r, requestedRoute)
	if err != nil {
		s.stateFail(x, w, err, x.family)
		return
	}
	if _, _, _, validationErr := validateAnthropicBatchRequest(body, route.Slug, "", ""); validationErr != nil {
		s.stateFail(x, w, validationErr, x.family)
		return
	}
	p, _, e := s.anthropicSelectPinWithFiles(r.Context(), x, authority, route, body)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if x.route != nil {
		route = x.route
	}
	translated, fileIDs, customIDs, e := validateAnthropicBatchRequest(body, route.Slug, p.model, p.provider.Connector().Model(p.model))
	if e != nil {
		s.releaseHold(r.Context(), p.hold)
		s.stateFail(x, w, e, x.family)
		return
	}
	if len(fileIDs) > 0 {
		rewrites := map[string]string{}
		for _, id := range fileIDs {
			file, getErr := s.deps.Resources.Get(r.Context(), resources.KindAnthropicFile, authority.ID, id)
			if getErr != nil || file.RouteSlug != route.Slug {
				s.releaseHold(r.Context(), p.hold)
				s.stateFail(x, w, notFoundError("not_found", "No file with this identifier exists for this key."), x.family)
				return
			}
			rewrites[id] = file.UpstreamID
		}
		var rewriteErr error
		translated, rewriteErr = replaceAnthropicFileReferences(translated, rewrites)
		if rewriteErr != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("invalid_request", "A referenced file could not be bound.", nil), x.family)
			return
		}
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e = s.unboundedPinLimits(x.admissionAuthority(authority), p, route); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if e = x.guardrailSurfaceGate(route); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if e = s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if p.hold == nil {
		gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
		if gate.verdict != gateAdmitted {
			s.stateFail(x, w, gateError(gate), x.family)
			return
		}
		p.hold = gate.hold
	}
	if len(fileIDs) > 0 {
		x.anthropicFilesBeta = true
	}
	source, parseErr := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if parseErr != nil {
		s.stateFail(x, w, invalidRequest("invalid_request", "The request body is invalid.", nil), x.family)
		return
	}
	endpoint, urlErr := resourceURL(p.provider.Connector(), p.model, "messages/batches", nil)
	if urlErr != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."), x.family)
		return
	}
	expected := int64(len(customIDs))
	submission, e := s.beginBatch(ctx, x, authority.ID, p, batchIntent{
		kind: resources.KindAnthropicBatch, adapter: connectors.BatchAnthropic, surface: batches.SurfaceAnthropic,
		operation: wire.LineGeneration, window: 24 * time.Hour, expected: &expected,
		submission: batches.AnthropicSubmission{}, source: source, canonical: source.Bytes(), effective: translated,
	})
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	id := submission.resource.UUID
	result, e := s.sendBatch(ctx, x, p, id, endpoint, translated)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	view, projectionErr := projectAnthropicBatch(result, resources.LocalID(resources.KindAnthropicBatch, id), s.gatewayOrigin(r), p.model)
	if projectionErr != nil {
		s.stateFail(x, w, s.batchSubmissionUnknown(ctx, id, "The provider returned a malformed batch object. The batch may have been accepted."), x.family)
		return
	}
	if _, e = s.activateBatch(ctx, submission, acceptedBatch{upstreamID: view.upstreamID, state: view.state, metadata: view.metadata}); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	x.completePinned()
	s.writeStateJSON(w, x, view.body)
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

func (s *Server) anthropicSelectPinWithFiles(ctx context.Context, x *call, authority keyauthority.Authority, route *routing.Route, body []byte) (*pin, []*resources.Resource, *Error) {
	refs, refErr := anthropicBatchFileReferences(body)
	if refErr != nil {
		return nil, nil, invalidRequest("invalid_request", "The request contains an invalid file source.", nil)
	}
	ids := uniqueStrings(refs)
	files := make([]*resources.Resource, 0, len(ids))
	for _, id := range ids {
		file, err := s.deps.Resources.Get(ctx, resources.KindAnthropicFile, authority.ID, id)
		if err != nil || file.RouteSlug != route.Slug {
			return nil, nil, notFoundError("not_found", "No file with this identifier exists for this key.")
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		p, err := s.anthropicPin(ctx, x, route)
		return p, files, err
	}
	if !sameAnthropicAffinity(files) {
		return nil, nil, invalidRequest("invalid_request", "Referenced files must share the same provider affinity.", nil)
	}
	p, pinnedRoute, err := s.resolveResource(ctx, x, authority, files[0], "batch", retainedNewWork)
	if err != nil || pinnedRoute.Slug != route.Slug {
		return nil, nil, notFoundError("not_found", "No file with this identifier exists for this key.")
	}
	for _, file := range files[1:] {
		if file.ProviderID != files[0].ProviderID || file.TargetID != files[0].TargetID || file.SlotID != files[0].SlotID || !equalCredentialID(file.CredentialID, files[0].CredentialID) {
			return nil, nil, invalidRequest("invalid_request", "Referenced files must share the same provider affinity.", nil)
		}
	}
	x.route = pinnedRoute
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

func (s *Server) anthropicBatchCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, keyauthority.Authority, *resources.Resource, *pin) *Error) {
	x, authority, done := s.anthropicBegin(w, r, wire.AnthropicBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	res, err := s.deps.Resources.Get(r.Context(), resources.KindAnthropicBatch, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No message batch with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The batch mapping could not be read."), x.family)
		return
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, res, "batch", use)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if !p.provider.Connector().Supports("batch", "anthropic", "unary") || !p.provider.Supports(p.model, "batch", "anthropic", "unary") {
		s.stateFail(x, w, notFoundError("not_found", "No message batch with this identifier exists for this key."), x.family)
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e = s.reserveStateAndSlot(ctx, x, authority, p); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if e = op(ctx, x, authority, res, p); e != nil {
		s.stateFail(x, w, e, x.family)
	}
}

func (s *Server) anthropicBatchRequest(w http.ResponseWriter, r *http.Request, use retainedUse, method string, cancel bool) {
	s.anthropicBatchCall(w, r, use, func(ctx context.Context, x *call, _ keyauthority.Authority, res *resources.Resource, p *pin) *Error {
		path := "messages/batches/" + url.PathEscape(res.UpstreamID)
		if cancel {
			path += "/cancel"
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, path, nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, method, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				return notFoundError("not_found", "No message batch with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		body, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		view, err := projectAnthropicBatch(body, res.ID, s.gatewayOrigin(r), resourceModel(res))
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
		}
		if view.upstreamID != res.UpstreamID {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a mismatched batch identity.")
		}
		if err := s.deps.Resources.Update(ctx, res.ID, view.state, view.metadata, nil); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The batch refresh could not be committed.")
		}
		x.dispatched = true
		x.completePinned()
		s.writeStateJSON(w, x, view.body)
		return nil
	})
}

func (s *Server) anthropicGetBatch(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchRequest(w, r, retainedRetrieval, http.MethodGet, false)
}

func (s *Server) anthropicCancelBatch(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchRequest(w, r, retainedHousekeeping, http.MethodPost, true)
}

func (s *Server) anthropicDeleteBatch(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, _ keyauthority.Authority, res *resources.Resource, p *pin) *Error {
		if err := s.requireBatchEvidenceSettled(ctx, res.UUID); err != nil {
			return err
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "messages/batches/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.deps.Resources.Tombstone(ctx, res.ID)
				return notFoundError("not_found", "No message batch with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		body, readErr := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		_ = resp.Body.Close()
		if readErr != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		if err := s.deps.Resources.Tombstone(ctx, res.ID); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The batch mapping could not be deleted.")
		}
		x.dispatched = true
		x.completePinned()
		view, projectErr := projectAnthropicBatch(body, res.ID, s.gatewayOrigin(r), resourceModel(res))
		if projectErr != nil {
			s.writeStateJSON(w, x, mustJSON(map[string]string{"id": res.ID, "type": "message_batch_deleted"}))
			return nil
		}
		s.writeStateJSON(w, x, view.body)
		return nil
	})
}

func (s *Server) anthropicBatchResults(w http.ResponseWriter, r *http.Request) {
	s.anthropicBatchCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, _ keyauthority.Authority, res *resources.Resource, p *pin) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "messages/batches/"+url.PathEscape(res.UpstreamID)+"/results", nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/jsonl"
		}
		artifact, err := s.deps.Staging.Put(ctx, staging.Upload{Filename: "batch-results", ContentType: contentType, MaximumLength: s.cfg.MaxResponseBytes, Body: resp.Body})
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_response_too_large", "The results exceeded the configured response bound.")
		}
		defer s.deps.Staging.Remove(artifact.Handle)
		opened, err := s.deps.Staging.Open(artifact.Handle)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The staged results could not be read.")
		}
		defer opened.File.Close()
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", fmt.Sprint(artifact.ContentLength))
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
			return serverError(http.StatusInternalServerError, "internal_error", "The results response could not be bounded.")
		}
		x.completePinned()
		x.responseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := copyResourceContent(w, opened.File, artifact.ContentLength)
		if copyErr != nil || written != artifact.ContentLength {
			if len(x.facts) > 0 {
				x.facts[len(x.facts)-1].Class = classCancelled
			}
			x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The results response ended before all bytes were delivered.")
			s.finish(x, &outcome{err: x.failure, committed: true, cancelled: true}, x.failure.Status)
			return nil
		}
		if written > 0 {
			x.delivered(s.now())
		}
		s.finish(x, &outcome{committed: true}, http.StatusOK)
		return nil
	})
}

type anthropicBatchRequest struct {
	CustomID string          `json:"custom_id"`
	Params   json.RawMessage `json:"params"`
}

func validateAnthropicBatchRequest(body []byte, routeSlug, targetModel, boundModel string) ([]byte, []string, []string, *Error) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil || source.Root().Kind() != oif.Object || len(source.Root().Members()) != 1 || source.Root().Members()[0].Name != "requests" {
		return nil, nil, nil, invalidRequest("invalid_request", "The request body must contain only requests.", nil)
	}
	requestsValue, present := source.Root().Lookup("requests")
	elements := requestsValue.Elements()
	if !present || requestsValue.Kind() != oif.Array || len(elements) < 1 || len(elements) > 100000 {
		return nil, nil, nil, invalidRequest("invalid_request", "requests must contain between 1 and 100000 entries.", nil)
	}
	var requests []anthropicBatchRequest
	if json.Unmarshal(body, &struct {
		Requests *[]anthropicBatchRequest `json:"requests"`
	}{Requests: &requests}) != nil || len(requests) != len(elements) {
		return nil, nil, nil, invalidRequest("invalid_request", "Every request must contain custom_id and params.", nil)
	}
	seen := make(map[string]struct{}, len(requests))
	fileIDs := []string{}
	model := routeSlug
	for i, request := range requests {
		members := elements[i].Members()
		if elements[i].Kind() != oif.Object || len(members) != 2 {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d] must contain only custom_id and params.", i), nil)
		}
		memberNames := map[string]bool{}
		for _, member := range members {
			memberNames[member.Name] = true
		}
		if !memberNames["custom_id"] || !memberNames["params"] {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d] must contain custom_id and params.", i), nil)
		}
		if !anthropicCustomID.MatchString(request.CustomID) {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].custom_id is invalid.", i), nil)
		}
		if _, exists := seen[request.CustomID]; exists {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].custom_id is duplicated.", i), nil)
		}
		seen[request.CustomID] = struct{}{}
		params, parseErr := oif.ParseJSON(request.Params, oif.Limits{MaxBytes: len(request.Params)})
		if parseErr != nil || params.Root().Kind() != oif.Object {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params must be an object.", i), nil)
		}
		if stream, ok := params.Root().Lookup("stream"); ok && stream.Raw() == "true" {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params.stream is unsupported.", i), nil)
		}
		modelValue, hasModel := params.Root().Lookup("model")
		requestModel, _ := modelValue.Text()
		parseModel := routeSlug
		if parseModel == "" {
			parseModel = requestModel
		}
		if parsed, parseErr := protocols.Parse(wire.Anthropic, request.Params, parseModel); parseErr != nil || parsed == nil {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params is invalid.", i), nil)
		}
		if routeSlug == "" {
			if !hasModel || requestModel == "" || i > 0 && model != requestModel {
				return nil, nil, nil, invalidRequest("invalid_request", "A single common params.model or X-OLP-Route is required.", nil)
			}
			model = requestModel
		}
		if requestModel == "" || targetModel != "" && requestModel != routeSlug && requestModel != targetModel {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params.model does not match the route.", i), nil)
		}
		refs, refErr := anthropicFileReferences(request.Params)
		if refErr != nil {
			return nil, nil, nil, invalidRequest("invalid_request", fmt.Sprintf("requests[%d].params contains an invalid file source.", i), nil)
		}
		fileIDs = append(fileIDs, refs...)
	}
	bound, err := bindAnthropicBatchModel(requests, cmp.Or(boundModel, targetModel))
	if err != nil {
		return nil, nil, nil, invalidRequest("invalid_request", "Every request params must be an object.", nil)
	}
	return mustJSON(struct {
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
			params["model"] = mustJSON(model)
		}
		bound[i] = anthropicBatchRequest{CustomID: request.CustomID, Params: mustJSON(params)}
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
	projected["id"] = mustJSON(localID)
	if fields.ResultsURL != nil {
		projected["results_url"] = mustJSON(origin + "/anthropic/v1/messages/batches/" + url.PathEscape(localID) + "/results")
	}
	stored := maps.Clone(projected)
	delete(stored, "id")
	stored["upstream_model"] = mustJSON(model)
	return anthropicBatchView{body: mustJSON(projected), upstreamID: fields.ID, state: fields.ProcessingStatus, metadata: mustJSON(stored)}, nil
}
