package gateway

import (
	"bytes"
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

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

const stateRouteHeader = "X-OLP-Route"

func (s *Server) registerState(mux *http.ServeMux) {
	if s.Resources == nil {
		return
	}
	if s.Media != nil {
		mux.HandleFunc("POST /v1/files", s.uploadFile)
		mux.HandleFunc("GET /v1/files", s.listFiles)
		mux.HandleFunc("GET /v1/files/{id}", s.getFile)
		mux.HandleFunc("DELETE /v1/files/{id}", s.deleteFile)
		mux.HandleFunc("GET /v1/files/{id}/content", s.fileContent)
		mux.HandleFunc("POST /v1/batches", s.createBatch)
		mux.HandleFunc("GET /v1/batches", s.listBatches)
		mux.HandleFunc("GET /v1/batches/{id}", s.getBatch)
		mux.HandleFunc("POST /v1/batches/{id}/cancel", s.cancelBatch)
	}
	if s.Resources.Encrypted() {
		mux.HandleFunc("GET /v1/continuations/{id}", s.recoverContinuation)
		mux.HandleFunc("GET /v1/continuation-submissions/{submission}", s.recoverContinuation)
	}
	mux.HandleFunc("GET /v1/responses/{id}", s.getResponse)
	mux.HandleFunc("DELETE /v1/responses/{id}", s.deleteResponse)
	mux.HandleFunc("POST /v1/responses/{id}/cancel", s.cancelResponse)
	mux.HandleFunc("GET /v1/responses/{id}/input_items", s.responseInputItems)
	mux.HandleFunc("GET /v1/realtime", s.realtime)
	mux.HandleFunc("POST /bedrock/model/{model}/converse", s.bedrockConverse)
	mux.HandleFunc("POST /bedrock/model/{model}/converse-stream", s.bedrockConverseStream)
	mux.HandleFunc("POST /bedrock/model/{model}/invoke", s.bedrockInvoke)
	mux.HandleFunc("POST /bedrock/model/{model}/invoke-with-response-stream", s.bedrockInvokeStream)
}

const resourceEstimate = 100

const maxResourceList = 100

type pin struct {
	target    runtime.Target
	provider  runtime.Provider
	attempt   runtime.Attempt
	slot      runtime.Slot
	model     string
	hold      *dispatchHold
	secret    []byte
	hasSecret bool
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errResponseTooLarge
	}
	return body, nil
}

func stateQualified(p *runtime.Provider, model, operation, mode string) bool {
	if !p.Connector().Supports(operation, "openai", mode) {
		return false
	}
	if !p.Supports(model, operation, "openai", mode) {
		return false
	}
	return p.Connector().SupportsRetainedResponses()
}

func officialOpenAIEndpoint(endpoint string) bool {
	endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	return endpoint == "" || endpoint == "https://api.openai.com/v1"
}

func resourceURL(cfg connectors.Config, model, path string, query url.Values) (string, error) {
	return cfg.ResourceURL(model, path, query)
}

func pinUnavailable() *Error {
	return serverError(http.StatusConflict, "provider_resource_credential_unavailable",
		"The provider revision, slot, or credential that owns this object is no longer available.")
}

func (s *Server) resolveResource(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, operation string) (*pin, *runtime.Route, *Error) {
	if s.Resolver == nil || s.Resources == nil {
		return nil, nil, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider state is not configured on this installation.")
	}
	tx, err := s.Resources.Begin(ctx)
	if err != nil {
		return nil, nil, serverError(http.StatusInternalServerError, "internal_error", "The stored target could not be rebuilt.")
	}
	defer tx.Rollback(ctx)
	provider, route, slot, secret, err := s.Resolver.Resolve(ctx, tx, res, operation)
	if errors.Is(err, resources.ErrUnavailable) || errors.Is(err, resources.ErrNoRows) {
		return nil, nil, pinUnavailable()
	}
	if err != nil {
		return nil, nil, serverError(http.StatusInternalServerError, "internal_error", "The stored target could not be rebuilt.")
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		return nil, nil, notFoundError("not_found", "No "+res.Kind+" with this identifier exists for this key.")
	}
	var target *runtime.Target
	for i := range route.Targets {
		if route.Targets[i].ProviderID != provider.ID || route.Targets[i].ProviderModel != resourceModel(res) {
			continue
		}
		if target != nil {
			return nil, nil, pinUnavailable()
		}
		target = &route.Targets[i]
	}
	if target == nil {
		return nil, nil, pinUnavailable()
	}
	attempt := runtime.Attempt{
		TargetID:           target.ID,
		ProviderID:         provider.ID,
		ProviderRevisionID: provider.RevisionID,
		ProviderKind:       provider.Kind,
		UpstreamModel:      target.ProviderModel,
		Timeout:            time.Duration(target.Timeout) * time.Millisecond,
		VendorID:           provider.VendorID,
	}
	p := &pin{target: *target, provider: *provider, attempt: attempt, slot: *slot, model: target.ProviderModel}
	if secret != nil {
		p.secret, p.hasSecret = secret, true
	}
	return p, route, nil
}

func (s *Server) pinSecret(x *execution, p *pin) []byte {
	if p.hasSecret {
		return p.secret
	}
	if p.slot.CredentialID != nil {
		secret, _ := x.request.release.Credential(*p.slot.CredentialID)
		return secret
	}
	return nil
}

func resourceModel(res *resources.Resource) string {
	var meta struct {
		UpstreamModel string `json:"upstream_model"`
	}
	_ = json.Unmarshal(res.Metadata, &meta)
	return meta.UpstreamModel
}

func (s *Server) selectPin(ctx context.Context, x *execution, route *runtime.Route, operation, mode string) (*pin, *Error) {
	return s.selectPinSurface(ctx, x, route, operation, "openai", mode, func(p *runtime.Provider, model string) bool {
		return stateQualified(p, model, operation, mode)
	})
}

func (s *Server) selectPinSurface(ctx context.Context, x *execution, route *runtime.Route, operation, surface, mode string, qualified func(*runtime.Provider, string) bool) (*pin, *Error) {
	snapshot := x.request.release.Snapshot
	plan, err := runtime.PlanRequest(snapshot, route.Slug, operation, surface, mode, x.affinity, runtime.SelectionOptions{
		KeyID: x.keyID, Preferences: x.preferences, Inputs: s.routingInputs(), Now: s.now(),
		CheckSlots: true, CredentialRevoked: s.Runtime.Revoked,
		Accept: func(p runtime.Provider, t runtime.Target) error {
			if !qualified(&p, t.ProviderModel) {
				return errors.New("provider capability unavailable")
			}
			return nil
		}})
	if err != nil {
		return nil, requestError(err)
	}
	if len(plan.Attempts) == 0 {
		return nil, selectionError(&runtime.SelectionError{Code: runtime.NoEligibleTargets}, route.Slug)
	}
	attempt := plan.Attempts[0]
	provider, ok := snapshot.Providers[attempt.ProviderID]
	if !ok {
		return nil, selectionError(&runtime.SelectionError{Code: runtime.NoEligibleTargets}, route.Slug)
	}
	x.decisions = plan.Decisions
	x.policy = plan.Policy
	x.budget = 1
	x.attempts = plan.Attempts[:1]
	deadline := s.now().Add(time.Duration(route.OverallTimeout) * time.Millisecond)
	if mode == "realtime" {
		var ok bool
		deadline, ok = ctx.Deadline()
		if !ok {
			return nil, serverError(http.StatusInternalServerError, "internal_error", "Realtime admission requires a session deadline.")
		}
	}
	for i := range provider.Slots {
		slot := &provider.Slots[i]
		if !s.slotAvailable(x, attempt, slot) || s.cooling(ctx, provider.ID, slot) {
			continue
		}
		gate := s.gateSlot(ctx, &provider, slot, resourceEstimate, deadline)
		switch gate.verdict {
		case gateAdmitted:
			return &pin{provider: provider, attempt: attempt, slot: *slot, model: attempt.UpstreamModel, hold: gate.hold}, nil
		case gateRejected:
			return nil, gate.rejection.toError()
		case gateExpired:
			return nil, (&attemptFailure{class: classTimeout}).toError()
		}
	}
	return nil, serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No credential slot is currently able to serve `"+route.Slug+"`.")
}

func (s *Server) pinnedDo(ctx context.Context, x *execution, p *pin, method, endpoint string, body []byte, contentType string) (*http.Response, *attemptFailure) {
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1)
	fact.Mode = x.mode
	finish := func(class string, f *attemptFailure) *attemptFailure {
		if f == nil {
			f = &attemptFailure{}
		}
		f.class = class
		fact.Class = class
		fact.Committed = f.committed
		if fact.Interaction != nil && x.family == openai.FamilyGeminiInteractions {
			switch {
			case f.status > 0:
				fact.Interaction.UpstreamState = usage.UpstreamTerminal
			case f.dispatched:
				fact.Interaction.UpstreamState = usage.UpstreamUnknown
			}
		}
		fact.Duration = s.now().Sub(fact.StartedAt)
		// Stored-response lifecycle calls do not generate billable work.
		fact.recordEvidence(x.family != openai.FamilyResponses && x.actor != "api_key_resource")
		x.facts = append(x.facts, fact)
		return f
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, finish(classConnect, nil)
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	if _, err := s.egress.ValidateEndpoint(parsed.String()); err != nil {
		return nil, finish(classConnect, nil)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, finish(classConnect, nil)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "olp-go/gateway")
	req.Header.Set("Accept", "application/json")
	if x.family == openai.FamilyGeminiInteractions && x.mode == "streaming" {
		req.Header.Set("Accept", "text/event-stream")
	}
	cfg := p.provider.Connector()
	if _, err := s.auth.Apply(ctx, req, cfg, s.pinSecret(x, p), body); err != nil {
		if ctx.Err() != nil {
			return nil, finish(classCancelled, nil)
		}
		return nil, finish(classCredential, nil)
	}
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		return nil, finish(classCredential, nil)
	}
	resp, err := client.Do(req)
	if err != nil {
		class := classConnect
		if ctx.Err() != nil {
			class = classCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				class = classTimeout
			}
		}
		return nil, finish(class, &attemptFailure{dispatched: true})
	}
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte = &received
	fact.Status = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fact.Class = "success"
		fact.Committed = true
		if x.family == openai.FamilyGeminiInteractions {
			// The upstream has answered, but the client cannot observe a new
			// Interaction until its encrypted ID mapping commits.
			fact.Committed = false
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = usage.UpstreamAccepted
			}
		}
		fact.Duration = s.now().Sub(fact.StartedAt)
		// Stored-response lifecycle calls do not generate billable work.
		fact.recordEvidence(x.family != openai.FamilyResponses && x.actor != "api_key_resource")
		x.facts = append(x.facts, fact)
		return resp, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	resp.Body.Close()
	f := &attemptFailure{status: resp.StatusCode, upstream: openai.ParseErrorBody(raw), dispatched: true}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		f.class = classCredential
	case resp.StatusCode == http.StatusTooManyRequests:
		f.retryAfter = retryAfter(resp.Header.Get("Retry-After"), s.now())
		f.class = classRateLimit
	case resp.StatusCode == http.StatusNotFound:
		f.class = classUpstreamClient
	case resp.StatusCode >= 500:
		f.class = classUpstreamServer
	default:
		f.class = classUpstreamClient
	}
	return nil, finish(f.class, f)
}

func upstreamError(f *attemptFailure) *Error {
	if f.upstream != nil && f.upstream.Message != "" {
		return &Error{Status: f.status, Type: f.upstream.Type, Code: f.upstream.Code, Message: f.upstream.Message}
	}
	return serverError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
}

func (s *Server) stateBegin(w http.ResponseWriter, r *http.Request, family openai.Family) (*execution, access.Authority, bool) {
	x := &execution{request: s.begin(w, r), family: family, actor: "api_key"}
	if !s.admit(r.Context()) {
		s.stateFail(x, w, overloaded, family)
		return x, access.Authority{}, true
	}
	authority, e := s.authenticate(r, "inference")
	if e != nil {
		s.release(r.Context())
		s.stateFail(x, w, e, family)
		return x, access.Authority{}, true
	}
	x.keyID, x.affinity = authority.ID, []byte(authority.ID)
	x.budgetGroupID = authority.BudgetGroupID
	if x.attribution, e = s.parseAttribution(r, authority); e != nil {
		s.release(r.Context())
		s.stateFail(x, w, e, family)
		return x, access.Authority{}, true
	}
	return x, authority, false
}

func (s *Server) stateFail(x *execution, w http.ResponseWriter, e *Error, family openai.Family) {
	x.failure = e
	s.finish(x, nil, e.Status)
	writeSurfaceError(w, e, family.Surface())
}

func (s *Server) stateDeadline(ctx context.Context, route *runtime.Route) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(route.OverallTimeout)*time.Millisecond)
}

func (s *Server) reserveState(ctx context.Context, x *execution, authority access.Authority, overall time.Duration) *Error {
	var e *Error
	x.lease, e = s.Admission.reserveKey(ctx, authority, resourceEstimate, overall)
	return e
}

func (s *Server) resourceSettle(ctx context.Context, x *execution, p *pin) {
	if p != nil && p.hold != nil {
		p.hold.settle(ctx, x.dispatched, nil)
	}
	settleKey(ctx, x.lease, x.dispatched, nil, s.log)
}

func upstreamString(body json.RawMessage, name string) (string, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return "", false
	}
	raw, ok := obj[name]
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}

func rewriteID(body []byte, name, value string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	obj[name] = encoded
	return json.Marshal(obj)
}

func fileObject(res *resources.Resource) ([]byte, error) {
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(res.Metadata, &meta); err != nil {
		return nil, err
	}
	obj := map[string]json.RawMessage{}
	for _, name := range []string{"object", "purpose", "filename", "bytes", "status", "status_details", "expires_at", "created_at"} {
		if raw, ok := meta[name]; ok {
			obj[name] = raw
		}
	}
	encoded, err := json.Marshal(res.ID)
	if err != nil {
		return nil, err
	}
	obj["id"] = encoded
	if _, ok := obj["object"]; !ok {
		obj["object"] = json.RawMessage(`"file"`)
	}
	if _, ok := obj["created_at"]; !ok {
		at, _ := json.Marshal(res.CreatedAt.Unix())
		obj["created_at"] = at
	}
	if res.State == resources.StateDeleted {
		obj["deleted"] = json.RawMessage(`true`)
	}
	return json.Marshal(obj)
}

func (s *Server) batchObject(ctx context.Context, res *resources.Resource) ([]byte, error) {
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(res.Metadata, &meta); err != nil {
		return nil, err
	}
	obj := map[string]json.RawMessage{}
	for name, raw := range meta {
		if name == "upstream_model" {
			continue
		}
		obj[name] = raw
	}
	for _, name := range []string{"output_file_id", "error_file_id"} {
		raw, ok := obj[name]
		if !ok {
			continue
		}
		var upstreamID string
		if json.Unmarshal(raw, &upstreamID) != nil || upstreamID == "" || strings.HasPrefix(upstreamID, "file_") {
			continue
		}
		mapped, err := s.mapUpstreamFile(ctx, res, upstreamID)
		if err != nil {
			return nil, err
		}
		encoded, _ := json.Marshal(mapped.ID)
		obj[name] = encoded
	}
	encoded, err := json.Marshal(res.ID)
	if err != nil {
		return nil, err
	}
	obj["id"] = encoded
	if _, ok := obj["object"]; !ok {
		obj["object"] = json.RawMessage(`"batch"`)
	}
	return json.Marshal(obj)
}

func (s *Server) mapUpstreamFile(ctx context.Context, owner *resources.Resource, upstreamID string) (*resources.Resource, error) {
	if existing, err := s.Resources.GetByUpstream(ctx, resources.KindFile, owner.APIKeyID, owner.ProviderID, upstreamID); err == nil {
		return existing, nil
	}
	metadata, _ := json.Marshal(map[string]any{"upstream_model": resourceModel(owner)})
	return s.Resources.Put(ctx, &resources.Resource{
		Kind:               resources.KindFile,
		APIKeyID:           owner.APIKeyID,
		RouteSlug:          owner.RouteSlug,
		ProviderID:         owner.ProviderID,
		ProviderRevisionID: owner.ProviderRevisionID,
		RouteRevisionID:    owner.RouteRevisionID,
		SlotID:             owner.SlotID,
		CredentialID:       owner.CredentialID,
		UpstreamID:         upstreamID,
		State:              "processed",
		Metadata:           metadata,
	})
}

func fileMetadata(body []byte, model string) (json.RawMessage, string, *time.Time) {
	meta := map[string]json.RawMessage{}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) == nil {
		for _, name := range []string{"object", "purpose", "filename", "bytes", "status", "status_details", "expires_at", "created_at"} {
			if raw, ok := obj[name]; ok {
				meta[name] = raw
			}
		}
	}
	if model != "" {
		encoded, _ := json.Marshal(model)
		meta["upstream_model"] = encoded
	}
	var expires *time.Time
	if raw, ok := meta["expires_at"]; ok {
		var seconds int64
		if json.Unmarshal(raw, &seconds) == nil && seconds > 0 {
			at := time.Unix(seconds, 0).UTC()
			expires = &at
		}
	}
	state := "uploaded"
	if raw, ok := meta["status"]; ok {
		var status string
		if json.Unmarshal(raw, &status) == nil && status != "" {
			state = status
		}
	}
	encoded, _ := json.Marshal(meta)
	return encoded, state, expires
}

func batchMetadata(body []byte, model string) (json.RawMessage, string) {
	meta := map[string]json.RawMessage{}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) == nil {
		for name, raw := range obj {
			if name == "id" {
				continue
			}
			meta[name] = raw
		}
	}
	if model != "" {
		encoded, _ := json.Marshal(model)
		meta["upstream_model"] = encoded
	}
	state := "validating"
	if raw, ok := meta["status"]; ok {
		var status string
		if json.Unmarshal(raw, &status) == nil && status != "" {
			state = status
		}
	}
	encoded, _ := json.Marshal(meta)
	return encoded, state
}

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, openai.FamilyFile)
	if done {
		return
	}
	defer s.release(r.Context())
	slug := strings.TrimSpace(r.Header.Get(stateRouteHeader))
	if slug == "" {
		s.stateFail(x, w, invalidRequest("missing_required_parameter", "X-OLP-Route must name a batch-enabled route.", nil), x.family)
		return
	}
	snapshot := x.request.release.Snapshot
	route, ok := snapshot.Routes[slug]
	if !ok {
		s.stateFail(x, w, modelNotFound(slug), x.family)
		return
	}
	x.route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		s.stateFail(x, w, permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."), x.family)
		return
	}
	if !slices.Contains(route.Operations, "batch") {
		s.stateFail(x, w, invalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow batch operations.", nil), x.family)
		return
	}
	if e := policySurfaceGate(&route); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	form, e := s.parseMediaForm(w, r, authority.ID, s.cfg.MaxMediaBodyBytes, 1)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	defer form.Cleanup()
	purpose, mErr := form.Required("purpose")
	if mErr != nil {
		s.stateFail(x, w, mediaError(mErr), x.family)
		return
	}
	file, mErr := form.TakeSingleFile("file")
	if mErr != nil {
		s.stateFail(x, w, mediaError(mErr), x.family)
		return
	}
	if file == nil {
		s.stateFail(x, w, invalidRequest("missing_required_parameter", "The file field must carry one file.", nil), x.family)
		return
	}
	extra, mErr := form.TakeExtensions()
	if mErr != nil {
		s.stateFail(x, w, mediaError(mErr), x.family)
		return
	}
	form.Disarm()
	defer func() {
		if err := s.transport().Spool.Remove(file.Handle); err != nil {
			s.log.Warn("file upload cleanup failed", "error", err)
		}
	}()
	p, e := s.selectPin(r.Context(), x, &route, "batch", "unary")
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), &route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	endpoint, err := resourceURL(p.provider.Connector(), p.model, "files", nil)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."), x.family)
		return
	}
	resp, failure := s.uploadMultipart(ctx, x, p, endpoint, purpose, extra, file)
	if failure != nil {
		x.dispatched = true
		s.stateFail(x, w, upstreamError(failure), x.family)
		return
	}
	defer resp.Body.Close()
	x.dispatched = true
	body, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."), x.family)
		return
	}
	upstreamID, ok := upstreamString(body, "id")
	if !ok || len(upstreamID) > 512 {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."), x.family)
		return
	}
	metadata, state, expires := fileMetadata(body, p.model)
	res, err := s.Resources.Put(r.Context(), &resources.Resource{
		Kind:               resources.KindFile,
		APIKeyID:           authority.ID,
		RouteSlug:          route.Slug,
		ProviderID:         p.provider.ID,
		ProviderRevisionID: p.provider.RevisionID,
		RouteRevisionID:    route.RevisionID,
		SlotID:             p.slot.ID,
		CredentialID:       p.slot.CredentialID,
		UpstreamID:         upstreamID,
		State:              state,
		Metadata:           metadata,
		ExpiresAt:          expires,
	})
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be stored."), x.family)
		return
	}
	out, err := rewriteID(body, "id", res.ID)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."), x.family)
		return
	}
	s.writeStateJSON(w, x, out)
}

func (s *Server) uploadMultipart(ctx context.Context, x *execution, p *pin, endpoint, purpose string, extra map[string]string, file *media.Part) (*http.Response, *attemptFailure) {
	pipeR, pipeW := io.Pipe()
	form := multipart.NewWriter(pipeW)
	sendDone := make(chan error, 1)
	go func() {
		failure := func(err error) error {
			pipeW.CloseWithError(err)
			return err
		}
		if err := form.WriteField("purpose", purpose); err != nil {
			sendDone <- failure(err)
			return
		}
		for name, value := range extra {
			if err := form.WriteField(name, value); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		opened, err := s.transport().Spool.Open(file.Handle)
		if err != nil {
			sendDone <- failure(err)
			return
		}
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
			opened.File.Close()
			sendDone <- failure(err)
			return
		}
		_, copyErr := io.Copy(part, opened.File)
		opened.File.Close()
		if copyErr != nil {
			sendDone <- failure(copyErr)
			return
		}
		if err := form.Close(); err != nil {
			sendDone <- failure(err)
			return
		}
		sendDone <- pipeW.Close()
	}()
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1)
	finish := func(class string, f *attemptFailure) *attemptFailure {
		if f == nil {
			f = &attemptFailure{}
		}
		f.class = class
		fact.Class = class
		fact.Committed = f.committed
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(true)
		x.facts = append(x.facts, fact)
		return f
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pipeR)
	if err != nil {
		pipeR.CloseWithError(err)
		return nil, finish(classConnect, nil)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("User-Agent", "olp-go/gateway")
	req.Header.Set("Accept", "application/json")
	if _, err := s.auth.Apply(ctx, req, p.provider.Connector(), s.pinSecret(x, p), nil); err != nil {
		pipeR.CloseWithError(err)
		return nil, finish(classCredential, nil)
	}
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		pipeR.CloseWithError(err)
		return nil, finish(classCredential, nil)
	}
	resp, err := client.Do(req)
	if err != nil {
		pipeR.CloseWithError(err)
		class := classConnect
		if ctx.Err() != nil {
			class = classCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				class = classTimeout
			}
		}
		return nil, finish(class, &attemptFailure{dispatched: true})
	}
	if sendErr := <-sendDone; sendErr != nil {
		resp.Body.Close()
		return nil, finish(classConnect, &attemptFailure{dispatched: true})
	}
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte = &received
	fact.Status = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fact.Class = "success"
		fact.Committed = true
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(true)
		x.facts = append(x.facts, fact)
		return resp, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	resp.Body.Close()
	f := &attemptFailure{status: resp.StatusCode, upstream: openai.ParseErrorBody(raw), dispatched: true}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		f.class = classCredential
	case resp.StatusCode == http.StatusTooManyRequests:
		f.retryAfter = retryAfter(resp.Header.Get("Retry-After"), s.now())
		f.class = classRateLimit
	case resp.StatusCode == http.StatusNotFound:
		f.class = classUpstreamClient
	case resp.StatusCode >= 500:
		f.class = classUpstreamServer
	default:
		f.class = classUpstreamClient
	}
	return nil, finish(f.class, f)
}

var stateQuoteReplacer = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "", "\x00", "").Replace

func stateQuote(value string) string { return stateQuoteReplacer(value) }

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, openai.FamilyFile)
	if done {
		return
	}
	defer s.release(r.Context())
	limit, after, e := listArgs(r)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	rows, err := s.Resources.List(r.Context(), resources.KindFile, authority.ID, limit, after)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file list could not be read."), x.family)
		return
	}
	items := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		obj, err := fileObject(row)
		if err != nil {
			s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A file in this list could not be projected."), x.family)
			return
		}
		items = append(items, obj)
	}
	s.writeStateJSON(w, x, listBody(items))
}

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, func(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, p *pin, route *runtime.Route) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "files/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.Resources.Tombstone(ctx, res.ID)
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
		metadata, state, expires := fileMetadata(body, resourceModel(res))
		if err := s.Resources.Update(ctx, res.ID, state, metadata, expires); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed file state could not be committed.")
		}
		res.State, res.Metadata, res.ExpiresAt = state, metadata, expires
		out, err := rewriteID(body, "id", res.ID)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
		}
		s.writeStateJSON(w, x, out)
		return nil
	})
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, func(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, p *pin, route *runtime.Route) *Error {
		endpoint, err := resourceURL(p.provider.Connector(), p.model, "files/"+url.PathEscape(res.UpstreamID), nil)
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.pinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.status == http.StatusNotFound {
				_ = s.Resources.Tombstone(ctx, res.ID)
				return notFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		x.dispatched = true
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
		if err := s.Resources.Tombstone(ctx, res.ID); err != nil {
			return serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be deleted.")
		}
		encoded, _ := json.Marshal(res.ID)
		out, _ := json.Marshal(map[string]json.RawMessage{
			"id": encoded, "object": json.RawMessage(`"file"`), "deleted": json.RawMessage(`true`),
		})
		s.writeStateJSON(w, x, out)
		return nil
	})
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, func(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, p *pin, route *runtime.Route) *Error {
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
		spool := s.transport().Spool
		if spool == nil {
			return serverError(http.StatusServiceUnavailable, "media_content_unavailable", "The bounded file spool is unavailable.")
		}
		artifact, err := spool.Put(ctx, media.Upload{
			Filename: "file-content", ContentType: contentType,
			MaximumLength: s.cfg.MaxResponseBytes, Body: resp.Body,
		})
		if err != nil {
			if len(x.facts) > 0 {
				fact := &x.facts[len(x.facts)-1]
				fact.Class, fact.UsageComplete, fact.BillingUncertain = classProtocol, false, true
			}
			var tooLarge *media.TooLargeError
			if errors.As(err, &tooLarge) {
				return serverError(http.StatusBadGateway, "upstream_response_too_large", "The provider file exceeded the configured response bound.")
			}
			return serverError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}
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
		w.WriteHeader(http.StatusOK)
		x.delivered(s.now())
		written, copyErr := io.Copy(w, opened.File)
		if copyErr != nil || written != artifact.ContentLength {
			x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The file response ended before all bytes were delivered.")
			s.finish(x, &outcome{err: x.failure, committed: true}, x.failure.Status)
			return nil
		}
		s.finish(x, &outcome{committed: true}, http.StatusOK)
		return nil
	})
}

func (s *Server) fileCall(w http.ResponseWriter, r *http.Request, op func(context.Context, *execution, access.Authority, *resources.Resource, *pin, *runtime.Route) *Error) {
	x, authority, done := s.stateBegin(w, r, openai.FamilyFile)
	if done {
		return
	}
	defer s.release(r.Context())
	res, err := s.Resources.Get(r.Context(), resources.KindFile, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No file with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."), x.family)
		return
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, res, "batch")
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond))
	if gate.verdict != gateAdmitted {
		s.stateFail(x, w, gateError(gate), x.family)
		return
	}
	p.hold = gate.hold
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.stateFail(x, w, e, x.family)
	}
}

func gateError(gate gateResult) *Error {
	switch gate.verdict {
	case gateRejected:
		return gate.rejection.toError()
	case gateExpired:
		return (&attemptFailure{class: classTimeout}).toError()
	}
	return serverError(http.StatusServiceUnavailable, "upstream_unavailable", "The pinned credential cannot currently serve this object.")
}

func (s *Server) writeStateJSON(w http.ResponseWriter, x *execution, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	x.delivered(s.now())
	s.finish(x, &outcome{committed: true}, http.StatusOK)
}

func listArgs(r *http.Request) (int, string, *Error) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var parsed int
		if _, err := fmt.Sscanf(raw, "%d", &parsed); err != nil || parsed < 1 || parsed > maxResourceList {
			param := "limit"
			return 0, "", invalidRequest("invalid_value", "limit must be an integer between 1 and 100.", &param)
		}
		limit = parsed
	}
	after := r.URL.Query().Get("after")
	if after != "" && !strings.Contains(after, "_") {
		param := "after"
		return 0, "", invalidRequest("invalid_value", "after must be a gateway object identifier.", &param)
	}
	return limit, after, nil
}

func listBody(items []json.RawMessage) []byte {
	data, _ := json.Marshal(items)
	out, _ := json.Marshal(map[string]json.RawMessage{
		"object":   json.RawMessage(`"list"`),
		"data":     data,
		"has_more": json.RawMessage(`false`),
	})
	return out
}

func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, openai.FamilyBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	body, e := s.readBody(r)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		s.stateFail(x, w, invalidRequest("invalid_json", "The request body must be one JSON object.", nil), x.family)
		return
	}
	localFile, ok := upstreamString(body, "input_file_id")
	if !ok || !strings.HasPrefix(localFile, "file_") {
		param := "input_file_id"
		s.stateFail(x, w, invalidRequest("missing_required_parameter", "input_file_id must name a file uploaded through this key.", &param), x.family)
		return
	}
	file, err := s.Resources.Get(r.Context(), resources.KindFile, authority.ID, localFile)
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No file with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read."), x.family)
		return
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, file, "batch")
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	x.route = route
	if e := policySurfaceGate(route); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond))
	if gate.verdict != gateAdmitted {
		s.stateFail(x, w, gateError(gate), x.family)
		return
	}
	p.hold = gate.hold
	request["input_file_id"], _ = json.Marshal(file.UpstreamID)
	if p.provider.Kind == "azure_openai" {
		deployment := p.provider.Connector().Model(p.model)
		if deployment != "" {
			request["model"], _ = json.Marshal(deployment)
		}
	}
	delete(request, "id")
	upstream, _ := json.Marshal(request)
	endpoint, err := resourceURL(p.provider.Connector(), p.model, "batches", nil)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."), x.family)
		return
	}
	resp, failure := s.pinnedDo(ctx, x, p, http.MethodPost, endpoint, upstream, "application/json")
	if failure != nil {
		x.dispatched = true
		s.stateFail(x, w, upstreamError(failure), x.family)
		return
	}
	defer resp.Body.Close()
	x.dispatched = true
	result, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."), x.family)
		return
	}
	upstreamID, ok := upstreamString(result, "id")
	if !ok || len(upstreamID) > 512 {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object."), x.family)
		return
	}
	metadata, state := batchMetadata(result, p.model)
	res, err := s.Resources.Put(r.Context(), &resources.Resource{
		Kind:               resources.KindBatch,
		APIKeyID:           authority.ID,
		RouteSlug:          route.Slug,
		ProviderID:         p.provider.ID,
		ProviderRevisionID: p.provider.RevisionID,
		RouteRevisionID:    route.RevisionID,
		SlotID:             p.slot.ID,
		CredentialID:       p.slot.CredentialID,
		UpstreamID:         upstreamID,
		State:              state,
		Metadata:           metadata,
	})
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The batch mapping could not be stored."), x.family)
		return
	}
	out, err := s.batchObject(r.Context(), res)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object."), x.family)
		return
	}
	s.writeStateJSON(w, x, out)
}

func (s *Server) batchCall(w http.ResponseWriter, r *http.Request, op func(context.Context, *execution, access.Authority, *resources.Resource, *pin, *runtime.Route) *Error) {
	x, authority, done := s.stateBegin(w, r, openai.FamilyBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	res, err := s.Resources.Get(r.Context(), resources.KindBatch, authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.stateFail(x, w, notFoundError("not_found", "No batch with this identifier exists for this key."), x.family)
		return
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The batch mapping could not be read."), x.family)
		return
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, res, "batch")
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	x.route = route
	ctx, cancel := s.stateDeadline(r.Context(), route)
	defer cancel()
	defer s.resourceSettle(ctx, x, p)
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond))
	if gate.verdict != gateAdmitted {
		s.stateFail(x, w, gateError(gate), x.family)
		return
	}
	p.hold = gate.hold
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.stateFail(x, w, e, x.family)
	}
}

func (s *Server) listBatches(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.stateBegin(w, r, openai.FamilyBatch)
	if done {
		return
	}
	defer s.release(r.Context())
	limit, after, e := listArgs(r)
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	rows, err := s.Resources.List(r.Context(), resources.KindBatch, authority.ID, limit, after)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The batch list could not be read."), x.family)
		return
	}
	items := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		obj, err := s.batchObject(r.Context(), row)
		if err != nil {
			s.stateFail(x, w, serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "A batch in this list could not be projected."), x.family)
			return
		}
		items = append(items, obj)
	}
	s.writeStateJSON(w, x, listBody(items))
}

func (s *Server) getBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, func(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, p *pin, route *runtime.Route) *Error {
		return s.batchRefresh(ctx, x, res, p, http.MethodGet, "batches/"+url.PathEscape(res.UpstreamID), nil, w)
	})
}

func (s *Server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, func(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, p *pin, route *runtime.Route) *Error {
		return s.batchRefresh(ctx, x, res, p, http.MethodPost, "batches/"+url.PathEscape(res.UpstreamID)+"/cancel", []byte(`{}`), w)
	})
}

func (s *Server) batchRefresh(ctx context.Context, x *execution, res *resources.Resource, p *pin, method, path string, body []byte, w http.ResponseWriter) *Error {
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
	metadata, state := batchMetadata(result, resourceModel(res))
	if err := s.Resources.Update(ctx, res.ID, state, metadata, nil); err != nil {
		return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed batch state could not be committed.")
	}
	res.State, res.Metadata = state, metadata
	out, err := s.batchObject(ctx, res)
	if err != nil {
		return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
	}
	s.writeStateJSON(w, x, out)
	return nil
}
