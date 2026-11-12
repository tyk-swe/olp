package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

type batchCreateRequest struct {
	source    oif.Document
	endpoint  string
	operation wire.LineOperation
	localFile string
}

type batchPlan struct {
	id             uuid.UUID
	adapter        connectors.BatchAdapter
	kind           string
	endpoint       string
	body           []byte
	window         batchWindow
	expected       *int64
	serving        oif.ServingIdentity
	submission     batches.Submission
	manifest       batchManifest
	clientMetadata json.RawMessage
}

func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.Batch)
	if done {
		return
	}
	defer s.gate.Release(r.Context())
	request, e := s.batchCreateRequest(w, r)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	file, contract, e := s.batchInputFile(r.Context(), authority.ID, request.localFile)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	if candidate, exists := x.snapshot().FindRoute(file.RouteSlug); exists {
		x.route = &candidate
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, file, "batch", retainedNewWork)
	if e != nil {
		if refusal := fireworksBatchRefusal(x, x.route); refusal != nil {
			e = refusal
		}
		s.refuse(x, w, e)
		return
	}
	x.route = route
	plan, e := s.planBatch(r.Context(), x, authority, request, file, contract, p, route)
	if e != nil {
		s.releaseHold(r.Context(), p.hold)
		s.refuse(x, w, e)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.refuse(x, w, e)
		return
	}
	resource, e := s.submitBatch(ctx, x, authority, p, request, plan)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	out, err := s.batchObject(r.Context(), resource)
	if err != nil {
		s.refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object."))
		return
	}
	s.writeStateJSON(w, x, out)
}

func (s *Server) batchCreateRequest(w http.ResponseWriter, r *http.Request) (batchCreateRequest, *fault.Error) {
	body, e := s.readResourceBody(w, r)
	if e != nil {
		return batchCreateRequest{}, e
	}
	source, err := ingress.ParseObjectRequest(body, oif.Limits{MaxBytes: int(s.cfg.MaxBodyBytes)})
	if err != nil || source.Root().Kind() != oif.Object {
		return batchCreateRequest{}, fault.InvalidRequest("invalid_json", "The request body must be one JSON object.", nil)
	}
	endpoint, operation, e := batchLineOperation(source)
	if e != nil {
		return batchCreateRequest{}, e
	}
	fileValue, ok := source.Root().Lookup("input_file_id")
	localFile, valid := fileValue.Text()
	if !ok || !valid || !strings.HasPrefix(localFile, "file_") && !strings.HasPrefix(localFile, "strict_file_") {
		return batchCreateRequest{}, fault.InvalidRequest("missing_required_parameter", "input_file_id must name a file uploaded through this key.", new("input_file_id"))
	}
	return batchCreateRequest{source: source, endpoint: endpoint, operation: operation, localFile: localFile}, nil
}

func (s *Server) batchInputFile(ctx context.Context, owner, localFile string) (*resources.Resource, *durableDocument, *fault.Error) {
	file, contract, err := s.readDurable(ctx, durableKind(resources.KindFile, localFile), owner, localFile)
	if errors.Is(err, resources.ErrNotFound) {
		return nil, nil, fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
	}
	if err != nil {
		return nil, nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read.")
	}
	return file, contract, nil
}

func (s *Server) planBatch(ctx context.Context, x *call, authority keyauthority.Authority, request batchCreateRequest, file *resources.Resource, contract *durableDocument, p *pin, route *routing.Route) (batchPlan, *fault.Error) {
	window, e := batchCompletionWindow(request.source, p.provider.VendorID)
	if e != nil {
		return batchPlan{}, e
	}
	if p.provider.VendorID == "groq" && request.endpoint != "/v1/chat/completions" {
		return batchPlan{}, fault.InvalidRequest("unsupported_parameter", "Groq batches currently support only /v1/chat/completions.", new("endpoint"))
	}
	adapter, ok := p.provider.Connector().FilesBatchesAdapter()
	if !ok {
		return batchPlan{}, fault.InvalidRequest("target_capability", "The selected target does not support batches.", nil)
	}
	if e := managedBatchShape(request.source, p.provider.VendorID, adapter); e != nil {
		return batchPlan{}, e
	}
	if x.strict() {
		if e := s.strictBatchAdmission(ctx, x, authority, request, file, contract, p); e != nil {
			return batchPlan{}, e
		}
	}
	manifest, e := inputBatchManifest(file, adapter, request.endpoint, x.strict())
	if e != nil {
		return batchPlan{}, e
	}
	if _, supplied := request.source.Root().Lookup("id"); supplied && adapter != connectors.BatchTogether && adapter != connectors.BatchMistral {
		return batchPlan{}, fault.InvalidRequest("invalid_request", "A new batch cannot supply a provider resource ID.", new("id"))
	}
	if e := s.unboundedPinLimits(x.admissionAuthority(authority), p, route); e != nil {
		return batchPlan{}, e
	}
	if e := x.guardrailSurfaceGate(route); e != nil {
		return batchPlan{}, e
	}
	plan := batchPlan{
		id: uuid.New(), adapter: adapter, kind: resources.KindBatch, window: window, manifest: manifest,
		clientMetadata: request.source.Fields()["metadata"],
	}
	if x.strict() {
		return s.planStrictBatch(x, request, file, contract, p, route, plan)
	}
	return planFilesBatch(request, file, p, plan)
}

func managedBatchShape(source oif.Document, vendor string, adapter connectors.BatchAdapter) *fault.Error {
	if vendor != "together" && vendor != "mistral" && adapter != connectors.BatchGeminiManaged {
		return nil
	}
	allowed := []string{"input_file_id", "endpoint", "completion_window", "metadata"}
	for _, field := range source.Root().Members() {
		if !slices.Contains(allowed, field.Name) {
			return fault.InvalidRequest("unsupported_parameter", "This batch parameter is not supported.", new(field.Name))
		}
	}
	if adapter != connectors.BatchGeminiManaged {
		return nil
	}
	for _, required := range allowed[:3] {
		if _, present := source.Root().Lookup(required); !present {
			return fault.InvalidRequest("missing_required_parameter", "Gemini managed batches require input_file_id, endpoint, and completion_window.", new(required))
		}
	}
	return nil
}

func (s *Server) strictBatchAdmission(ctx context.Context, x *call, authority keyauthority.Authority, request batchCreateRequest, file *resources.Resource, contract *durableDocument, p *pin) *fault.Error {
	if refusal := strictBatchTargetRefusal(p); refusal != nil {
		return refusal
	}
	if durableKind(resources.KindFile, request.localFile) != resources.KindStrictFile || contract == nil || !authority.Policy.AllowProviderState || !s.deps.Resources.Encrypted() {
		return fault.InvalidRequest("resource_affinity", "Strict batches require a file uploaded under the same strict route and state-enabled key.", new("input_file_id"))
	}
	if e := s.authorizeDurable(ctx, x, authority, file, contract, "batch", retainedNewWork); e != nil {
		return e
	}
	if contract.Asset == nil || contract.Asset.Endpoint != request.endpoint || contract.Asset.ItemCount < 1 {
		return fault.InvalidRequest("resource_affinity", "The batch endpoint must match every qualified item in its uploaded file.", new("endpoint"))
	}
	return nil
}

type storedFileManifest struct {
	batchManifest
	Qualified *batchManifest `json:"batch_manifest"`
}

func inputBatchManifest(file *resources.Resource, adapter connectors.BatchAdapter, endpoint string, strict bool) (batchManifest, *fault.Error) {
	if strict || adapter != connectors.BatchOpenAI && !adapter.Managed() {
		return batchManifest{}, nil
	}
	var stored storedFileManifest
	decodeErr := json.Unmarshal(file.Metadata, &stored)
	if adapter == connectors.BatchOpenAI {
		if decodeErr != nil || stored.Qualified == nil || !stored.Qualified.usable(adapter) || stored.Qualified.Endpoint != endpoint {
			return batchManifest{}, fault.InvalidRequest("resource_affinity", "Upload a batch file qualified for this model and endpoint before submitting it.", new("input_file_id"))
		}
		return *stored.Qualified, nil
	}
	if decodeErr != nil || !stored.batchManifest.usable(adapter) {
		return batchManifest{}, fault.NotFoundError("not_found", "The uploaded file has no compatible batch manifest.")
	}
	return stored.batchManifest, nil
}

func (m batchManifest) usable(adapter connectors.BatchAdapter) bool {
	return m.Adapter == adapter && m.Endpoint != "" && m.LineCount >= 1 && m.ManifestDigest != ""
}

func (s *Server) planStrictBatch(x *call, request batchCreateRequest, file *resources.Resource, contract *durableDocument, p *pin, route *routing.Route, plan batchPlan) (batchPlan, *fault.Error) {
	template, ok := x.snapshot().DurableTemplate(route.Slug, p.target.ID)
	if !ok {
		return batchPlan{}, fault.InvalidRequest("target_capability", "The selected target has no strict batch contract.", nil)
	}
	bound, err := template.BindBatch(request.source, route.Slug, request.localFile, file.UpstreamID)
	if err != nil {
		return batchPlan{}, fault.InvalidRequest("target_capability", "The batch source cannot satisfy this native target contract.", nil)
	}
	endpoint, err := resourceURL(p.provider.Connector(), p.model, "batches", nil)
	if err != nil {
		return batchPlan{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	plan.kind, plan.endpoint, plan.serving = resources.KindStrictBatch, endpoint, bound.Receipt.Serving
	plan.body = bound.Effective.Document().Bytes()
	plan.expected = new(int64(contract.Asset.ItemCount))
	plan.submission = filesSubmission(request, file, plan)
	return plan, nil
}

func planFilesBatch(request batchCreateRequest, file *resources.Resource, p *pin, plan batchPlan) (batchPlan, *fault.Error) {
	body, e := filesBatchBody(request, file, p, plan)
	if e != nil {
		return batchPlan{}, e
	}
	path := upstream.JobsPath(plan.adapter, "", false)
	if plan.adapter == connectors.BatchGeminiManaged {
		path = "models/" + url.PathEscape(p.provider.Connector().Model(p.model)) + ":batchGenerateContent"
	}
	endpoint, err := resourceURL(p.provider.Connector(), p.model, strings.TrimSuffix(path, "/"), nil)
	if err != nil {
		return batchPlan{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	plan.endpoint, plan.body = endpoint, body
	plan.expected = new(int64(plan.manifest.LineCount))
	if plan.adapter != connectors.BatchGeminiManaged {
		plan.submission = filesSubmission(request, file, plan)
		return plan, nil
	}
	pending, err := geminiManagedBatchMetadata([]byte(`{"metadata":{"state":"BATCH_STATE_PENDING"}}`), p.model, newGeminiManagedRecord(request.localFile, plan.manifest, plan.clientMetadata))
	if err != nil {
		return batchPlan{}, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini submission metadata could not be prepared.")
	}
	plan.submission = batches.GeminiManagedSubmission{
		GeminiFile: file.UpstreamID, LocalInputFileID: request.localFile, Endpoint: plan.manifest.Endpoint, Metadata: pending.metadata,
	}
	return plan, nil
}

func filesSubmission(request batchCreateRequest, file *resources.Resource, plan batchPlan) batches.FilesSubmission {
	submission := batches.FilesSubmission{
		InputFileID: file.UpstreamID, LocalInputFileID: request.localFile, Endpoint: request.endpoint, CompletionWindow: plan.window.label,
	}
	if plan.adapter == connectors.BatchTogether {
		submission.ClientMetadata = plan.clientMetadata
	}
	return submission
}

func filesBatchBody(request batchCreateRequest, file *resources.Resource, p *pin, plan batchPlan) ([]byte, *fault.Error) {
	if plan.adapter == connectors.BatchOpenAI {
		return openAIBatchBody(request.source, file.UpstreamID, p, plan.window)
	}
	return managedBatchRequestBody(request.source, plan.adapter, request.endpoint, plan.window, file.UpstreamID, p.provider.Connector().Model(p.model), plan.manifest, plan.id)
}

func openAIBatchBody(source oif.Document, upstreamFile string, p *pin, window batchWindow) ([]byte, *fault.Error) {
	upstreamWindow := "24h"
	if p.provider.VendorID == "groq" {
		upstreamWindow = window.label
	}
	changes := []oif.Change{
		{Pointer: "/input_file_id", Value: string(mustJSON(upstreamFile)), Origin: oif.ResourceBinding, Reason: "owner-scoped uploaded file"},
		{Pointer: "/completion_window", Value: string(mustJSON(upstreamWindow)), Origin: oif.ResourceBinding, Reason: "provider batch completion window"},
	}
	if deployment := p.provider.Connector().Model(p.model); p.provider.Kind == "azure_openai" && deployment != "" {
		changes = append(changes, oif.Change{Pointer: "/model", Value: string(mustJSON(deployment)), Origin: oif.IdentityBinding, Reason: "selected Azure deployment"})
	}
	effective, err := oif.Apply(source, changes)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_json", "The batch request could not be bound to its provider resource.", nil)
	}
	return effective.Bytes(), nil
}

func (s *Server) submitBatch(ctx context.Context, x *call, authority keyauthority.Authority, p *pin, request batchCreateRequest, plan batchPlan) (*resources.Resource, *fault.Error) {
	submission, e := s.beginBatch(ctx, x, authority.ID, p, batchIntent{
		resourceID: plan.id, kind: plan.kind, adapter: plan.adapter, surface: batches.SurfaceOpenAI,
		operation: request.operation, window: plan.window.length, expected: plan.expected, submission: plan.submission,
		source: request.source, canonical: request.source.Bytes(), effective: plan.body, serving: plan.serving,
	})
	if e != nil {
		return nil, e
	}
	result, e := s.sendBatch(ctx, x, p, plan.id, plan.endpoint, plan.body)
	if e != nil {
		return nil, e
	}
	accepted, e := s.acceptFilesBatch(ctx, p, request, plan, result)
	if e != nil {
		return nil, e
	}
	x.completePinned()
	return s.activateBatch(ctx, submission, accepted)
}

func (s *Server) acceptFilesBatch(ctx context.Context, p *pin, request batchCreateRequest, plan batchPlan, result []byte) (acceptedBatch, *fault.Error) {
	switch {
	case plan.kind == resources.KindStrictBatch:
		return s.acceptStrictBatch(ctx, p, request, plan, result)
	case plan.adapter == connectors.BatchGeminiManaged:
		batch, err := upstream.DecodeGeminiBatch(result)
		if err != nil {
			return acceptedBatch{}, s.batchSubmissionUnknown(ctx, plan.id, "The provider accepted a batch with an invalid operation name.")
		}
		managed, err := geminiManagedBatchMetadata(result, p.model, newGeminiManagedRecord(request.localFile, plan.manifest, plan.clientMetadata))
		if err != nil {
			return acceptedBatch{}, s.markBatchUnknown(ctx, plan.id, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid Gemini batch object."))
		}
		return acceptedBatch{upstreamID: batch.ID, state: managed.status, metadata: managed.metadata}, nil
	case plan.adapter.Managed():
		projected, upstreamID, err := upstream.ProjectManagedBatch(plan.adapter, result, plan.window.label)
		if err != nil {
			return acceptedBatch{}, s.markBatchUnknown(ctx, plan.id, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid managed batch result."))
		}
		adoption, err := upstream.FilesMetadata(plan.adapter, projected, p.model, request.localFile, plan.clientMetadata)
		if err != nil {
			return acceptedBatch{}, s.markBatchUnknown(ctx, plan.id, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid managed batch result."))
		}
		return acceptedBatch{upstreamID: upstreamID, state: adoption.State, metadata: adoption.Metadata}, nil
	default:
		upstreamID, valid := nativeResourceID(result, s.cfg.MaxResponseBytes)
		adoption, err := upstream.FilesMetadata(connectors.BatchOpenAI, result, p.model, request.localFile, nil)
		if !valid || err != nil {
			return acceptedBatch{}, s.batchSubmissionUnknown(ctx, plan.id, "The provider returned a malformed batch object. The batch may have been accepted.")
		}
		return acceptedBatch{upstreamID: upstreamID, state: adoption.State, metadata: adoption.Metadata, expected: batchExpectedResults(result)}, nil
	}
}

func (s *Server) acceptStrictBatch(ctx context.Context, p *pin, request batchCreateRequest, plan batchPlan, result []byte) (acceptedBatch, *fault.Error) {
	validated, upstreamID, err := strictBatchResult(result, "")
	if err != nil || !strictBatchInputMatches(validated, plan.body) {
		return acceptedBatch{}, s.batchSubmissionUnknown(ctx, plan.id, "The provider returned an invalid native batch result. The batch may have been accepted.")
	}
	adoption, err := upstream.FilesMetadata(connectors.BatchOpenAI, result, p.model, request.localFile, nil)
	if err != nil {
		return acceptedBatch{}, s.batchSubmissionUnknown(ctx, plan.id, "The provider returned an invalid native batch result. The batch may have been accepted.")
	}
	contract, err := json.Marshal(durableDocument{
		Version: resources.DurableContractVersion, Source: request.source.Bytes(), Effective: plan.body,
		Result: result, Binding: p.model, Serving: plan.serving,
	})
	if err != nil {
		return acceptedBatch{}, s.batchSubmissionUnknown(ctx, plan.id, "The accepted batch contract could not be encoded.")
	}
	return acceptedBatch{upstreamID: upstreamID, state: adoption.State, metadata: durableMetadata(p.model), expected: batchExpectedResults(result), contract: contract}, nil
}

func (s *Server) batchCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, keyauthority.Authority, *resources.Resource, *pin, *routing.Route) *fault.Error) {
	x, authority, done := s.stateBegin(w, r, wire.Batch)
	if done {
		return
	}
	defer s.gate.Release(r.Context())
	kind := durableKind(resources.KindBatch, r.PathValue("id"))
	if kind == resources.KindStrictBatch && !authority.Policy.AllowProviderState {
		s.refuse(x, w, providerStateForbidden())
		return
	}
	res, contract, err := s.readDurable(r.Context(), kind, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.refuse(x, w, fault.NotFoundError("not_found", "No batch with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The batch mapping could not be read."))
		return
	}
	p, route, e := s.admitDurable(r.Context(), x, authority, res, contract, "batch", use)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.refuse(x, w, e)
		return
	}
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.refuse(x, w, e)
	}
}

func (s *Server) listBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.Batch)
	if done {
		return
	}
	defer s.gate.Release(r.Context())
	limit, after, e := listArgs(r)
	if e != nil {
		s.refuse(x, w, e)
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
		s.refuse(x, w, resourceListError(err, "The batch list could not be read."))
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
				s.refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A batch in this list is unavailable."))
				return
			}
		}
		obj, err := s.batchObject(r.Context(), row)
		if err != nil {
			s.refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A batch in this list could not be projected."))
			return
		}
		items = append(items, obj)
		ids = append(ids, row.ID)
	}
	s.writeStateJSON(w, x, listBody(items, ids, hasMore))
}

func (s *Server) getBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, authority keyauthority.Authority, res *resources.Resource, p *pin, route *routing.Route) *fault.Error {
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		return s.batchRefresh(ctx, x, res, p, adapter, http.MethodGet, upstream.JobsPath(adapter, res.UpstreamID, false), nil, w)
	})
}

func (s *Server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, authority keyauthority.Authority, res *resources.Resource, p *pin, route *routing.Route) *fault.Error {
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		if adapter == connectors.BatchGeminiManaged {
			if e := s.cancelGeminiManagedBatch(ctx, x, res, p); e != nil {
				return e
			}
			return s.batchRefresh(ctx, x, res, p, adapter, http.MethodGet, upstream.JobsPath(adapter, res.UpstreamID, false), nil, w)
		}
		if res.Kind == resources.KindStrictBatch {
			switch wire.BatchStageOf(res.State) {
			case wire.BatchTerminal:
				return fault.ServerError(http.StatusConflict, "resource_transition", "This batch has already reached a terminal state.")
			case wire.BatchCancelling:
				out, err := s.batchObject(ctx, res)
				if err != nil {
					return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The accepted cancellation could not be projected.")
				}
				s.writeStateJSON(w, x, out)
				return nil
			}
		}
		return s.batchRefresh(ctx, x, res, p, adapter, http.MethodPost, upstream.JobsPath(adapter, res.UpstreamID, true), []byte(`{}`), w)
	})
}

func storedAdapter(res *resources.Resource) (connectors.BatchAdapter, *fault.Error) {
	adapter, err := resourceAdapter(res)
	if err != nil {
		return "", fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored provider mapping is malformed.")
	}
	return adapter, nil
}

func (s *Server) cancelGeminiManagedBatch(ctx context.Context, x *call, res *resources.Resource, p *pin) *fault.Error {
	endpoint, err := resourceURL(p.provider.Connector(), p.model, upstream.JobsPath(connectors.BatchGeminiManaged, res.UpstreamID, true), nil)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	response, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, []byte(`{}`), "application/json")
	if failure != nil {
		return upstreamError(failure)
	}
	_ = response.Body.Close()
	x.dispatched = true
	x.completePinned()
	return nil
}

type refreshedBatch struct {
	state    string
	metadata json.RawMessage
}

func (s *Server) batchRefresh(ctx context.Context, x *call, res *resources.Resource, p *pin, adapter connectors.BatchAdapter, method, path string, body []byte, w http.ResponseWriter) *fault.Error {
	result, e := s.fetchBatch(ctx, x, p, method, path, body)
	if e != nil {
		return e
	}
	refreshed, e := refreshBatchMetadata(res, adapter, result, s.cfg.MaxResponseBytes)
	if e != nil {
		return e
	}
	commitCtx, stopCommit := ctx, func() {}
	if method == http.MethodPost {
		commitCtx, stopCommit = resourceCommitContext(ctx)
	}
	defer stopCommit()
	current := *res
	if res.Kind == resources.KindStrictBatch {
		if e := s.commitStrictBatchRefresh(ctx, commitCtx, x, res, refreshed.state, result); e != nil {
			return e
		}
	} else {
		x.completePinned()
		if err := s.deps.Resources.Update(commitCtx, res.ID, refreshed.state, refreshed.metadata, nil); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed batch state could not be committed.")
		}
		current.State, current.Metadata = refreshed.state, refreshed.metadata
	}
	out, err := s.batchObject(ctx, &current)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
	}
	s.writeStateJSON(w, x, out)
	return nil
}

func (s *Server) fetchBatch(ctx context.Context, x *call, p *pin, method, path string, body []byte) ([]byte, *fault.Error) {
	endpoint, err := resourceURL(p.provider.Connector(), p.model, path, nil)
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.pinnedDo(ctx, x, p, method, endpoint, body, "application/json")
	if failure != nil {
		if failure.status == http.StatusNotFound {
			return nil, fault.NotFoundError("not_found", "No batch with this identifier exists for this key.")
		}
		return nil, upstreamError(failure)
	}
	defer resp.Body.Close()
	x.dispatched = true
	result, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	return result, nil
}

func refreshBatchMetadata(res *resources.Resource, adapter connectors.BatchAdapter, result []byte, maxBytes int64) (refreshedBatch, *fault.Error) {
	malformed := fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
	record, err := decodeBatchRecord(res)
	if err != nil {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored batch mapping is malformed.")
	}
	switch {
	case adapter == connectors.BatchGeminiManaged:
		return refreshGeminiManagedBatch(res, result)
	case adapter.Managed():
		projected, upstreamID, err := upstream.ProjectManagedBatch(adapter, result, cmp.Or(record.CompletionWindow, "24h"))
		if err != nil {
			return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unknown batch status.")
		}
		if upstreamID != res.UpstreamID {
			return refreshedBatch{}, malformed
		}
		adoption, err := upstream.FilesMetadata(adapter, projected, resourceModel(res), record.LocalInputFileID, record.Metadata)
		if err != nil {
			return refreshedBatch{}, malformed
		}
		return refreshedBatch{state: adoption.State, metadata: adoption.Metadata}, nil
	default:
		if res.Kind != resources.KindStrictBatch && !retainedIdentityMatches(result, res.UpstreamID, maxBytes) {
			return refreshedBatch{}, malformed
		}
		adoption, err := upstream.FilesMetadata(connectors.BatchOpenAI, result, resourceModel(res), record.LocalInputFileID, nil)
		if err != nil {
			return refreshedBatch{}, malformed
		}
		return refreshedBatch{state: adoption.State, metadata: adoption.Metadata}, nil
	}
}

func refreshGeminiManagedBatch(res *resources.Resource, result []byte) (refreshedBatch, *fault.Error) {
	batch, err := upstream.DecodeGeminiBatch(result)
	if err != nil || batch.ID != res.UpstreamID {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini batch identity.")
	}
	base, err := decodeGeminiManagedRecord(res.Metadata)
	if err != nil {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored Gemini batch could not be projected.")
	}
	managed, err := geminiManagedBatchMetadata(result, resourceModel(res), base)
	if errors.Is(err, errGeminiManagedBatchState) {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unknown Gemini batch status.")
	}
	if err != nil {
		return refreshedBatch{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a malformed Gemini batch object.")
	}
	return refreshedBatch{state: managed.status, metadata: managed.metadata}, nil
}

func (s *Server) commitStrictBatchRefresh(ctx, commitCtx context.Context, x *call, res *resources.Resource, state string, result []byte) *fault.Error {
	validated, _, err := strictBatchResult(result, res.UpstreamID)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native batch result.")
	}
	_, contract, err := s.readDurable(ctx, res.Kind, res.APIKeyID, res.ID)
	if err != nil {
		return durableError(err)
	}
	if !strictBatchInputMatches(validated, contract.Effective) {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider changed the batch input file identity.")
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
	return nil
}
