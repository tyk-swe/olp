package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.Batch)
	if done {
		return
	}
	defer s.release(r.Context())
	body, e := s.readResourceBody(w, r)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	source, err := parseObjectRequest(body, oif.Limits{MaxBytes: int(s.cfg.MaxBodyBytes)})
	if err != nil || source.Root().Kind() != oif.Object {
		s.stateFail(x, w, invalidRequest("invalid_json", "The request body must be one JSON object.", nil), x.family)
		return
	}
	lineOperation, e := batchLineOperation(source)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	fileValue, ok := source.Root().Lookup("input_file_id")
	localFile, valid := fileValue.Text()
	if !ok || !valid || !strings.HasPrefix(localFile, "file_") && !strings.HasPrefix(localFile, "strict_file_") {
		param := "input_file_id"
		s.stateFail(x, w, invalidRequest("missing_required_parameter", "input_file_id must name a file uploaded through this key.", &param), x.family)
		return
	}
	fileKind := durableKind(resources.KindFile, localFile)
	file, fileContract, err := s.readDurable(r.Context(), fileKind, authority.ID, localFile)
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No file with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."), x.family)
		return
	}
	// The batch runs under the file's retained route revision. Resolution
	// refuses a file whose contract the current route no longer promises, so
	// that revision's fidelity is also the route's current one.
	if candidate, exists := x.snapshot().FindRoute(file.RouteSlug); exists {
		x.route = &candidate
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, file, "batch", retainedNewWork)
	if e != nil {
		if refusal := fireworksBatchRefusal(x, x.route); refusal != nil {
			e = refusal
		}
		s.stateFail(x, w, e, x.family)
		return
	}
	x.route = route
	completionWindow, e := batchCompletionWindow(source, p.provider.VendorID)
	if e != nil {
		s.releaseHold(r.Context(), p.hold)
		s.stateFail(x, w, e, x.family)
		return
	}
	endpointValue, endpointPresent := source.Root().Lookup("endpoint")
	requestEndpoint, endpointValid := endpointValue.Text()
	if p.provider.VendorID == "groq" && (!endpointPresent || !endpointValid || requestEndpoint != "/v1/chat/completions") {
		param := "endpoint"
		s.releaseHold(r.Context(), p.hold)
		s.stateFail(x, w, invalidRequest("unsupported_parameter", "Groq batches currently support only /v1/chat/completions.", &param), x.family)
		return
	}
	adapter := p.provider.Connector().FilesBatchesAdapter()
	if p.provider.VendorID == "together" || p.provider.VendorID == "mistral" || adapter == "gemini_managed" {
		allowed := map[string]bool{"input_file_id": true, "endpoint": true, "completion_window": true, "metadata": true}
		for _, field := range source.Root().Members() {
			if !allowed[field.Name] {
				param := field.Name
				s.releaseHold(r.Context(), p.hold)
				s.stateFail(x, w, invalidRequest("unsupported_parameter", "This batch parameter is not supported.", &param), x.family)
				return
			}
		}
		if adapter == "gemini_managed" {
			for _, required := range []string{"input_file_id", "endpoint", "completion_window"} {
				if _, present := source.Root().Lookup(required); !present {
					param := required
					s.releaseHold(r.Context(), p.hold)
					s.stateFail(x, w, invalidRequest("missing_required_parameter", "Gemini managed batches require input_file_id, endpoint, and completion_window.", &param), x.family)
					return
				}
			}
		}
	}
	if x.strict() {
		if refusal := strictBatchTargetRefusal(p); refusal != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, refusal, x.family)
			return
		}
	}
	var inputManifest batchManifest
	if adapter == "openai" && !x.strict() {
		var metadata map[string]json.RawMessage
		_ = json.Unmarshal(file.Metadata, &metadata)
		if json.Unmarshal(metadata["batch_manifest"], &inputManifest) != nil || inputManifest.Adapter != "openai" ||
			inputManifest.Endpoint != requestEndpoint || inputManifest.LineCount < 1 || inputManifest.ManifestDigest == "" {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("resource_affinity", "Upload a batch file qualified for this model and endpoint before submitting it.", strPtr("input_file_id")), x.family)
			return
		}
	}
	if adapter == "together" || adapter == "mistral" || adapter == "gemini_managed" {
		var meta map[string]json.RawMessage
		_ = json.Unmarshal(file.Metadata, &meta)
		var storedAdapter string
		_ = json.Unmarshal(meta["adapter"], &storedAdapter)
		_ = json.Unmarshal(meta["endpoint"], &inputManifest.Endpoint)
		_ = json.Unmarshal(meta["line_operation"], &inputManifest.LineOperation)
		_ = json.Unmarshal(meta["line_count"], &inputManifest.LineCount)
		_ = json.Unmarshal(meta["manifest_digest"], &inputManifest.ManifestDigest)
		inputManifest.Adapter = storedAdapter
		if storedAdapter != adapter || inputManifest.Endpoint == "" || inputManifest.LineCount < 1 || inputManifest.ManifestDigest == "" {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, notFoundError("not_found", "The uploaded file has no compatible batch manifest."), x.family)
			return
		}
	}
	if _, supplied := source.Root().Lookup("id"); supplied && adapter != "together" && adapter != "mistral" {
		s.releaseHold(r.Context(), p.hold)
		s.stateFail(x, w, invalidRequest("invalid_request", "A new batch cannot supply a provider resource ID.", strPtr("id")), x.family)
		return
	}
	if x.strict() {
		if fileKind != resources.KindStrictFile || fileContract == nil || !authority.Policy.AllowProviderState || !s.deps.Resources.Encrypted() {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("resource_affinity", "Strict batches require a file uploaded under the same strict route and state-enabled key.", strPtr("input_file_id")), x.family)
			return
		}
		if e := s.authorizeDurable(r.Context(), x, authority, file, fileContract, "batch", retainedNewWork); e != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, e, x.family)
			return
		}
		requestEndpoint, present := source.Root().Lookup("endpoint")
		path, valid := requestEndpoint.Text()
		if !present || !valid || fileContract.Asset == nil || fileContract.Asset.Endpoint != path || fileContract.Asset.ItemCount < 1 {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("resource_affinity", "The batch endpoint must match every qualified item in its uploaded file.", strPtr("endpoint")), x.family)
			return
		}
	}
	if e := s.unboundedPinLimits(x.admissionAuthority(authority), p, route); e != nil {
		s.releaseHold(r.Context(), p.hold)
		s.stateFail(x, w, e, x.family)
		return
	}
	if e := x.guardrailSurfaceGate(route); e != nil {
		s.releaseHold(r.Context(), p.hold)
		s.stateFail(x, w, e, x.family)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveStateAndSlot(ctx, x, authority, p); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	upstreamFile, _ := json.Marshal(file.UpstreamID)
	var upstream []byte
	var strictServing oif.ServingIdentity
	var expectedResults *int64
	if x.strict() {
		expectedResults = new(int64(fileContract.Asset.ItemCount))
	} else if inputManifest.Adapter == "openai" {
		expectedResults = new(int64(inputManifest.LineCount))
	}
	requestedWindow := "24h"
	if value, present := source.Root().Lookup("completion_window"); present {
		requestedWindow, _ = value.Text()
	}
	if x.strict() {
		template, ok := x.snapshot().DurableTemplate(route.Slug, p.target.ID)
		if !ok {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("target_capability", "The selected target has no strict batch contract.", nil), x.family)
			return
		}
		bound, bindErr := template.BindBatch(source, route.Slug, localFile, file.UpstreamID)
		if bindErr != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("target_capability", "The batch source cannot satisfy this native target contract.", nil), x.family)
			return
		}
		upstream = bound.Effective.Document().Bytes()
		strictServing = bound.Receipt.Serving
	} else if adapter == "together" || adapter == "mistral" || adapter == "gemini_managed" {
		upstream, expectedResults, e = managedBatchRequestBody(source, adapter, file.UpstreamID, p.provider.Connector().Model(p.model), inputManifest)
		if e != nil {
			s.stateFail(x, w, e, x.family)
			return
		}
	} else {
		changes := []oif.Change{{Pointer: "/input_file_id", Value: string(upstreamFile), Origin: oif.ResourceBinding, Reason: "owner-scoped uploaded file"}}
		window := "24h"
		if p.provider.VendorID == "groq" {
			if value, present := source.Root().Lookup("completion_window"); present {
				window, _ = value.Text()
			}
		}
		windowJSON, _ := json.Marshal(window)
		changes = append(changes, oif.Change{Pointer: "/completion_window", Value: string(windowJSON), Origin: oif.ResourceBinding, Reason: "provider batch completion window"})
		if p.provider.Kind == "azure_openai" {
			deployment := p.provider.Connector().Model(p.model)
			if deployment != "" {
				upstreamModel, _ := json.Marshal(deployment)
				changes = append(changes, oif.Change{Pointer: "/model", Value: string(upstreamModel), Origin: oif.IdentityBinding, Reason: "selected Azure deployment"})
			}
		}
		effective, applyErr := oif.Apply(source, changes)
		if applyErr != nil {
			s.stateFail(x, w, invalidRequest("invalid_json", "The batch request could not be bound to its provider resource.", nil), x.family)
			return
		}
		upstream = effective.Bytes()
	}
	batchPath := "batches"
	if adapter == "mistral" {
		batchPath = "batch/jobs"
	} else if adapter == "gemini_managed" {
		batchPath = "models/" + url.PathEscape(p.provider.Connector().Model(p.model)) + ":batchGenerateContent"
	}
	endpoint, err := resourceURL(p.provider.Connector(), p.model, batchPath, nil)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."), x.family)
		return
	}
	kind := resources.KindBatch
	if x.strict() {
		kind = resources.KindStrictBatch
	}
	var reconciliationCursor json.RawMessage
	if adapter == "gemini_managed" {
		clientMetadata := source.Fields()["metadata"]
		metadata, _, metadataErr := geminiManagedBatchMetadata([]byte(`{"metadata":{"state":"BATCH_STATE_PENDING"}}`), p.model, localFile, inputManifest, nil, clientMetadata)
		if metadataErr != nil {
			s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The Gemini submission metadata could not be prepared."), x.family)
			return
		}
		reconciliationCursor = mustJSON(map[string]any{"submission": map[string]any{
			"gemini_file": file.UpstreamID, "local_input_file_id": localFile,
			"endpoint": inputManifest.Endpoint, "gemini_metadata": metadata,
		}})
	}
	submission, e := s.prepareBatchSubmission(ctx, x, authority.ID, p, source, source.Bytes(), upstream,
		strictServing, localFile, file.UpstreamID, kind, adapter, "openai", lineOperation, completionWindow, expectedResults, reconciliationCursor)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if adapter == "gemini_managed" {
		displayName := "olp-" + strings.ReplaceAll(submission.resource.UUID.String(), "-", "")
		upstream, err = geminiManagedBatchBody(displayName, file.UpstreamID)
		if err != nil {
			cleanupCtx, stopCleanup := resourceCommitContext(r.Context())
			defer stopCleanup()
			if cleanupErr := s.deps.Batches.DeleteSubmission(cleanupCtx, submission.resource.UUID); cleanupErr != nil {
				s.log.Warn("Gemini batch submission intent could not be removed", "resource_id", submission.resource.UUID, "error", cleanupErr)
			}
			s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The Gemini batch request could not be constructed."), x.family)
			return
		}
	}
	resp, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, upstream, "application/json")
	if failure != nil {
		x.dispatched = failure.dispatched
		if failure.acceptance.Unresolved() {
			unknownCtx, stopUnknown := resourceCommitContext(r.Context())
			defer stopUnknown()
			if err := s.deps.Batches.MarkUnknown(unknownCtx, submission.resource.UUID, "submission_unknown", s.now()); err != nil {
				s.log.Warn("batch submission outcome could not be recorded", "resource_id", submission.resource.UUID, "error", err)
			}
			s.stateFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The provider may have accepted this batch, but its outcome is unknown."), x.family)
			return
		}
		cleanupCtx, stopCleanup := resourceCommitContext(r.Context())
		defer stopCleanup()
		if err := s.deps.Batches.DeleteSubmission(cleanupCtx, submission.resource.UUID); err != nil {
			s.log.Warn("failed batch submission intent could not be removed", "resource_id", submission.resource.UUID, "error", err)
		}
		s.stateFail(x, w, upstreamError(failure), x.family)
		return
	}
	defer resp.Body.Close()
	x.dispatched = true
	result, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		s.stateFail(x, w, s.batchSubmissionUnknown(r.Context(), submission.resource.UUID, "The provider response could not be read. The batch may have been accepted."), x.family)
		return
	}
	var upstreamID string
	if x.strict() {
		var validated oif.Document
		validated, upstreamID, err = strictBatchResult(result, "")
		if err != nil || !strictBatchInputMatches(validated, upstream) {
			s.stateFail(x, w, s.batchSubmissionUnknown(r.Context(), submission.resource.UUID, "The provider returned an invalid native batch result. The batch may have been accepted."), x.family)
			return
		}
	} else if adapter == "gemini_managed" {
		var identityErr error
		upstreamID, identityErr = geminiBatchName(result)
		if identityErr != nil {
			unknownCtx, stopUnknown := resourceCommitContext(r.Context())
			defer stopUnknown()
			if markErr := s.deps.Batches.MarkUnknown(unknownCtx, submission.resource.UUID, "submission_unknown", s.now()); markErr != nil {
				s.log.Warn("Gemini batch submission outcome could not be recorded", "resource_id", submission.resource.UUID, "error", markErr)
			}
			s.stateFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The provider accepted a batch with an invalid operation name."), x.family)
			return
		}
	} else if adapter == "together" || adapter == "mistral" {
		var valid bool
		result, upstreamID, valid = managedBatchResponse(result, adapter, requestedWindow)
		if !valid {
			unknownCtx, stopUnknown := resourceCommitContext(r.Context())
			defer stopUnknown()
			if markErr := s.deps.Batches.MarkUnknown(unknownCtx, submission.resource.UUID, "submission_unknown", s.now()); markErr != nil {
				s.log.Warn("managed batch submission outcome could not be recorded", "resource_id", submission.resource.UUID, "error", markErr)
			}
			s.stateFail(x, w, serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid managed batch result."), x.family)
			return
		}
		if metadata, present := source.Fields()["metadata"]; present {
			result = attachBatchClientMetadata(result, metadata)
		}
	} else {
		var valid bool
		if upstreamID, valid = nativeResourceID(result, s.cfg.MaxResponseBytes); !valid {
			s.stateFail(x, w, s.batchSubmissionUnknown(r.Context(), submission.resource.UUID, "The provider returned a malformed batch object. The batch may have been accepted."), x.family)
			return
		}
	}
	x.completePinned()
	metadata, state := batchMetadata(result, p.model, localFile)
	commitCtx, stopCommit := resourceCommitContext(ctx)
	defer stopCommit()
	resource := submission.resource
	resource.UpstreamID, resource.State = upstreamID, state
	resource.Metadata = metadata
	if adapter == "gemini_managed" {
		var clientMetadata json.RawMessage
		if raw, present := source.Fields()["metadata"]; present {
			clientMetadata = raw
		}
		resource.Metadata, state, err = geminiManagedBatchMetadata(result, p.model, localFile, inputManifest, nil, clientMetadata)
		if err != nil {
			if markErr := s.deps.Batches.MarkUnknown(commitCtx, resource.UUID, "submission_unknown", s.now()); markErr != nil {
				s.log.Warn("Gemini batch submission metadata could not be recorded", "resource_id", resource.UUID, "error", markErr)
			}
			s.stateFail(x, w, serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid Gemini batch object."), x.family)
			return
		}
		resource.State = state
	}
	var res *resources.Resource
	if x.strict() {
		resource.Metadata = durableMetadata(p.model)
	} else if adapter == "together" || adapter == "mistral" {
		resource.Metadata, state = managedBatchMetadata(result, p.model, localFile, adapter)
		resource.State = state
	}
	res, err = func() (*resources.Resource, error) {
		tx, beginErr := s.deps.Resources.Begin(commitCtx)
		if beginErr != nil {
			return nil, beginErr
		}
		defer tx.Rollback(commitCtx)
		var created *resources.Resource
		if x.strict() {
			payload, marshalErr := json.Marshal(durableDocument{
				Version: *resource.ContractVersion, Source: source.Bytes(), Effective: upstream,
				Result: result, Binding: p.model, Serving: strictServing,
			})
			if marshalErr != nil {
				return nil, marshalErr
			}
			created, beginErr = s.deps.Resources.PutDurableContractTx(commitCtx, tx, resource, payload)
		} else {
			created, beginErr = s.deps.Resources.PutTx(commitCtx, tx, resource)
		}
		if beginErr != nil {
			return nil, beginErr
		}
		acceptedExpected := expectedResults
		if acceptedExpected == nil {
			acceptedExpected = batchExpectedResults(result)
		}
		if beginErr = s.deps.Batches.ActivateTx(commitCtx, tx, resource.UUID, upstreamID, acceptedExpected); beginErr != nil {
			return nil, beginErr
		}
		if beginErr = tx.Commit(commitCtx); beginErr != nil {
			return nil, beginErr
		}
		return created, nil
	}()
	if err != nil {
		if markErr := s.deps.Batches.MarkUnknown(commitCtx, resource.UUID, "submission_commit_unavailable", s.now()); markErr != nil {
			s.log.Warn("accepted batch submission intent could not be activated", "resource_id", resource.UUID, "error", markErr)
		}
		s.stateFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The provider accepted the batch, but its local state could not be confirmed."), x.family)
		return
	}
	out, err := s.batchObject(r.Context(), res)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object."), x.family)
		return
	}
	s.writeStateJSON(w, x, out)
}

func (s *Server) batchCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, keyauthority.Authority, *resources.Resource, *pin, *routing.Route) *Error) {
	x, authority, done := s.stateBegin(w, r, wire.Batch)
	if done {
		return
	}
	defer s.release(r.Context())
	kind := durableKind(resources.KindBatch, r.PathValue("id"))
	if kind == resources.KindStrictBatch && !authority.Policy.AllowProviderState {
		s.stateFail(x, w, providerStateForbidden(), x.family)
		return
	}
	res, contract, err := s.readDurable(r.Context(), kind, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No batch with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The batch mapping could not be read."), x.family)
		return
	}
	p, route, e := s.admitDurable(r.Context(), x, authority, res, contract, "batch", use)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveStateAndSlot(ctx, x, authority, p); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.stateFail(x, w, e, x.family)
	}
}

func (s *Server) listBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.Batch)
	if done {
		return
	}
	defer s.release(r.Context())
	limit, after, e := listArgs(r)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	kinds := []string{resources.KindBatch}
	if authority.Policy.AllowProviderState {
		kinds = append(kinds, resources.KindStrictBatch)
	}
	// One extra row reports whether another page follows; it is never
	// authorized or projected.
	rows, err := s.deps.Resources.ListKinds(r.Context(), kinds, authority.ID, limit+1, after)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The batch list could not be read."), x.family)
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]json.RawMessage, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Kind == resources.KindStrictBatch {
			_, contract, readErr := s.readDurable(r.Context(), row.Kind, authority.ID, row.ID)
			if readErr != nil || s.authorizeDurable(r.Context(), x, authority, row, contract, "batch", retainedHousekeeping) != nil {
				s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A batch in this list is unavailable."), x.family)
				return
			}
		}
		obj, err := s.batchObject(r.Context(), row)
		if err != nil {
			s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A batch in this list could not be projected."), x.family)
			return
		}
		items = append(items, obj)
		ids = append(ids, row.ID)
	}
	s.writeStateJSON(w, x, listBody(items, ids, hasMore))
}

func (s *Server) getBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, authority keyauthority.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
		return s.batchRefresh(ctx, x, res, p, http.MethodGet, managedBatchPath(res, false), nil, w)
	})
}

func (s *Server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, authority keyauthority.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
		if batchAdapter(res) == "gemini_managed" {
			endpoint, err := resourceURL(p.provider.Connector(), p.model, managedBatchPath(res, true), nil)
			if err != nil {
				return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
			}
			response, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, []byte(`{}`), "application/json")
			if failure != nil {
				return upstreamError(failure)
			}
			_ = response.Body.Close()
			x.dispatched = true
			x.completePinned()
			return s.batchRefresh(ctx, x, res, p, http.MethodGet, managedBatchPath(res, false), nil, w)
		}
		if res.Kind == resources.KindStrictBatch {
			switch wire.BatchStageOf(res.State) {
			case wire.BatchTerminal:
				return serverError(http.StatusConflict, "resource_transition", "This batch has already reached a terminal state.")
			case wire.BatchCancelling:
				out, err := s.batchObject(ctx, res)
				if err != nil {
					return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The accepted cancellation could not be projected.")
				}
				s.writeStateJSON(w, x, out)
				return nil
			}
		}
		return s.batchRefresh(ctx, x, res, p, http.MethodPost, managedBatchPath(res, true), []byte(`{}`), w)
	})
}

func (s *Server) batchRefresh(ctx context.Context, x *call, res *resources.Resource, p *pin, method, path string, body []byte, w http.ResponseWriter) *Error {
	endpoint, err := resourceURL(p.provider.Connector(), p.model, path, nil)
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.pinnedDo(ctx, x, p, method, endpoint, body, "application/json")
	if failure != nil {
		if failure.status == http.StatusNotFound {
			return notFoundError("not_found", "No batch with this identifier exists for this key.")
		}
		return upstreamError(failure)
	}
	defer resp.Body.Close()
	x.dispatched = true
	result, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	adapter := batchAdapter(res)
	if method == http.MethodPost && strings.HasSuffix(path, ":cancel") && adapter == "gemini_managed" && geminiBatchCancelAcknowledgement(result) {
		x.completePinned()
		body, err := s.batchObject(ctx, res)
		if err != nil {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored Gemini batch could not be projected.")
		}
		s.writeStateJSON(w, x, body)
		return nil
	}
	if adapter == "together" || adapter == "mistral" || adapter == "gemini_managed" {
		if adapter == "gemini_managed" {
			upstreamID, identityErr := geminiBatchName(result)
			if identityErr != nil || upstreamID != res.UpstreamID {
				return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini batch identity.")
			}
		} else {
			window := "24h"
			var stored map[string]json.RawMessage
			_ = json.Unmarshal(res.Metadata, &stored)
			_ = json.Unmarshal(stored["completion_window"], &window)
			projected, _, projectErr := projectManagedBatch(result, adapter, window)
			if projectErr != nil {
				return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unknown batch status.")
			}
			var prior map[string]json.RawMessage
			_ = json.Unmarshal(res.Metadata, &prior)
			result = attachBatchClientMetadata(projected, prior["metadata"])
		}
	}
	metadata, state := batchMetadata(result, resourceModel(res), batchLocalInput(res))
	if adapter == "gemini_managed" {
		var clientMetadata json.RawMessage
		var previous map[string]json.RawMessage
		_ = json.Unmarshal(res.Metadata, &previous)
		clientMetadata = previous["client_metadata"]
		metadata, state, err = geminiManagedBatchMetadata(result, resourceModel(res), batchLocalInput(res),
			geminiManagedManifest(res.Metadata), res.Metadata, clientMetadata)
		if errors.Is(err, errGeminiManagedBatchState) {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unknown Gemini batch status.")
		}
		if err != nil {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a malformed Gemini batch object.")
		}
	} else if adapter == "together" || adapter == "mistral" {
		metadata, state = managedBatchMetadata(result, resourceModel(res), batchLocalInput(res), adapter)
	}
	commitCtx := ctx
	stopCommit := func() {}
	if method == http.MethodPost {
		commitCtx, stopCommit = resourceCommitContext(ctx)
	}
	defer stopCommit()
	if res.Kind == resources.KindStrictBatch {
		validated, _, err := strictBatchResult(result, res.UpstreamID)
		if err != nil {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native batch result.")
		}
		_, contract, err := s.readDurable(ctx, res.Kind, res.APIKeyID, res.ID)
		if err != nil {
			return durableError(err)
		}
		if !strictBatchInputMatches(validated, contract.Effective) {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider changed the batch input file identity.")
		}
		x.completePinned()
		contract.Result = result
		payload, err := json.Marshal(contract)
		if err != nil {
			return durableError(err)
		}
		if _, _, err = s.deps.Resources.UpdateDurableContract(commitCtx, res.Kind, res.APIKeyID, res.ID, state, payload); err != nil {
			return durableError(err)
		}
	} else if adapter == "gemini_managed" {
		upstreamID, identityErr := geminiBatchName(result)
		if identityErr != nil || upstreamID != res.UpstreamID {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini batch identity.")
		}
		x.completePinned()
		if err := s.deps.Resources.Update(commitCtx, res.ID, state, metadata, nil); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed batch state could not be committed.")
		}
		res.State, res.Metadata = state, metadata
	} else {
		if !retainedIdentityMatches(result, res.UpstreamID, s.cfg.MaxResponseBytes) {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
		}
		x.completePinned()
		if err := s.deps.Resources.Update(commitCtx, res.ID, state, metadata, nil); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed batch state could not be committed.")
		}
		res.State, res.Metadata = state, metadata
	}
	out, err := s.batchObject(ctx, res)
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
	}
	s.writeStateJSON(w, x, out)
	return nil
}
