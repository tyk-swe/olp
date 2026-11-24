package geministate

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

type declaredUpload struct {
	length      int64
	contentType string
	displayName string
	named       bool
}

func (d declaredUpload) startBody() []byte {
	if !d.named {
		return []byte{}
	}
	return kernel.MustJSON(map[string]any{"file": map[string]string{"display_name": d.displayName}})
}

func (d declaredUpload) metadata(model string) map[string]any {
	metadata := map[string]any{"upstream_model": model, "expected_length": d.length, "offset": int64(0)}
	if d.named {
		metadata["display_name"] = d.displayName
	}
	if d.contentType != "" {
		metadata["content_type"] = d.contentType
	}
	return metadata
}

func validateStart(protocol, command, rawLength string, maximum int64) (int64, *fault.Error) {
	if protocol != "resumable" || command != "start" {
		return 0, fault.InvalidRequest("invalid_request", "Gemini files require a resumable start upload.", nil)
	}
	length, err := strconv.ParseInt(rawLength, 10, 64)
	if err != nil || length <= 0 || length > maximum {
		param := "X-Goog-Upload-Header-Content-Length"
		return 0, fault.InvalidRequest("invalid_value", "The upload length must be between 1 byte and the configured media limit.", &param)
	}
	return length, nil
}

func parseStartBody(body []byte) (string, bool, *fault.Error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return "", false, nil
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil || doc.Root().Kind() != oif.Object {
		return "", false, fault.InvalidRequest("invalid_request", "The start body must be empty or contain only file.displayName.", nil)
	}
	if len(doc.Root().Members()) == 0 {
		return "", false, nil
	}
	if len(doc.Root().Members()) != 1 || doc.Root().Members()[0].Name != "file" {
		return "", false, fault.InvalidRequest("invalid_request", "The start body must be empty or contain only file.displayName.", nil)
	}
	file, present := doc.Root().Lookup("file")
	if !present || file.Kind() != oif.Object || len(file.Members()) > 1 {
		return "", false, fault.InvalidRequest("invalid_request", "The start body must contain one file display name.", nil)
	}
	if len(file.Members()) == 0 {
		return "", false, nil
	}
	member := file.Members()[0]
	if member.Name != "displayName" && member.Name != "display_name" {
		return "", false, fault.InvalidRequest("invalid_request", "The start body must contain only file.displayName.", nil)
	}
	name, ok := member.Value.Text()
	if !ok || len(name) > 512 {
		return "", false, fault.InvalidRequest("invalid_value", "file.displayName must be a string of at most 512 bytes.", nil)
	}
	return name, true, nil
}

func (s surface) startUpload(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiFile)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	route, err := s.kernel.NativeStateRoute(x, authority, r, "")
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	declared, err := s.declared(w, r)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	p, err := s.kernel.NativeStatePin(r.Context(), x, route)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.gates(ctx, x, authority, p, route, true); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	resp, sessionURL, err := s.openSession(ctx, x, p, declared)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	resource, err := s.storeSession(ctx, authority, route, p, declared, sessionURL)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	x.ResponseHeaders(w.Header(), false)
	w.Header().Set("X-Goog-Upload-Status", "active")
	w.Header().Set("X-Goog-Upload-URL", s.kernel.GatewayOrigin(r)+"/gemini/upload/"+url.PathEscape(resource.ID))
	w.WriteHeader(http.StatusOK)
	s.finishAck(w, x, resp.Exchange)
}

func (s surface) declared(w http.ResponseWriter, r *http.Request) (declaredUpload, *fault.Error) {
	length, err := validateStart(r.Header.Get("X-Goog-Upload-Protocol"), r.Header.Get("X-Goog-Upload-Command"),
		r.Header.Get("X-Goog-Upload-Header-Content-Length"), s.kernel.Config.MaxMediaBodyBytes)
	if err != nil {
		return declaredUpload{}, err
	}
	body, err := s.kernel.ReadResourceBody(w, r)
	if err != nil {
		return declaredUpload{}, err
	}
	displayName, named, err := parseStartBody(body)
	if err != nil {
		return declaredUpload{}, err
	}
	if !s.kernel.Deps.Resources.Encrypted() {
		return declaredUpload{}, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Gemini upload sessions require encrypted resource storage.")
	}
	return declaredUpload{length: length, contentType: r.Header.Get("X-Goog-Upload-Header-Content-Type"), displayName: displayName, named: named}, nil
}

func (s surface) openSession(ctx context.Context, x *kernel.Call, p *kernel.Pin, declared declaredUpload) (*kernel.PinnedResponse, string, *fault.Error) {
	endpoint, originErr := kernel.GeminiUploadOrigin(p.Provider.Connector(), p.Model, s.kernel.Egress)
	if originErr != nil {
		return nil, "", fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini upload address could not be resolved.")
	}
	headers := http.Header{
		"X-Goog-Upload-Protocol":              {"resumable"},
		"X-Goog-Upload-Command":               {"start"},
		"X-Goog-Upload-Header-Content-Length": {strconv.FormatInt(declared.length, 10)},
	}
	if declared.contentType != "" {
		headers.Set("X-Goog-Upload-Header-Content-Type", declared.contentType)
	}
	payload := declared.startBody()
	resp, failure := s.kernel.GeminiPinnedRequest(ctx, x, p, http.MethodPost, endpoint, bytes.NewReader(payload), int64(len(payload)), "application/json", "application/json", headers)
	if failure != nil {
		return nil, "", kernel.UpstreamError(failure)
	}
	sessionURLs := resp.Header.Values("X-Goog-Upload-URL")
	if len(sessionURLs) != 1 {
		return resp, "", fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider did not return one resumable upload URL.")
	}
	generationURL, _ := p.Provider.Connector().URL(wire.Gemini, p.Model, false)
	if err := kernel.ValidateGeminiUploadURL(sessionURLs[0], generationURL, s.kernel.Egress); err != nil {
		return resp, "", fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned an invalid resumable upload URL.")
	}
	resp.Exchange.Complete(kernel.ClassSuccess)
	return resp, sessionURLs[0], nil
}

func (s surface) storeSession(ctx context.Context, authority keyauthority.Authority, route *routing.Route, p *kernel.Pin, declared declaredUpload, sessionURL string) (*resources.Resource, *fault.Error) {
	expires := s.kernel.Now().Add(min(time.Hour, resources.DurableLifetime))
	id := uuid.Must(uuid.NewV7())
	commitCtx, stop := kernel.ResourceCommitContext(ctx)
	defer stop()
	resource, err := s.kernel.Deps.Resources.PutGeminiUploadSession(commitCtx, &resources.Resource{
		UUID: id, Kind: resources.KindGeminiUpload, APIKeyID: authority.ID, RouteSlug: route.Slug,
		ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID, RouteRevisionID: route.RevisionID,
		TargetID: p.Target.ID, SlotID: p.Slot.ID, CredentialID: p.Slot.CredentialID,
		UpstreamID: id.String(), State: "active", Metadata: kernel.MustJSON(declared.metadata(p.Model)), ExpiresAt: &expires,
	}, sessionURL)
	if err != nil {
		return nil, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The upload session could not be stored.")
	}
	return resource, nil
}
