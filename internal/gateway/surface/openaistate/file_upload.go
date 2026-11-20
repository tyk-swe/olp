package openaistate

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
)

type uploadFields struct {
	purpose string
	file    *staging.Part
	extra   map[string]string
}

type strictUploadSource struct {
	document  []byte
	fileFirst bool
}

type providerUpload struct {
	file     *staging.Part
	purpose  string
	extra    map[string]string
	manifest kernel.BatchManifest
}

type strictBatchContract struct {
	serving  oif.ServingIdentity
	endpoint string
	items    int
}

type providerFile struct {
	body       []byte
	upstreamID string
	exchange   *kernel.PinnedExchange
}

type batchFileUpload struct {
	server    *kernel.Server
	x         *kernel.Call
	authority keyauthority.Authority
	route     *routing.Route
	pin       *kernel.Pin
	adapter   connectors.BatchAdapter
	fields    uploadFields
	source    strictUploadSource
}

func (s surface) uploadFile(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	route, e := uploadRoute(r, x)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	x.Route = route
	if e := s.admitUpload(x, authority, route); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	form, e := s.kernel.ParseMediaForm(w, r, authority.ID, s.kernel.Config.MaxMediaBodyBytes, 1)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	defer form.Cleanup()
	fields, e := readUploadFields(form)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	if fields.purpose != "batch" {
		s.kernel.UploadRetainedFile(w, r, x, authority, route, fields.file, fields.purpose, fields.extra)
		return
	}
	s.uploadBatchFile(w, r, x, authority, route, form, fields)
}

func uploadRoute(r *http.Request, x *kernel.Call) (*routing.Route, *fault.Error) {
	slug := strings.TrimSpace(r.Header.Get(kernel.StateRouteHeader))
	if slug == "" {
		return nil, fault.InvalidRequest("missing_required_parameter", "X-OLP-Route is required for file uploads.", nil)
	}
	route, ok := x.Snapshot().FindRoute(slug)
	if !ok {
		return nil, fault.ModelNotFound(slug)
	}
	return &route, nil
}

func (s surface) admitUpload(x *kernel.Call, authority keyauthority.Authority, route *routing.Route) *fault.Error {
	if x.Strict() && !authority.Policy.AllowProviderState {
		return kernel.ProviderStateForbidden()
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.kernel.Now()) {
		return fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
	}
	return x.GuardrailSurfaceGate(route)
}

func readUploadFields(form *staging.Form) (uploadFields, *fault.Error) {
	fields := uploadFields{purpose: form.Required("purpose"), file: form.TakeSingleFile("file"), extra: form.TakeExtensions()}
	if err := form.Err(); err != nil {
		return uploadFields{}, fault.InvalidRequest("invalid_request", err.Error(), nil)
	}
	if fields.file == nil {
		return uploadFields{}, fault.InvalidRequest("missing_required_parameter", "The file field must carry one file.", nil)
	}
	return fields, nil
}

func (s surface) uploadBatchFile(w http.ResponseWriter, r *http.Request, x *kernel.Call, authority keyauthority.Authority, route *routing.Route, form *staging.Form, fields uploadFields) {
	if !slices.Contains(route.Operations, "batch") {
		s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow batch operations.", nil))
		return
	}
	if x.Strict() && !s.kernel.Deps.Resources.Encrypted() {
		s.kernel.Refuse(x, w, kernel.ProviderStateForbidden())
		return
	}
	source, e := readStrictUploadSource(x, r, form, fields)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	form.Disarm()
	defer s.unstage(fields.file, "file upload cleanup failed")
	p, e := s.batchUploadPin(r.Context(), x, route)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	adapter, _ := p.Provider.Connector().FilesBatchesAdapter()
	u := batchFileUpload{server: s.kernel, x: x, authority: authority, route: route, pin: p, adapter: adapter, fields: fields, source: source}
	sending, e := u.translated(r.Context())
	if e != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.kernel.Refuse(x, w, e)
		return
	}
	if sending.file != fields.file {
		defer s.unstage(sending.file, "translated batch spool cleanup failed")
	}
	contract, e := u.contract()
	if e != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.kernel.Refuse(x, w, e)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if e := s.kernel.ReserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	sent, e := u.send(ctx, sending)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	kept, e := u.keep(ctx, sending.manifest, contract, sent)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	out, err := u.object(r.Context(), sent.body, kept)
	if err != nil {
		s.kernel.Refuse(x, w, malformedFileObject())
		return
	}
	s.kernel.WriteStateJSON(w, x, out, sent.exchange)
}

func (s surface) unstage(part *staging.Part, warning string) {
	if err := s.kernel.Deps.Staging.Remove(part.Handle); err != nil {
		s.kernel.Log.Warn(warning, "error", err)
	}
}

func malformedFileObject() *fault.Error {
	return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
}

type strictSourceField struct {
	Name  string               `json:"name"`
	Value string               `json:"value,omitempty"`
	Asset *kernel.DurableAsset `json:"asset,omitempty"`
}

func readStrictUploadSource(x *kernel.Call, r *http.Request, form *staging.Form, fields uploadFields) (strictUploadSource, *fault.Error) {
	if !x.Strict() {
		return strictUploadSource{}, nil
	}
	parts, normalized := form.SourceFields()
	file := fields.file
	if normalized || len(parts) != 2 || len(fields.extra) != 0 ||
		file.ContentType == "" || strings.ContainsAny(file.Filename, "\"\\\r\n") || r.Header.Get("Idempotency-Key") != "" {
		return strictUploadSource{}, fault.InvalidRequest("target_capability", "Strict batch file upload requires one purpose=batch and one typed file field without unqualified options.", nil)
	}
	ordered := make([]strictSourceField, len(parts))
	for i, part := range parts {
		if text, ok := part.Value.(staging.Text); ok {
			ordered[i] = strictSourceField{Name: part.Name, Value: string(text)}
			continue
		}
		ordered[i] = strictSourceField{Name: part.Name, Asset: &kernel.DurableAsset{SHA256: file.Digest, Filename: file.Filename, ContentType: file.ContentType, Size: file.Size}}
	}
	return strictUploadSource{document: kernel.MustJSON(ordered), fileFirst: parts[0].Name == "file"}, nil
}

func (s surface) batchUploadPin(ctx context.Context, x *kernel.Call, route *routing.Route) (*kernel.Pin, *fault.Error) {
	p, e := s.kernel.SelectPin(ctx, x, route, "batch", "unary")
	if e != nil {
		return nil, cmp.Or(kernel.FireworksBatchRefusal(x, route), e)
	}
	if !x.Strict() {
		return p, nil
	}
	if refusal := kernel.StrictBatchTargetRefusal(p); refusal != nil {
		s.kernel.ReleaseHold(ctx, p.Hold)
		return nil, refusal
	}
	return p, nil
}

func (u batchFileUpload) translated(ctx context.Context) (providerUpload, *fault.Error) {
	file, extra := u.fields.file, u.fields.extra
	if !u.adapter.Managed() && (u.adapter != connectors.BatchOpenAI || u.x.Strict()) {
		return providerUpload{file: file, purpose: "batch", extra: extra}, nil
	}
	if u.adapter != connectors.BatchOpenAI && len(extra) > 0 {
		return providerUpload{}, fault.InvalidRequest("unsupported_parameter", "Managed batch uploads require purpose=batch and no extra fields.", new("purpose"))
	}
	translated, manifest, e := u.server.TranslateManagedBatchInput(ctx, u.pin, u.route, file, u.adapter)
	if e != nil {
		return providerUpload{}, e
	}
	if u.adapter == connectors.BatchTogether {
		return providerUpload{file: translated, purpose: "batch-api", extra: map[string]string{"file_name": file.Filename, "file_type": "jsonl"}, manifest: manifest}, nil
	}
	return providerUpload{file: translated, purpose: "batch", extra: extra, manifest: manifest}, nil
}

func (u batchFileUpload) contract() (strictBatchContract, *fault.Error) {
	if !u.x.Strict() {
		return strictBatchContract{}, nil
	}
	template, ok := u.x.Snapshot().DurableTemplate(u.route.Slug, u.pin.Target.ID)
	if !ok {
		return strictBatchContract{}, fault.InvalidRequest("target_capability", "The selected target has no strict batch contract.", nil)
	}
	endpoint, items, err := u.server.ValidateBatchInput(u.fields.file, template.Model(), &u.pin.Provider, u.route)
	if err != nil {
		return strictBatchContract{}, fault.InvalidRequest("target_capability", "The batch file has an unqualified item, duplicate identity, or mixed endpoint.", new("file"))
	}
	return strictBatchContract{serving: template.Serving(), endpoint: endpoint, items: items}, nil
}

func (u batchFileUpload) send(ctx context.Context, sending providerUpload) (providerFile, *fault.Error) {
	s, p := u.server, u.pin
	if u.adapter == connectors.BatchGeminiManaged {
		body, upstreamID, exchange, failure := s.UploadGeminiManagedFile(ctx, u.x, p, sending.file)
		if failure != nil {
			return providerFile{}, kernel.UpstreamError(failure)
		}
		return providerFile{body: body, upstreamID: upstreamID, exchange: exchange}, nil
	}
	filePath := "files"
	if u.adapter == connectors.BatchTogether {
		filePath = "files/upload"
	}
	endpoint, err := kernel.ResourceURL(p.Provider.Connector(), p.Model, filePath, nil)
	if err != nil {
		return providerFile{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.UploadMultipart(ctx, u.x, p, endpoint, sending.purpose, sending.extra, sending.file, u.source.fileFirst)
	if failure != nil {
		return providerFile{}, kernel.UpstreamError(failure)
	}
	defer resp.Body.Close()
	body, err := resp.ReadBody(s.Config.MaxResponseBytes)
	if err != nil {
		return providerFile{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	if u.x.Strict() {
		_, upstreamID, err := kernel.StrictResult(body, "")
		if err != nil {
			return providerFile{}, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result.")
		}
		return providerFile{body: body, upstreamID: upstreamID, exchange: resp.Exchange}, nil
	}
	upstreamID, valid := kernel.NativeResourceID(body, s.Config.MaxResponseBytes)
	if !valid {
		return providerFile{}, malformedFileObject()
	}
	return providerFile{body: body, upstreamID: upstreamID, exchange: resp.Exchange}, nil
}

func (u batchFileUpload) keep(ctx context.Context, manifest kernel.BatchManifest, contract strictBatchContract, sent providerFile) (*resources.Resource, *fault.Error) {
	s, p := u.server, u.pin
	provided, state, expires := kernel.FileMetadata(sent.body, p.Model)
	metadata, err := uploadedFileMetadata(provided, sent.body, u.adapter, manifest)
	if err != nil {
		return nil, malformedFileObject()
	}
	sent.exchange.Complete(kernel.ClassSuccess)
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	defer stopCommit()
	if !u.x.Strict() {
		kept, err := s.Deps.Resources.Put(commitCtx, u.resource(resources.KindFile, sent.upstreamID, state, metadata, expires, nil))
		if err != nil {
			return nil, unstoredFileMapping()
		}
		return kept, nil
	}
	file, version := u.fields.file, resources.DurableContractVersion
	payload, err := json.Marshal(kernel.DurableDocument{Version: version, Source: u.source.document, Result: sent.body, Binding: p.Model, Serving: contract.serving,
		Asset: &kernel.DurableAsset{SHA256: file.Digest, Filename: file.Filename, ContentType: file.ContentType, Size: file.Size, Endpoint: contract.endpoint, ItemCount: contract.items}})
	if err != nil {
		return nil, kernel.DurableError(err)
	}
	strict := u.resource(resources.KindStrictFile, sent.upstreamID, state, kernel.DurableMetadata(p.Model), s.DurableExpiry(expires), &version)
	kept, err := s.Deps.Resources.PutDurableContract(commitCtx, strict, payload)
	if err != nil {
		return nil, unstoredFileMapping()
	}
	return kept, nil
}

func (u batchFileUpload) resource(kind, upstreamID, state string, metadata json.RawMessage, expires *time.Time, version *string) *resources.Resource {
	p := u.pin
	return &resources.Resource{
		Kind:               kind,
		APIKeyID:           u.authority.ID,
		RouteSlug:          u.route.Slug,
		ProviderID:         p.Provider.ID,
		ProviderRevisionID: p.Provider.RevisionID,
		RouteRevisionID:    u.route.RevisionID,
		TargetID:           p.Target.ID,
		SlotID:             p.Slot.ID,
		CredentialID:       p.Slot.CredentialID,
		UpstreamID:         upstreamID,
		State:              state,
		Metadata:           metadata,
		ExpiresAt:          expires,
		ContractVersion:    version,
	}
}

func unstoredFileMapping() *fault.Error {
	return fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be stored.")
}

func (u batchFileUpload) object(ctx context.Context, body []byte, kept *resources.Resource) ([]byte, error) {
	if u.x.Strict() {
		return kernel.DurableProjection(body, kept.ID, nil)
	}
	if u.adapter.Managed() {
		return u.server.FileObject(ctx, kept)
	}
	return kernel.RewriteID(body, "id", kept.ID)
}
