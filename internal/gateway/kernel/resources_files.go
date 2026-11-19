package kernel

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	slug := strings.TrimSpace(r.Header.Get(StateRouteHeader))
	if slug == "" {
		s.Refuse(x, w, fault.InvalidRequest("missing_required_parameter", "X-OLP-Route is required for file uploads.", nil))
		return
	}
	snapshot := x.request.release.Snapshot()
	route, ok := snapshot.FindRoute(slug)
	if !ok {
		s.Refuse(x, w, fault.ModelNotFound(slug))
		return
	}
	x.Route = &route
	if x.Strict() && !authority.Policy.AllowProviderState {
		s.Refuse(x, w, providerStateForbidden())
		return
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
		s.Refuse(x, w, fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."))
		return
	}
	if e := x.GuardrailSurfaceGate(&route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	form, e := s.ParseMediaForm(w, r, authority.ID, s.Config.MaxMediaBodyBytes, 1)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	defer form.Cleanup()
	purpose := form.Required("purpose")
	file := form.TakeSingleFile("file")
	extra := form.TakeExtensions()
	if err := form.Err(); err != nil {
		s.Refuse(x, w, fault.InvalidRequest("invalid_request", err.Error(), nil))
		return
	}
	if file == nil {
		s.Refuse(x, w, fault.InvalidRequest("missing_required_parameter", "The file field must carry one file.", nil))
		return
	}
	if purpose != "batch" {
		s.uploadRetainedFile(w, r, x, authority, &route, file, purpose, extra)
		return
	}
	if !slices.Contains(route.Operations, "batch") {
		s.Refuse(x, w, fault.InvalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow batch operations.", nil))
		return
	}
	if x.Strict() && !s.Deps.Resources.Encrypted() {
		s.Refuse(x, w, providerStateForbidden())
		return
	}
	var strictSource []byte
	fileFirst := false
	if x.Strict() {
		fields, normalized := form.SourceFields()
		fileFirst = len(fields) == 2 && fields[0].Name == "file"
		if normalized || len(fields) != 2 || len(extra) != 0 || purpose != "batch" ||
			file.ContentType == "" || strings.ContainsAny(file.Filename, "\"\\\r\n") || r.Header.Get("Idempotency-Key") != "" {
			s.Refuse(x, w, fault.InvalidRequest("target_capability", "Strict batch file upload requires one purpose=batch and one typed file field without unqualified options.", nil))
			return
		}
		type sourceField struct {
			Name  string        `json:"name"`
			Value string        `json:"value,omitempty"`
			Asset *durableAsset `json:"asset,omitempty"`
		}
		ordered := make([]sourceField, 0, 2)
		for _, field := range fields {
			if text, ok := field.Value.(staging.Text); ok {
				ordered = append(ordered, sourceField{Name: field.Name, Value: string(text)})
			} else {
				ordered = append(ordered, sourceField{Name: field.Name, Asset: &durableAsset{SHA256: file.Digest, Filename: file.Filename, ContentType: file.ContentType, Size: file.Size}})
			}
		}
		strictSource = MustJSON(ordered)
	}
	form.Disarm()
	defer func() {
		if err := s.Deps.Staging.Remove(file.Handle); err != nil {
			s.log.Warn("file upload cleanup failed", "error", err)
		}
	}()
	p, e := s.selectPin(r.Context(), x, &route, "batch", "unary")
	if e != nil {
		if refusal := fireworksBatchRefusal(x, &route); refusal != nil {
			e = refusal
		}
		s.Refuse(x, w, e)
		return
	}
	if x.Strict() {
		if refusal := strictBatchTargetRefusal(p); refusal != nil {
			s.ReleaseHold(r.Context(), p.Hold)
			s.Refuse(x, w, refusal)
			return
		}
	}
	adapter, _ := p.Provider.Connector().FilesBatchesAdapter()
	uploadFile := file
	uploadPurpose, uploadExtra := purpose, extra
	var manifest batchManifest
	if p.Provider.VendorID == "groq" && purpose != "batch" {
		param := "purpose"
		s.ReleaseHold(r.Context(), p.Hold)
		s.Refuse(x, w, fault.InvalidRequest("unsupported_parameter", "Groq files used by batches require purpose=batch.", &param))
		return
	}
	if adapter.Managed() || adapter == connectors.BatchOpenAI && !x.Strict() && purpose == "batch" {
		if adapter != connectors.BatchOpenAI && (purpose != "batch" || len(extra) > 0) {
			param := "purpose"
			s.ReleaseHold(r.Context(), p.Hold)
			s.Refuse(x, w, fault.InvalidRequest("unsupported_parameter", "Managed batch uploads require purpose=batch and no extra fields.", &param))
			return
		}
		var translated *staging.Part
		translated, manifest, e = s.translateManagedBatchInput(r.Context(), p, &route, file, adapter)
		if e != nil {
			s.ReleaseHold(r.Context(), p.Hold)
			s.Refuse(x, w, e)
			return
		}
		uploadFile = translated
		defer func() {
			if err := s.Deps.Staging.Remove(translated.Handle); err != nil {
				s.log.Warn("translated batch spool cleanup failed", "error", err)
			}
		}()
		if adapter == connectors.BatchTogether {
			uploadPurpose = "batch-api"
			uploadExtra = map[string]string{"file_name": file.Filename, "file_type": "jsonl"}
		}
	}
	var strictServing oif.ServingIdentity
	var strictEndpoint string
	var strictItems int
	if x.Strict() {
		template, ok := x.Snapshot().DurableTemplate(route.Slug, p.Target.ID)
		if !ok {
			s.ReleaseHold(r.Context(), p.Hold)
			s.Refuse(x, w, fault.InvalidRequest("target_capability", "The selected target has no strict batch contract.", nil))
			return
		}
		strictServing = template.Serving()
		var validationErr error
		strictEndpoint, strictItems, validationErr = s.validateBatchInput(file, template.Model(), &p.Provider, &route)
		if validationErr != nil {
			s.ReleaseHold(r.Context(), p.Hold)
			s.Refuse(x, w, fault.InvalidRequest("target_capability", "The batch file has an unqualified item, duplicate identity, or mixed endpoint.", new("file")))
			return
		}
	}
	ctx, cancel := s.StateDeadline(r.Context(), &route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.Refuse(x, w, e)
		return
	}
	var exchange *PinnedExchange
	var body []byte
	var upstreamID string
	var err error
	if adapter == connectors.BatchGeminiManaged {
		var failure *AttemptFailure
		body, upstreamID, exchange, failure = s.uploadGeminiManagedFile(ctx, x, p, uploadFile)
		if failure != nil {
			s.Refuse(x, w, UpstreamError(failure))
			return
		}
	} else {
		filePath := "files"
		if adapter == connectors.BatchTogether {
			filePath = "files/upload"
		}
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, filePath, nil)
		if err != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."))
			return
		}
		resp, failure := s.UploadMultipart(ctx, x, p, endpoint, uploadPurpose, uploadExtra, uploadFile, fileFirst)
		if failure != nil {
			s.Refuse(x, w, UpstreamError(failure))
			return
		}
		defer resp.Body.Close()
		exchange = resp.Exchange

		body, err = resp.ReadBody(s.Config.MaxResponseBytes)
		if err != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."))
			return
		}
		if x.Strict() {
			if _, upstreamID, err = strictResult(body, ""); err != nil {
				s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result."))
				return
			}
		} else {
			var valid bool
			if upstreamID, valid = NativeResourceID(body, s.Config.MaxResponseBytes); !valid {
				s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."))
				return
			}
		}
	}
	provided, state, expires := fileMetadata(body, p.Model)
	metadata, err := uploadedFileMetadata(provided, body, adapter, manifest)
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."))
		return
	}
	exchange.Complete(ClassSuccess)
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	kind := resources.KindFile
	if x.Strict() {
		kind = resources.KindStrictFile
		metadata = durableMetadata(p.Model)
		expires = s.durableExpiry(expires)
	}
	resource := &resources.Resource{
		Kind:               kind,
		APIKeyID:           authority.ID,
		RouteSlug:          route.Slug,
		ProviderID:         p.Provider.ID,
		ProviderRevisionID: p.Provider.RevisionID,
		RouteRevisionID:    route.RevisionID,
		TargetID:           p.Target.ID,
		SlotID:             p.Slot.ID,
		CredentialID:       p.Slot.CredentialID,
		UpstreamID:         upstreamID,
		State:              state,
		Metadata:           metadata,
		ExpiresAt:          expires,
	}
	var res *resources.Resource
	if x.Strict() {
		version := resources.DurableContractVersion
		resource.ContractVersion = &version
		payload, marshalErr := json.Marshal(durableDocument{Version: version, Source: strictSource, Result: body, Binding: p.Model, Serving: strictServing,
			Asset: &durableAsset{SHA256: file.Digest, Filename: file.Filename, ContentType: file.ContentType, Size: file.Size, Endpoint: strictEndpoint, ItemCount: strictItems}})
		if marshalErr != nil {
			s.Refuse(x, w, durableError(marshalErr))
			return
		}
		res, err = s.Deps.Resources.PutDurableContract(commitCtx, resource, payload)
	} else {
		res, err = s.Deps.Resources.Put(commitCtx, resource)
	}
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be stored."))
		return
	}
	var out []byte
	if x.Strict() {
		out, err = durableProjection(body, res.ID, nil)
	} else if adapter.Managed() {
		out, err = s.fileObject(r.Context(), res)
	} else {
		out, err = RewriteID(body, "id", res.ID)
	}
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."))
		return
	}
	s.WriteStateJSON(w, x, out, exchange)
}

func (s *Server) UploadMultipart(ctx context.Context, x *Call, p *Pin, endpoint, purpose string, extra map[string]string, file *staging.Part, fileFirst bool) (*PinnedResponse, *AttemptFailure) {
	pipeR, pipeW := io.Pipe()
	form := multipart.NewWriter(pipeW)
	sendDone := make(chan error, 1)
	go func() {
		failure := func(err error) error {
			pipeW.CloseWithError(err)
			return err
		}
		defer panics.Recover(s.log, "multipart upload", func(err error) { sendDone <- failure(err) })
		writeFile := func() error {
			opened, err := s.Deps.Staging.Open(file.Handle)
			if err != nil {
				return fmt.Errorf("%w: %w", errPinnedUploadRead, err)
			}
			defer opened.File.Close()
			name := opened.Filename
			if name == "" {
				name = "upload"
			}
			header := textproto.MIMEHeader{}
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, stateQuote(name)))
			contentType := opened.Artifact.ContentType
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			header.Set("Content-Type", contentType)
			part, err := form.CreatePart(header)
			if err != nil {
				return err
			}
			_, err = io.Copy(part, pinnedUploadSource{opened.File})
			return err
		}
		if purpose != "" && fileFirst {
			if err := writeFile(); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		if purpose != "" {
			if err := form.WriteField("purpose", purpose); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		for name, value := range extra {
			if err := form.WriteField(name, value); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		if purpose == "" || !fileFirst {
			if err := writeFile(); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		if err := form.Close(); err != nil {
			sendDone <- failure(err)
			return
		}
		sendDone <- pipeW.Close()
	}()
	joined := false
	defer func() {
		_ = pipeR.CloseWithError(io.ErrClosedPipe)
		if !joined {
			<-sendDone
		}
	}()
	exchange := s.beginPinned(ctx, x, p, http.MethodPost)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pipeR)
	if err != nil {
		return nil, exchange.fail(classConnect, nil)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("User-Agent", "olp/gateway")
	req.Header.Set("Accept", "application/json")
	if err := s.applySlotCredential(ctx, x, req, p.Provider.Connector(), p.Slot, nil, &p.Provider, p.Model); err != nil {
		return nil, exchange.fail(classCredential, nil)
	}
	maps.Copy(req.Header, x.PinnedHeaders)
	resp, failure := s.doPinnedRequest(ctx, p, exchange, req, openai.ParseErrorBody)
	// A provider can respond before consuming the body. Keep its rejection as
	// authoritative; only successful headers with an incomplete upload are invalid.
	_ = pipeR.CloseWithError(io.ErrClosedPipe)
	sendErr := <-sendDone
	joined = true
	if failure != nil {
		if failure.Status == 0 && errors.Is(sendErr, errPinnedUploadRead) {
			exchange.fact().LocalRefusal = true
		}
		return nil, failure
	}
	if sendErr != nil {
		exchange.fact().LocalRefusal = errors.Is(sendErr, errPinnedUploadRead)
		_ = resp.Body.Close()
		return nil, exchange.fail(exchange.state.classify(sendErr, false), nil)
	}
	return resp, nil
}

var stateQuoteReplacer = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "", "\x00", "").Replace

func stateQuote(value string) string { return stateQuoteReplacer(value) }

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	limit, after, e := ListArgs(r)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	kinds := []string{resources.KindFile}
	if authority.Policy.AllowProviderState {
		kinds = append(kinds, resources.KindStrictFile)
	}
	// One extra row reports whether another page follows; it is never
	// authorized or projected.
	rows, err := s.Deps.Resources.ListKinds(r.Context(), kinds, authority.ID, limit+1, after)
	if err != nil {
		s.Refuse(x, w, resourceListError(err, "The file list could not be read."))
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]json.RawMessage, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		if retainedFileOperation(row) != "batch" {
			if !authority.Policy.AllowProviderState {
				s.Refuse(x, w, providerStateForbidden())
				return
			}
			if _, _, e := s.ResolveResource(r.Context(), x, authority, row, retainedFileOperation(row), RetainedHousekeeping); e != nil {
				s.Refuse(x, w, e)
				return
			}
		}
		if row.Kind == resources.KindStrictFile {
			_, contract, readErr := s.readDurable(r.Context(), row.Kind, authority.ID, row.ID)
			if readErr != nil || s.authorizeDurable(r.Context(), x, authority, row, contract, "batch", RetainedHousekeeping) != nil {
				s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A file in this list is unavailable."))
				return
			}
		}
		obj, err := s.fileObject(r.Context(), row)
		if err != nil {
			s.Refuse(x, w, fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A file in this list could not be projected."))
			return
		}
		items = append(items, obj)
		ids = append(ids, row.ID)
	}
	s.WriteStateJSON(w, x, listBody(items, ids, hasMore))
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, _ *routing.Route) *fault.Error {
		return s.getPinnedFile(ctx, w, x, authority, res, p)
	})
}

func (s *Server) getPinnedFile(ctx context.Context, w http.ResponseWriter, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin) *fault.Error {
	adapter, e := storedAdapter(res)
	if e != nil {
		return e
	}
	path, e := providerFilePath(res, adapter)
	if e != nil {
		return e
	}
	endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, path, nil)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		if failure.Status == http.StatusNotFound {
			if err := s.Deps.Resources.Tombstone(ctx, res.ID); err != nil {
				return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
			}
			return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
		}
		return UpstreamError(failure)
	}
	defer resp.Body.Close()

	body, err := resp.ReadBody(s.Config.MaxResponseBytes)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	if e := s.verifyProviderFile(res, adapter, body); e != nil {
		return e
	}
	refreshed, err := refreshedFileMetadata(res, body, adapter)
	if errors.Is(err, errProviderFileMetadata) {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned malformed file metadata.")
	}
	if err != nil {
		resp.Exchange.Interrupt(ClassProtocol, true)
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored file mapping is malformed.")
	}
	var out []byte
	if res.Kind == resources.KindStrictFile {
		if _, _, err := strictResult(body, res.UpstreamID); err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result.")
		}
		resp.Exchange.completeRetainedControl()
		_, contract, readErr := s.readDurable(ctx, res.Kind, authority.ID, res.ID)
		if readErr != nil {
			return durableError(readErr)
		}
		contract.Result = body
		payload, marshalErr := json.Marshal(contract)
		if marshalErr != nil {
			return durableError(marshalErr)
		}
		commitCtx, stopCommit := ResourceCommitContext(ctx)
		defer stopCommit()
		if _, _, commitErr := s.Deps.Resources.UpdateDurableContract(commitCtx, res.Kind, authority.ID, res.ID, refreshed.state, payload); commitErr != nil {
			return durableError(commitErr)
		}
		out, err = durableProjection(body, res.ID, nil)
	} else {
		resp.Exchange.completeRetainedControl()
		if err := s.Deps.Resources.Update(ctx, res.ID, refreshed.state, refreshed.metadata, refreshed.expires); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed file state could not be committed.")
		}
		if adapter.Managed() {
			current := *res
			current.State, current.Metadata, current.ExpiresAt = refreshed.state, refreshed.metadata, refreshed.expires
			out, err = s.fileObject(ctx, &current)
		} else {
			out, err = RewriteID(body, "id", res.ID)
		}
	}
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
	}
	s.WriteStateJSON(w, x, out, resp.Exchange)
	return nil
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, RetainedHousekeeping, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, route *routing.Route) *fault.Error {
		writeDeleted := func(exchanges ...*PinnedExchange) {
			s.WriteStateJSON(w, x, MustJSON(map[string]any{"id": res.ID, "object": "file", "deleted": true}), exchanges...)
		}
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		if adapter == connectors.BatchGeminiManaged {
			detached, e := s.detachGeminiResultFile(ctx, res)
			if e != nil {
				return e
			}
			if detached {
				writeDeleted()
				return nil
			}
		} else if res.ParentID != nil {
			if err := s.RequireBatchEvidenceSettled(ctx, *res.ParentID); err != nil {
				return err
			}
		}
		path, e := providerFilePath(res, adapter)
		if e != nil {
			return e
		}
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, path, nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				commitCtx, stopCommit := ResourceCommitContext(ctx)
				defer stopCommit()
				if err := s.Deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
					return fault.ServerError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
				}
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		defer resp.Body.Close()

		// A successful DELETE status is the operation's terminal contract.
		resp.Exchange.completeRetainedControl()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
		commitCtx, stopCommit := ResourceCommitContext(ctx)
		defer stopCommit()
		if err := s.Deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be deleted.")
		}
		writeDeleted(resp.Exchange)
		return nil
	})
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, route *routing.Route) *fault.Error {
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		if adapter == connectors.BatchGeminiManaged {
			return s.geminiManagedFileContent(ctx, w, x, res, p)
		}
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, "files/"+url.PathEscape(res.UpstreamID)+"/content", nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				return fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		defer resp.Body.Close()

		// Stage the bounded upstream body before committing headers. Otherwise
		// a provider overflow would become a successful but truncated download.
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		spool := s.Deps.Staging
		if spool == nil {
			resp.Exchange.Interrupt(ClassProtocol, true)
			return fault.ServerError(http.StatusServiceUnavailable, "media_content_unavailable", "The bounded file spool is unavailable.")
		}
		artifact, err := resp.StageBody(ctx, spool, "file-content", contentType, s.Config.MaxResponseBytes)
		if err != nil {
			var tooLarge *staging.TooLargeError
			if errors.As(err, &tooLarge) {
				return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The provider file exceeded the configured response bound.")
			}
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}

		resp.Exchange.completeRetainedControl()
		defer spool.Remove(artifact.Handle)
		opened, err := spool.Open(artifact.Handle)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "media_content_unavailable", "The staged provider file could not be read.")
		}
		defer opened.File.Close()
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The file response could not be bounded.")
		}
		SetFileDownloadHeaders(w.Header(), contentType, artifact.ContentLength)
		if res.Kind == resources.KindStrictFile {
			w.Header().Set("X-OLP-Content-SHA256", artifact.Digest)
		}
		x.ResponseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := CopyResourceContent(w, opened.File, artifact.ContentLength)
		if copyErr != nil || written != artifact.ContentLength {
			x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The file response ended before all bytes were delivered.")
			resp.Exchange.Delivery(DeliveryState(written))
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

func (s *Server) fileCall(w http.ResponseWriter, r *http.Request, use RetainedUse, op func(context.Context, *Call, keyauthority.Authority, *resources.Resource, *Pin, *routing.Route) *fault.Error) {
	x, authority, done := s.StateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	kind := durableKind(resources.KindFile, r.PathValue("id"))
	if kind == resources.KindStrictFile && !authority.Policy.AllowProviderState {
		s.Refuse(x, w, providerStateForbidden())
		return
	}
	res, contract, err := s.readDurable(r.Context(), kind, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No file with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."))
		return
	}
	if operation := retainedFileOperation(res); operation != "batch" {
		if !authority.Policy.AllowProviderState {
			s.Refuse(x, w, providerStateForbidden())
			return
		}
		x.operationOverride = operation
		if res.State == "submitting" || res.State == "submission_unknown" || res.State == "not_accepted" {
			s.Refuse(x, w, fault.ServerError(http.StatusConflict, "submission_unknown", "The file upload has no reconciled native identity yet."))
			return
		}
		x.retained = retainedOperation{use: RetainedHousekeeping, usage: retainedTokenlessUsage}
		p, route, e := s.ResolveResource(r.Context(), x, authority, res, operation, use)
		if e != nil {
			s.Refuse(x, w, e)
			return
		}
		x.Route = route
		ctx, cancel := s.StateDeadline(r.Context(), route)
		defer cancel()
		defer s.retainedSettle(ctx, x, p)
		if e := s.admitRetainedRequest(ctx, x, authority, p, route, false); e != nil {
			s.Refuse(x, w, e)
			return
		}
		if e := op(ctx, x, authority, res, p, route); e != nil {
			s.Refuse(x, w, e)
		}
		return
	}
	p, route, e := s.admitDurable(r.Context(), x, authority, res, contract, "batch", use)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	x.Route = route
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if e := s.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.Refuse(x, w, e)
	}
}

func providerFilePath(res *resources.Resource, adapter connectors.BatchAdapter) (string, *fault.Error) {
	if adapter != connectors.BatchGeminiManaged {
		return "files/" + url.PathEscape(res.UpstreamID), nil
	}
	path, err := managedGeminiFilePath(res)
	if err != nil {
		return "", fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	return path, nil
}

func (s *Server) verifyProviderFile(res *resources.Resource, adapter connectors.BatchAdapter, body []byte) *fault.Error {
	if adapter == connectors.BatchGeminiManaged {
		var object struct {
			Name string `json:"name"`
		}
		name, err := geminiManagedFileName(res)
		if err != nil || json.Unmarshal(body, &object) != nil || object.Name != name {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini file name.")
		}
		return nil
	}
	if res.Kind != resources.KindStrictFile && !retainedIdentityMatches(body, res.UpstreamID, s.Config.MaxResponseBytes) {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
	}
	return nil
}

func (s *Server) detachGeminiResultFile(ctx context.Context, res *resources.Resource) (bool, *fault.Error) {
	record, err := decodeManagedFileRecord(res.Metadata)
	if err != nil {
		return false, fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	if record.Role != "output" && record.Role != "error" {
		return false, nil
	}
	name := cmp.Or(record.ResponsesFile, res.UpstreamID)
	commitCtx, stop := ResourceCommitContext(ctx)
	defer stop()
	detached, err := s.Deps.Resources.DetachGeminiResultView(commitCtx, res, name)
	if errors.Is(err, resources.ErrNotFound) {
		return false, fault.NotFoundError("not_found", "No file with this identifier exists for this key.")
	}
	if err != nil {
		return false, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The result file dependencies could not be read.")
	}
	if detached {
		return true, nil
	}
	pending, err := s.Deps.Batches.GeminiResultPending(ctx, res, name)
	if err != nil {
		return false, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The result file dependencies could not be read.")
	}
	if pending {
		return false, fault.ServerError(http.StatusConflict, "batch_settlement_pending", "The result file is still needed for settlement; retry after settlement closes.")
	}
	return false, nil
}

func uploadedFileMetadata(metadata json.RawMessage, body []byte, adapter connectors.BatchAdapter, manifest batchManifest) (json.RawMessage, error) {
	switch {
	case adapter.Managed():
		return managedFileMetadata(metadata, body, adapter, manifest, "input")
	case manifest.Adapter == connectors.BatchOpenAI:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(metadata, &fields); err != nil {
			return nil, err
		}
		fields["batch_manifest"] = MustJSON(manifest)
		return json.Marshal(fields)
	default:
		return metadata, nil
	}
}
