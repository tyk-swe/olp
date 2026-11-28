package openaistate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
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
	window         kernel.BatchWindow
	expected       *int64
	serving        oif.ServingIdentity
	submission     batches.Submission
	manifest       kernel.BatchManifest
	clientMetadata json.RawMessage
}

func (s surface) createBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.Batch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	request, e := s.batchCreateRequest(w, r)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	file, contract, e := s.batchInputFile(r.Context(), authority.ID, request.localFile)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if candidate, exists := x.Snapshot().FindRoute(file.RouteSlug); exists {
		x.Route = &candidate
	}
	p, route, e := s.kernel.ResolveResource(r.Context(), x, authority, file, "batch", kernel.RetainedNewWork)
	if e != nil {
		if refusal := vendorBatchRefusal(x, x.Route); refusal != nil {
			e = refusal
		}
		s.kernel.Refuse(x, w, e)
		return
	}
	x.Route = route
	plan, e := s.planBatch(r.Context(), x, authority, request, file, contract, p, route)
	if e != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.kernel.Refuse(x, w, e)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if e := s.kernel.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	resource, exchange, e := s.submitBatch(ctx, x, authority, p, request, plan)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	out, err := s.kernel.BatchObject(r.Context(), resource)
	if err != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object."))
		return
	}
	s.kernel.WriteStateJSON(w, x, out, exchange)
}

func (s surface) batchCreateRequest(w http.ResponseWriter, r *http.Request) (batchCreateRequest, *fault.Error) {
	body, e := s.kernel.ReadResourceBody(w, r)
	if e != nil {
		return batchCreateRequest{}, e
	}
	source, err := ingress.ParseObjectRequest(body, oif.Limits{MaxBytes: int(s.kernel.Config.MaxBodyBytes)})
	if err != nil || source.Root().Kind() != oif.Object {
		return batchCreateRequest{}, fault.InvalidRequest("invalid_json", "The request body must be one JSON object.", nil)
	}
	endpoint, operation, e := kernel.BatchLineOperation(source)
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

func (s surface) batchInputFile(ctx context.Context, owner, localFile string) (*resources.Resource, *kernel.DurableDocument, *fault.Error) {
	file, contract, err := s.kernel.ReadDurable(ctx, kernel.DurableKind(resources.KindFile, localFile), owner, localFile)
	if errors.Is(err, resources.ErrNotFound) {
		return nil, nil, fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
	}
	if err != nil {
		return nil, nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read.")
	}
	return file, contract, nil
}

func (s surface) planBatch(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, request batchCreateRequest, file *resources.Resource, contract *kernel.DurableDocument, p *kernel.Pin, route *routing.Route) (batchPlan, *fault.Error) {
	window, e := kernel.BatchCompletionWindow(request.source, p.Provider.VendorID)
	if e != nil {
		return batchPlan{}, e
	}
	if e := kernel.BatchEndpointRefusal(p, request.endpoint, "endpoint"); e != nil {
		return batchPlan{}, e
	}
	adapter, ok := p.Provider.Connector().FilesBatchesAdapter()
	if !ok {
		return batchPlan{}, fault.InvalidRequest("target_capability", "The selected target does not support batches.", nil)
	}
	if e := managedBatchShape(request.source, adapter); e != nil {
		return batchPlan{}, e
	}
	if x.Strict() {
		if e := s.strictBatchAdmission(ctx, x, authority, request, file, contract, p); e != nil {
			return batchPlan{}, e
		}
	}
	manifest, e := inputBatchManifest(file, adapter, request.endpoint, x.Strict())
	if e != nil {
		return batchPlan{}, e
	}
	if _, supplied := request.source.Root().Lookup("id"); supplied && adapter != connectors.BatchTogether && adapter != connectors.BatchMistral {
		return batchPlan{}, fault.InvalidRequest("invalid_request", "A new batch cannot supply a provider resource ID.", new("id"))
	}
	if e := s.kernel.UnboundedPinLimits(x.AdmissionAuthority(authority), p, route); e != nil {
		return batchPlan{}, e
	}
	if e := x.GuardrailSurfaceGate(route); e != nil {
		return batchPlan{}, e
	}
	plan := batchPlan{
		id: uuid.New(), adapter: adapter, kind: resources.KindBatch, window: window, manifest: manifest,
		clientMetadata: request.source.Fields()["metadata"],
	}
	if x.Strict() {
		return s.planStrictBatch(x, request, file, contract, p, route, plan)
	}
	return planFilesBatch(request, file, p, plan)
}

func managedBatchShape(source oif.Document, adapter connectors.BatchAdapter) *fault.Error {
	if !adapter.Managed() {
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

func (s surface) strictBatchAdmission(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, request batchCreateRequest, file *resources.Resource, contract *kernel.DurableDocument, p *kernel.Pin) *fault.Error {
	if refusal := strictBatchTargetRefusal(p); refusal != nil {
		return refusal
	}
	if kernel.DurableKind(resources.KindFile, request.localFile) != resources.KindStrictFile || contract == nil || !authority.Policy.AllowProviderState || !s.store.Encrypted() {
		return fault.InvalidRequest("resource_affinity", "Strict batches require a file uploaded under the same strict route and state-enabled key.", new("input_file_id"))
	}
	if e := s.kernel.AuthorizeDurable(ctx, x, authority, file, contract, "batch", kernel.RetainedNewWork); e != nil {
		return e
	}
	if contract.Asset == nil || contract.Asset.Endpoint != request.endpoint || contract.Asset.ItemCount < 1 {
		return fault.InvalidRequest("resource_affinity", "The batch endpoint must match every qualified item in its uploaded file.", new("endpoint"))
	}
	return nil
}

type storedFileManifest struct {
	kernel.BatchManifest
	Qualified *kernel.BatchManifest `json:"batch_manifest"`
}

func inputBatchManifest(file *resources.Resource, adapter connectors.BatchAdapter, endpoint string, strict bool) (kernel.BatchManifest, *fault.Error) {
	if strict || adapter != connectors.BatchOpenAI && !adapter.Managed() {
		return kernel.BatchManifest{}, nil
	}
	var stored storedFileManifest
	decodeErr := json.Unmarshal(file.Metadata, &stored)
	if adapter == connectors.BatchOpenAI {
		if decodeErr != nil || stored.Qualified == nil || !stored.Qualified.Usable(adapter) || stored.Qualified.Endpoint != endpoint {
			return kernel.BatchManifest{}, fault.InvalidRequest("resource_affinity", "Upload a batch file qualified for this model and endpoint before submitting it.", new("input_file_id"))
		}
		return *stored.Qualified, nil
	}
	if decodeErr != nil || !stored.BatchManifest.Usable(adapter) {
		return kernel.BatchManifest{}, fault.NotFoundError("not_found", "The uploaded file has no compatible batch manifest.")
	}
	return stored.BatchManifest, nil
}

func (s surface) planStrictBatch(x *kernel.Call, request batchCreateRequest, file *resources.Resource, contract *kernel.DurableDocument, p *kernel.Pin, route *routing.Route, plan batchPlan) (batchPlan, *fault.Error) {
	template, ok := x.Snapshot().DurableTemplate(route.Slug, p.Target.ID)
	if !ok {
		return batchPlan{}, fault.InvalidRequest("target_capability", "The selected target has no strict batch contract.", nil)
	}
	bound, err := template.BindBatch(request.source, route.Slug, request.localFile, file.UpstreamID)
	if err != nil {
		return batchPlan{}, fault.InvalidRequest("target_capability", "The batch source cannot satisfy this native target contract.", nil)
	}
	endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, "batches", nil)
	if err != nil {
		return batchPlan{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	plan.kind, plan.endpoint, plan.serving = resources.KindStrictBatch, endpoint, bound.Receipt.Serving
	plan.body = bound.Effective.Document().Bytes()
	plan.expected = new(int64(contract.Asset.ItemCount))
	plan.submission = filesSubmission(request, file, plan)
	return plan, nil
}

func planFilesBatch(request batchCreateRequest, file *resources.Resource, p *kernel.Pin, plan batchPlan) (batchPlan, *fault.Error) {
	body, e := filesBatchBody(request, file, p, plan)
	if e != nil {
		return batchPlan{}, e
	}
	path := upstream.JobsPath(plan.adapter, "", false)
	if plan.adapter == connectors.BatchGeminiManaged {
		path = "models/" + url.PathEscape(p.Provider.Connector().Model(p.Model)) + ":batchGenerateContent"
	}
	endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, strings.TrimSuffix(path, "/"), nil)
	if err != nil {
		return batchPlan{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	plan.endpoint, plan.body = endpoint, body
	plan.expected = new(int64(plan.manifest.LineCount))
	if plan.adapter != connectors.BatchGeminiManaged {
		plan.submission = filesSubmission(request, file, plan)
		return plan, nil
	}
	pending, err := kernel.GeminiManagedBatchMetadata([]byte(`{"metadata":{"state":"BATCH_STATE_PENDING"}}`), p.Model, kernel.NewGeminiManagedRecord(request.localFile, plan.manifest, plan.clientMetadata))
	if err != nil {
		return batchPlan{}, fault.ServerError(http.StatusInternalServerError, "internal_error", "The Gemini submission metadata could not be prepared.")
	}
	plan.submission = batches.GeminiManagedSubmission{
		GeminiFile: file.UpstreamID, LocalInputFileID: request.localFile, Endpoint: plan.manifest.Endpoint, Metadata: pending.Metadata,
	}
	return plan, nil
}

func filesSubmission(request batchCreateRequest, file *resources.Resource, plan batchPlan) batches.FilesSubmission {
	submission := batches.FilesSubmission{
		InputFileID: file.UpstreamID, LocalInputFileID: request.localFile, Endpoint: request.endpoint, CompletionWindow: plan.window.Label,
	}
	if plan.adapter == connectors.BatchTogether {
		submission.ClientMetadata = plan.clientMetadata
	}
	return submission
}

func filesBatchBody(request batchCreateRequest, file *resources.Resource, p *kernel.Pin, plan batchPlan) ([]byte, *fault.Error) {
	if plan.adapter == connectors.BatchOpenAI {
		return openAIBatchBody(request.source, file.UpstreamID, p, plan.window)
	}
	return kernel.ManagedBatchRequestBody(request.source, plan.adapter, request.endpoint, plan.window, file.UpstreamID, p.Provider.Connector().Model(p.Model), plan.manifest, plan.id)
}

func openAIBatchBody(source oif.Document, upstreamFile string, p *kernel.Pin, window kernel.BatchWindow) ([]byte, *fault.Error) {
	upstreamWindow := "24h"
	if _, declared := vendors.BatchWindowFor(p.Provider.VendorID); declared {
		upstreamWindow = window.Label
	}
	changes := []oif.Change{
		{Pointer: "/input_file_id", Value: string(kernel.MustJSON(upstreamFile)), Origin: oif.ResourceBinding, Reason: "owner-scoped uploaded file"},
		{Pointer: "/completion_window", Value: string(kernel.MustJSON(upstreamWindow)), Origin: oif.ResourceBinding, Reason: "provider batch completion window"},
	}
	if deployment := p.Provider.Connector().Model(p.Model); p.Provider.Kind == "azure_openai" && deployment != "" {
		changes = append(changes, oif.Change{Pointer: "/model", Value: string(kernel.MustJSON(deployment)), Origin: oif.IdentityBinding, Reason: "selected Azure deployment"})
	}
	effective, err := oif.Apply(source, changes)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_json", "The batch request could not be bound to its provider resource.", nil)
	}
	return effective.Bytes(), nil
}

func (s surface) submitBatch(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, p *kernel.Pin, request batchCreateRequest, plan batchPlan) (*resources.Resource, *kernel.PinnedExchange, *fault.Error) {
	submission, e := s.kernel.BeginBatch(ctx, x, authority.ID, p, kernel.BatchIntent{
		ResourceID: plan.id, Kind: plan.kind, Adapter: plan.adapter, Surface: batches.SurfaceOpenAI,
		Operation: request.operation, Window: plan.window.Length, Expected: plan.expected, Submission: plan.submission,
		Source: request.source, Canonical: request.source.Bytes(), Effective: plan.body, Serving: plan.serving,
	})
	if e != nil {
		return nil, nil, e
	}
	result, exchange, e := s.kernel.SendBatch(ctx, x, p, plan.id, plan.endpoint, plan.body)
	if e != nil {
		return nil, nil, e
	}
	accepted, e := s.acceptFilesBatch(ctx, p, request, plan, result)
	if e != nil {
		return nil, nil, e
	}
	exchange.Complete(kernel.ClassSuccess)
	resource, activationErr := s.kernel.ActivateBatch(ctx, submission, accepted)
	return resource, exchange, activationErr
}

func (s surface) acceptFilesBatch(ctx context.Context, p *kernel.Pin, request batchCreateRequest, plan batchPlan, result []byte) (kernel.AcceptedBatch, *fault.Error) {
	switch {
	case plan.kind == resources.KindStrictBatch:
		return s.acceptStrictBatch(ctx, p, request, plan, result)
	case plan.adapter == connectors.BatchGeminiManaged:
		batch, err := upstream.DecodeGeminiBatch(result)
		if err != nil {
			return kernel.AcceptedBatch{}, s.kernel.BatchSubmissionUnknown(ctx, plan.id, "The provider accepted a batch with an invalid operation name.")
		}
		managed, err := kernel.GeminiManagedBatchMetadata(result, p.Model, kernel.NewGeminiManagedRecord(request.localFile, plan.manifest, plan.clientMetadata))
		if err != nil {
			return kernel.AcceptedBatch{}, s.kernel.MarkBatchUnknown(ctx, plan.id, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid Gemini batch object."))
		}
		return kernel.AcceptedBatch{UpstreamID: batch.ID, State: managed.Status, Metadata: managed.Metadata}, nil
	case plan.adapter.Managed():
		projected, upstreamID, err := upstream.ProjectManagedBatch(plan.adapter, result, plan.window.Label)
		if err != nil {
			return kernel.AcceptedBatch{}, s.kernel.MarkBatchUnknown(ctx, plan.id, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid managed batch result."))
		}
		adoption, err := upstream.FilesMetadata(plan.adapter, projected, p.Model, request.localFile, plan.clientMetadata)
		if err != nil {
			return kernel.AcceptedBatch{}, s.kernel.MarkBatchUnknown(ctx, plan.id, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid managed batch result."))
		}
		return kernel.AcceptedBatch{UpstreamID: upstreamID, State: adoption.State, Metadata: adoption.Metadata}, nil
	default:
		upstreamID, valid := kernel.NativeResourceID(result, s.kernel.Config.MaxResponseBytes)
		adoption, err := upstream.FilesMetadata(connectors.BatchOpenAI, result, p.Model, request.localFile, nil)
		if !valid || err != nil {
			return kernel.AcceptedBatch{}, s.kernel.BatchSubmissionUnknown(ctx, plan.id, "The provider returned a malformed batch object. The batch may have been accepted.")
		}
		return kernel.AcceptedBatch{UpstreamID: upstreamID, State: adoption.State, Metadata: adoption.Metadata, Expected: kernel.BatchExpectedResults(result)}, nil
	}
}

func (s surface) acceptStrictBatch(ctx context.Context, p *kernel.Pin, request batchCreateRequest, plan batchPlan, result []byte) (kernel.AcceptedBatch, *fault.Error) {
	validated, upstreamID, err := kernel.StrictBatchResult(result, "")
	if err != nil || !kernel.StrictBatchInputMatches(validated, plan.body) {
		return kernel.AcceptedBatch{}, s.kernel.BatchSubmissionUnknown(ctx, plan.id, "The provider returned an invalid native batch result. The batch may have been accepted.")
	}
	adoption, err := upstream.FilesMetadata(connectors.BatchOpenAI, result, p.Model, request.localFile, nil)
	if err != nil {
		return kernel.AcceptedBatch{}, s.kernel.BatchSubmissionUnknown(ctx, plan.id, "The provider returned an invalid native batch result. The batch may have been accepted.")
	}
	contract, err := json.Marshal(kernel.DurableDocument{
		Version: resources.DurableContractVersion, Source: request.source.Bytes(), Effective: plan.body,
		Result: result, Binding: p.Model, Serving: plan.serving,
	})
	if err != nil {
		return kernel.AcceptedBatch{}, s.kernel.BatchSubmissionUnknown(ctx, plan.id, "The accepted batch contract could not be encoded.")
	}
	return kernel.AcceptedBatch{UpstreamID: upstreamID, State: adoption.State, Metadata: kernel.DurableMetadata(p.Model), Expected: kernel.BatchExpectedResults(result), Contract: contract}, nil
}
