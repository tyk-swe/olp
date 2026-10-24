package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

func retainedFileOperation(r *resources.Resource) string {
	var meta struct {
		Operation string `json:"file_operation"`
	}
	if r != nil && json.Unmarshal(r.Metadata, &meta) == nil && slices.Contains([]string{"fine_tuning", "vector_store", "eval", "generation"}, meta.Operation) {
		return meta.Operation
	}
	return "batch"
}

func retainedFilePurposeOperation(purpose string) string {
	switch purpose {
	case "fine-tune":
		return "fine_tuning"
	case "assistants":
		return "vector_store"
	case "evals":
		return "eval"
	case "user_data":
		return "generation"
	}
	return ""
}

// Native retained-family uploads keep file bytes in the existing bounded spool
// and persist their at-most-once submission identity before the provider sees it.
func (s *Server) uploadRetainedFile(w http.ResponseWriter, r *http.Request, x *call, a access.Authority, route *routing.Route, file *media.Part, purpose string, extra map[string]string) {
	status := http.StatusInternalServerError
	var p *pin
	defer func() { s.finish(x, nil, status); s.retainedSettle(r.Context(), x, p) }()
	fail := func(err *Error) {
		copy := *err
		copy.NoRetry = true
		x.failure = &copy
		status = err.Status
		writeSurfaceError(w, &copy, "openai")
	}
	operation := retainedFilePurposeOperation(purpose)
	if !a.Policy.AllowProviderState {
		fail(providerStateForbidden())
		return
	}
	if operation == "" || !slices.Contains(route.Operations, operation) {
		fail(invalidRequest("unsupported_parameter", "This file purpose requires a route which permits the corresponding native operation.", nil))
		return
	}
	if len(extra) > 0 || file.Size > s.cfg.MaxBodyBytes {
		fail(invalidRequest("unsupported_parameter", "Native retained uploads require one bounded file and a supported purpose.", nil))
		return
	}
	idempotency := r.Header.Get("Idempotency-Key")
	if idempotency == "" || len(idempotency) > 128 {
		fail(invalidRequest("missing_required_parameter", "Idempotency-Key is required for native file uploads.", nil))
		return
	}
	x.operationOverride, x.mode, x.actor = operation, "unary", "api_key_resource"
	x.retainedTokenless = true
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	body, contentType, encodeErr := s.retainedUploadBody(file, purpose)
	if encodeErr != nil {
		fail(invalidRequest("invalid_request", "The bounded native upload could not be read.", nil))
		return
	}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: operation, Surface: "openai", Kind: resources.KindFile, Identity: "id", Collection: "data", Path: "/v1/files"}, Action: "create", Method: http.MethodPost, Pattern: "/v1/files"}
	digest, digestErr := s.retainedBodyDigest(e, route.Slug, body, contentType)
	if digestErr != nil {
		fail(digestErr)
		return
	}
	prior, lookupErr := s.Resources.LookupLifecycle(ctx, x.keyID, operation+":file", idempotency, digest)
	if errors.Is(lookupErr, resources.ErrSubmissionConflict) {
		fail(serverError(409, "idempotency_conflict", "This submission identity is already bound to another upload."))
		return
	}
	if lookupErr == nil {
		w.Header().Set("X-OLP-Resource-ID", prior.ID)
		fail(serverError(409, "submission_exists", "This upload already exists; inspect X-OLP-Resource-ID instead of recreating it."))
		return
	}
	if !errors.Is(lookupErr, resources.ErrNotFound) {
		fail(serverError(503, "provider_state_unavailable", "The file submission could not be read."))
		return
	}
	var err *Error
	p, err = s.selectPinSurface(ctx, x, route, operation, "openai", "unary", func(provider *routing.Provider, model string) bool {
		return (provider.Kind == "openai" || provider.Kind == "azure_openai") && provider.CredentialSource != "caller" && provider.Supports(model, operation, "openai", "unary") && provider.Connector().Supports(operation, "openai", "unary")
	})
	if err != nil {
		fail(err)
		return
	}
	if err := s.admitRetainedRequest(ctx, x, a, p, route, false); err != nil {
		fail(err)
		return
	}
	version := retainedapi.Revision
	meta, _ := json.Marshal(map[string]any{"upstream_model": p.model, "file_operation": operation, "purpose": purpose, "bytes": file.Size, "filename": file.Filename, "upload_digest": file.Digest})
	candidate := &resources.Resource{Kind: resources.KindFile, APIKeyID: x.keyID, RouteSlug: route.Slug, RouteRevisionID: route.RevisionID, ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID, TargetID: p.target.ID, SlotID: p.slot.ID, CredentialID: p.slot.CredentialID, ContractVersion: &version, Metadata: meta}
	resource, fresh, prepareErr := s.Resources.PrepareLifecycle(ctx, candidate, operation+":file", idempotency, digest)
	if errors.Is(prepareErr, resources.ErrSubmissionConflict) {
		fail(serverError(409, "idempotency_conflict", "This submission identity is already bound to another upload."))
		return
	}
	if prepareErr != nil {
		fail(serverError(503, "provider_state_unavailable", "The file submission could not be committed."))
		return
	}
	w.Header().Set("X-OLP-Resource-ID", resource.ID)
	if !fresh {
		fail(serverError(409, "submission_exists", "This upload already exists; inspect X-OLP-Resource-ID instead of recreating it."))
		return
	}
	endpoint, endpointErr := p.provider.Connector().ResourceURL(p.model, "files", nil)
	if endpointErr != nil {
		fail(pinUnavailable())
		return
	}
	if err := s.Resources.DispatchLifecycle(ctx, resource.UUID); err != nil {
		fail(serverError(503, "provider_state_unavailable", "The file submission could not be fenced."))
		return
	}
	defer func() {
		commitCtx, cancel := resourceCommitContext(r.Context())
		defer cancel()
		_ = s.Resources.UnknownLifecycle(commitCtx, resource.UUID)
	}()
	status = s.retainedDispatch(w, r.WithContext(ctx), x, p, e, resource, endpoint, body, contentType, true, fail)
}

func (s *Server) retainedUploadBody(file *media.Part, purpose string) ([]byte, string, error) {
	opened, err := s.transport().Spool.Open(file.Handle)
	if err != nil {
		return nil, "", err
	}
	defer opened.File.Close()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("purpose", purpose); err != nil {
		return nil, "", err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": file.Filename}))
	contentType := file.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	n, err := io.Copy(part, io.LimitReader(opened.File, s.cfg.MaxBodyBytes+1))
	if err != nil || n != file.Size || n > s.cfg.MaxBodyBytes {
		return nil, "", resources.ErrContract
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}
