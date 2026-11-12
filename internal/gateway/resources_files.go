package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
)

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.release(r.Context())
	slug := strings.TrimSpace(r.Header.Get(stateRouteHeader))
	if slug == "" {
		s.stateFail(x, w, invalidRequest("missing_required_parameter", "X-OLP-Route is required for file uploads.", nil), x.family)
		return
	}
	snapshot := x.request.release.Snapshot()
	route, ok := snapshot.FindRoute(slug)
	if !ok {
		s.stateFail(x, w, modelNotFound(slug), x.family)
		return
	}
	x.route = &route
	if x.strict() && !authority.Policy.AllowProviderState {
		s.stateFail(x, w, providerStateForbidden(), x.family)
		return
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		s.stateFail(x, w, permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."), x.family)
		return
	}
	if e := x.guardrailSurfaceGate(&route); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	form, e := s.parseMediaForm(w, r, authority.ID, s.cfg.MaxMediaBodyBytes, 1)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	defer form.Cleanup()
	purpose := form.Required("purpose")
	file := form.TakeSingleFile("file")
	extra := form.TakeExtensions()
	if err := form.Err(); err != nil {
		s.stateFail(x, w, invalidRequest("invalid_request", err.Error(), nil), x.family)
		return
	}
	if file == nil {
		s.stateFail(x, w, invalidRequest("missing_required_parameter", "The file field must carry one file.", nil), x.family)
		return
	}
	if purpose != "batch" {
		s.uploadRetainedFile(w, r, x, authority, &route, file, purpose, extra)
		return
	}
	if !slices.Contains(route.Operations, "batch") {
		s.stateFail(x, w, invalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow batch operations.", nil), x.family)
		return
	}
	if x.strict() && !s.deps.Resources.Encrypted() {
		s.stateFail(x, w, providerStateForbidden(), x.family)
		return
	}
	var strictSource []byte
	fileFirst := false
	if x.strict() {
		fields, normalized := form.SourceFields()
		fileFirst = len(fields) == 2 && fields[0].Name == "file"
		if normalized || len(fields) != 2 || len(extra) != 0 || purpose != "batch" ||
			file.ContentType == "" || strings.ContainsAny(file.Filename, "\"\\\r\n") || r.Header.Get("Idempotency-Key") != "" {
			s.stateFail(x, w, invalidRequest("target_capability", "Strict batch file upload requires one purpose=batch and one typed file field without unqualified options.", nil), x.family)
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
		strictSource = mustJSON(ordered)
	}
	form.Disarm()
	defer func() {
		if err := s.deps.Staging.Remove(file.Handle); err != nil {
			s.log.Warn("file upload cleanup failed", "error", err)
		}
	}()
	p, e := s.selectPin(r.Context(), x, &route, "batch", "unary")
	if e != nil {
		if refusal := fireworksBatchRefusal(x, &route); refusal != nil {
			e = refusal
		}
		s.stateFail(x, w, e, x.family)
		return
	}
	if x.strict() {
		if refusal := strictBatchTargetRefusal(p); refusal != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, refusal, x.family)
			return
		}
	}
	adapter, _ := p.provider.Connector().FilesBatchesAdapter()
	uploadFile := file
	uploadPurpose, uploadExtra := purpose, extra
	var manifest batchManifest
	if p.provider.VendorID == "groq" && purpose != "batch" {
		param := "purpose"
		s.releaseHold(r.Context(), p.hold)
		s.stateFail(x, w, invalidRequest("unsupported_parameter", "Groq files used by batches require purpose=batch.", &param), x.family)
		return
	}
	if adapter.Managed() || adapter == connectors.BatchOpenAI && !x.strict() && purpose == "batch" {
		if adapter != connectors.BatchOpenAI && (purpose != "batch" || len(extra) > 0) {
			param := "purpose"
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("unsupported_parameter", "Managed batch uploads require purpose=batch and no extra fields.", &param), x.family)
			return
		}
		var translated *staging.Part
		translated, manifest, e = s.translateManagedBatchInput(r.Context(), p, &route, file, adapter)
		if e != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, e, x.family)
			return
		}
		uploadFile = translated
		defer func() {
			if err := s.deps.Staging.Remove(translated.Handle); err != nil {
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
	if x.strict() {
		template, ok := x.snapshot().DurableTemplate(route.Slug, p.target.ID)
		if !ok {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("target_capability", "The selected target has no strict batch contract.", nil), x.family)
			return
		}
		strictServing = template.Serving()
		var validationErr error
		strictEndpoint, strictItems, validationErr = s.validateBatchInput(file, template.Model(), &p.provider, &route)
		if validationErr != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("target_capability", "The batch file has an unqualified item, duplicate identity, or mixed endpoint.", strPtr("file")), x.family)
			return
		}
	}
	ctx, cancel := s.stateDeadline(r.Context(), &route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	var body []byte
	var upstreamID string
	var err error
	if adapter == connectors.BatchGeminiManaged {
		var failure *attemptFailure
		body, upstreamID, failure = s.uploadGeminiManagedFile(ctx, x, p, uploadFile)
		if failure != nil {
			x.dispatched = failure.dispatched
			s.stateFail(x, w, upstreamError(failure), x.family)
			return
		}
	} else {
		filePath := "files"
		if adapter == connectors.BatchTogether {
			filePath = "files/upload"
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, filePath, nil)
		if err != nil {
			s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."), x.family)
			return
		}
		resp, failure := s.uploadMultipart(ctx, x, p, endpoint, uploadPurpose, uploadExtra, uploadFile, fileFirst)
		if failure != nil {
			x.dispatched = failure.dispatched
			s.stateFail(x, w, upstreamError(failure), x.family)
			return
		}
		defer resp.Body.Close()
		x.dispatched = true
		body, err = readBounded(resp.Body, s.cfg.MaxResponseBytes)
		if err != nil {
			s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."), x.family)
			return
		}
		if x.strict() {
			if _, upstreamID, err = strictResult(body, ""); err != nil {
				s.stateFail(x, w, serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result."), x.family)
				return
			}
		} else {
			var valid bool
			if upstreamID, valid = nativeResourceID(body, s.cfg.MaxResponseBytes); !valid {
				s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."), x.family)
				return
			}
		}
	}
	x.completePinned()
	provided, state, expires := fileMetadata(body, p.model)
	metadata, err := uploadedFileMetadata(provided, body, adapter, manifest)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."), x.family)
		return
	}
	commitCtx, stopCommit := resourceCommitContext(ctx)
	defer stopCommit()
	kind := resources.KindFile
	if x.strict() {
		kind = resources.KindStrictFile
		metadata = durableMetadata(p.model)
		expires = s.durableExpiry(expires)
	}
	resource := &resources.Resource{
		Kind:               kind,
		APIKeyID:           authority.ID,
		RouteSlug:          route.Slug,
		ProviderID:         p.provider.ID,
		ProviderRevisionID: p.provider.RevisionID,
		RouteRevisionID:    route.RevisionID,
		TargetID:           p.target.ID,
		SlotID:             p.slot.ID,
		CredentialID:       p.slot.CredentialID,
		UpstreamID:         upstreamID,
		State:              state,
		Metadata:           metadata,
		ExpiresAt:          expires,
	}
	var res *resources.Resource
	if x.strict() {
		version := resources.DurableContractVersion
		resource.ContractVersion = &version
		payload, marshalErr := json.Marshal(durableDocument{Version: version, Source: strictSource, Result: body, Binding: p.model, Serving: strictServing,
			Asset: &durableAsset{SHA256: file.Digest, Filename: file.Filename, ContentType: file.ContentType, Size: file.Size, Endpoint: strictEndpoint, ItemCount: strictItems}})
		if marshalErr != nil {
			s.stateFail(x, w, durableError(marshalErr), x.family)
			return
		}
		res, err = s.deps.Resources.PutDurableContract(commitCtx, resource, payload)
	} else {
		res, err = s.deps.Resources.Put(commitCtx, resource)
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be stored."), x.family)
		return
	}
	var out []byte
	if x.strict() {
		out, err = durableProjection(body, res.ID, nil)
	} else if adapter.Managed() {
		out, err = s.fileObject(r.Context(), res)
	} else {
		out, err = rewriteID(body, "id", res.ID)
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."), x.family)
		return
	}
	s.writeStateJSON(w, x, out)
}

func (s *Server) uploadMultipart(ctx context.Context, x *call, p *pin, endpoint, purpose string, extra map[string]string, file *staging.Part, fileFirst bool) (*http.Response, *attemptFailure) {
	pipeR, pipeW := io.Pipe()
	form := multipart.NewWriter(pipeW)
	sendDone := make(chan error, 1)
	go func() {
		failure := func(err error) error {
			pipeW.CloseWithError(err)
			return err
		}
		writeFile := func() error {
			opened, err := s.deps.Staging.Open(file.Handle)
			if err != nil {
				return err
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
			_, err = io.Copy(part, opened.File)
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
		return nil, exchange.fail(s, x, classConnect, nil)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("User-Agent", "olp/gateway")
	req.Header.Set("Accept", "application/json")
	if err := s.applySlotCredential(ctx, x, req, p.provider.Connector(), p.slot, nil, &p.provider, p.model); err != nil {
		return nil, exchange.fail(s, x, classCredential, nil)
	}
	if x.anthropicFilesBeta {
		req.Header.Set("Anthropic-Beta", anthropicFilesBeta)
	}
	resp, failure := s.doPinnedRequest(ctx, x, p, exchange, req, openai.ParseErrorBody)
	if failure != nil {
		return nil, failure
	}
	// A provider can respond before consuming the body. Keep its rejection as
	// authoritative; only successful headers with an incomplete upload are invalid.
	_ = pipeR.CloseWithError(io.ErrClosedPipe)
	sendErr := <-sendDone
	joined = true
	if sendErr != nil {
		_ = resp.Body.Close()
		return nil, exchange.fail(s, x, exchange.state.classify(sendErr, false), nil)
	}
	return resp, nil
}

var stateQuoteReplacer = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "", "\x00", "").Replace

func stateQuote(value string) string { return stateQuoteReplacer(value) }

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.release(r.Context())
	limit, after, e := listArgs(r)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	kinds := []string{resources.KindFile}
	if authority.Policy.AllowProviderState {
		kinds = append(kinds, resources.KindStrictFile)
	}
	// One extra row reports whether another page follows; it is never
	// authorized or projected.
	rows, err := s.deps.Resources.ListKinds(r.Context(), kinds, authority.ID, limit+1, after)
	if err != nil {
		s.stateFail(x, w, resourceListError(err, "The file list could not be read."), x.family)
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
				s.stateFail(x, w, providerStateForbidden(), x.family)
				return
			}
			if _, _, e := s.resolveResource(r.Context(), x, authority, row, retainedFileOperation(row), retainedHousekeeping); e != nil {
				s.stateFail(x, w, e, x.family)
				return
			}
		}
		if row.Kind == resources.KindStrictFile {
			_, contract, readErr := s.readDurable(r.Context(), row.Kind, authority.ID, row.ID)
			if readErr != nil || s.authorizeDurable(r.Context(), x, authority, row, contract, "batch", retainedHousekeeping) != nil {
				s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A file in this list is unavailable."), x.family)
				return
			}
		}
		obj, err := s.fileObject(r.Context(), row)
		if err != nil {
			s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A file in this list could not be projected."), x.family)
			return
		}
		items = append(items, obj)
		ids = append(ids, row.ID)
	}
	s.writeStateJSON(w, x, listBody(items, ids, hasMore))
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, authority keyauthority.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		path, e := providerFilePath(res, adapter)
		if e != nil {
			return e
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, path, nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				if err := s.deps.Resources.Tombstone(ctx, res.ID); err != nil {
					return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
				}
				return notFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		x.dispatched = true
		body, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		if e := s.verifyProviderFile(res, adapter, body); e != nil {
			return e
		}
		refreshed, err := refreshedFileMetadata(res, body)
		if err != nil {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The stored file mapping is malformed.")
		}
		var out []byte
		if res.Kind == resources.KindStrictFile {
			if _, _, err := strictResult(body, res.UpstreamID); err != nil {
				return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result.")
			}
			x.completeFileControl()
			_, contract, readErr := s.readDurable(ctx, res.Kind, authority.ID, res.ID)
			if readErr != nil {
				return durableError(readErr)
			}
			contract.Result = body
			payload, marshalErr := json.Marshal(contract)
			if marshalErr != nil {
				return durableError(marshalErr)
			}
			commitCtx, stopCommit := resourceCommitContext(ctx)
			defer stopCommit()
			if _, _, commitErr := s.deps.Resources.UpdateDurableContract(commitCtx, res.Kind, authority.ID, res.ID, refreshed.state, payload); commitErr != nil {
				return durableError(commitErr)
			}
			out, err = durableProjection(body, res.ID, nil)
		} else {
			x.completeFileControl()
			if err := s.deps.Resources.Update(ctx, res.ID, refreshed.state, refreshed.metadata, refreshed.expires); err != nil {
				return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed file state could not be committed.")
			}
			if adapter.Managed() {
				current := *res
				current.State, current.Metadata, current.ExpiresAt = refreshed.state, refreshed.metadata, refreshed.expires
				out, err = s.fileObject(ctx, &current)
			} else {
				out, err = rewriteID(body, "id", res.ID)
			}
		}
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
		}
		s.writeStateJSON(w, x, out)
		return nil
	})
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, authority keyauthority.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
		writeDeleted := func() {
			s.writeStateJSON(w, x, mustJSON(map[string]any{"id": res.ID, "object": "file", "deleted": true}))
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
			if err := s.requireBatchEvidenceSettled(ctx, *res.ParentID); err != nil {
				return err
			}
		}
		path, e := providerFilePath(res, adapter)
		if e != nil {
			return e
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, path, nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				commitCtx, stopCommit := resourceCommitContext(ctx)
				defer stopCommit()
				if err := s.deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
					return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
				}
				return notFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		x.dispatched = true
		// A successful DELETE status is the operation's terminal contract.
		x.completeFileControl()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
		commitCtx, stopCommit := resourceCommitContext(ctx)
		defer stopCommit()
		if err := s.deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
			return serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be deleted.")
		}
		writeDeleted()
		return nil
	})
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, authority keyauthority.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
		adapter, e := storedAdapter(res)
		if e != nil {
			return e
		}
		if adapter == connectors.BatchGeminiManaged {
			return s.geminiManagedFileContent(ctx, w, x, res, p)
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "files/"+url.PathEscape(res.UpstreamID)+"/content", nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				return notFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		x.dispatched = true
		// Stage the bounded upstream body before committing headers. Otherwise
		// a provider overflow would become a successful but truncated download.
		contentType := resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		spool := s.deps.Staging
		if spool == nil {
			return serverError(http.StatusServiceUnavailable, "media_content_unavailable", "The bounded file spool is unavailable.")
		}
		artifact, err := spool.Put(ctx, staging.Upload{
			Filename: "file-content", ContentType: contentType,
			MaximumLength: s.cfg.MaxResponseBytes, Body: resp.Body,
		})
		if err != nil {
			if len(x.facts) > 0 {
				fact := &x.facts[len(x.facts)-1]
				fact.Class = classProtocol
			}
			var tooLarge *staging.TooLargeError
			if errors.As(err, &tooLarge) {
				return serverError(http.StatusBadGateway, "upstream_response_too_large", "The provider file exceeded the configured response bound.")
			}
			return serverError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}
		x.completeFileControl()
		defer spool.Remove(artifact.Handle)
		opened, err := spool.Open(artifact.Handle)
		if err != nil {
			return serverError(http.StatusBadGateway, "media_content_unavailable", "The staged provider file could not be read.")
		}
		defer opened.File.Close()
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
			return serverError(http.StatusInternalServerError, "internal_error", "The file response could not be bounded.")
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", fmt.Sprint(artifact.ContentLength))
		if res.Kind == resources.KindStrictFile {
			w.Header().Set("X-OLP-Content-SHA256", artifact.Digest)
		}
		x.responseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		written, copyErr := copyResourceContent(w, opened.File, artifact.ContentLength)
		if copyErr != nil || written != artifact.ContentLength {
			x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The file response ended before all bytes were delivered.")
			x.facts[len(x.facts)-1].Class = classCancelled
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

func (s *Server) fileCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, keyauthority.Authority, *resources.Resource, *pin, *routing.Route) *Error) {
	x, authority, done := s.stateBegin(w, r, wire.File)
	if done {
		return
	}
	defer s.release(r.Context())
	kind := durableKind(resources.KindFile, r.PathValue("id"))
	if kind == resources.KindStrictFile && !authority.Policy.AllowProviderState {
		s.stateFail(x, w, providerStateForbidden(), x.family)
		return
	}
	res, contract, err := s.readDurable(r.Context(), kind, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No file with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."), x.family)
		return
	}
	if operation := retainedFileOperation(res); operation != "batch" {
		if !authority.Policy.AllowProviderState {
			s.stateFail(x, w, providerStateForbidden(), x.family)
			return
		}
		x.operationOverride = operation
		if res.State == "submitting" || res.State == "submission_unknown" || res.State == "not_accepted" {
			s.stateFail(x, w, serverError(http.StatusConflict, "submission_unknown", "The file upload has no reconciled native identity yet."), x.family)
			return
		}
		x.retained = retainedOperation{use: retainedHousekeeping, usage: retainedTokenlessUsage}
		p, route, e := s.resolveResource(r.Context(), x, authority, res, operation, use)
		if e != nil {
			s.stateFail(x, w, e, x.family)
			return
		}
		x.route = route
		ctx, cancel := s.stateDeadline(r.Context(), route)
		defer cancel()
		defer s.retainedSettle(ctx, x, p)
		if e := s.admitRetainedRequest(ctx, x, authority, p, route, false); e != nil {
			s.stateFail(x, w, e, x.family)
			return
		}
		if e := op(ctx, x, authority, res, p, route); e != nil {
			s.stateFail(x, w, e, x.family)
		}
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

func providerFilePath(res *resources.Resource, adapter connectors.BatchAdapter) (string, *Error) {
	if adapter != connectors.BatchGeminiManaged {
		return "files/" + url.PathEscape(res.UpstreamID), nil
	}
	path, err := managedGeminiFilePath(res)
	if err != nil {
		return "", serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	return path, nil
}

func (s *Server) verifyProviderFile(res *resources.Resource, adapter connectors.BatchAdapter, body []byte) *Error {
	if adapter == connectors.BatchGeminiManaged {
		var object struct {
			Name string `json:"name"`
		}
		name, err := geminiManagedFileName(res)
		if err != nil || json.Unmarshal(body, &object) != nil || object.Name != name {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini file name.")
		}
		return nil
	}
	if res.Kind != resources.KindStrictFile && !retainedIdentityMatches(body, res.UpstreamID, s.cfg.MaxResponseBytes) {
		return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
	}
	return nil
}

func (s *Server) detachGeminiResultFile(ctx context.Context, res *resources.Resource) (bool, *Error) {
	record, err := decodeManagedFileRecord(res.Metadata)
	if err != nil {
		return false, serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
	}
	if record.Role != "output" && record.Role != "error" {
		return false, nil
	}
	name := cmp.Or(record.ResponsesFile, res.UpstreamID)
	commitCtx, stop := resourceCommitContext(ctx)
	defer stop()
	detached, err := s.deps.Resources.DetachGeminiResultView(commitCtx, res, name)
	if errors.Is(err, resources.ErrNotFound) {
		return false, notFoundError("not_found", "No file with this identifier exists for this key.")
	}
	if err != nil {
		return false, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The result file dependencies could not be read.")
	}
	if detached {
		return true, nil
	}
	pending, err := s.deps.Batches.GeminiResultPending(ctx, res, name)
	if err != nil {
		return false, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The result file dependencies could not be read.")
	}
	if pending {
		return false, serverError(http.StatusConflict, "batch_settlement_pending", "The result file is still needed for settlement; retry after settlement closes.")
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
		fields["batch_manifest"] = mustJSON(manifest)
		return json.Marshal(fields)
	default:
		return metadata, nil
	}
}
