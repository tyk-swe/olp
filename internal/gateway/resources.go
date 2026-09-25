package gateway

import (
	"bytes"
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

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
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

// Once a provider has accepted durable work, its owner mapping must outlive
// a client disconnect. Persistence still has a short independent deadline.
const resourceCommitTimeout = 5 * time.Second

func resourceCommitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), resourceCommitTimeout)
}

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
	provider, route, slot, secret, err := s.Resolver.ResolveCurrent(ctx, res, operation)
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

// resolveRequestFileAssets resolves every gateway-managed file reference in
// the lifted source request through the existing resource authority before
// binding. A caller string only nominates a candidate resource; owner,
// committed purpose, route and serving scope are established here by the
// durable contract, and any provider-native or unmanaged reference stays for
// the strict planner's generic refusal.
func (s *Server) resolveRequestFileAssets(ctx context.Context, x *execution) *Error {
	if x.gen == nil {
		return nil
	}
	refs := interaction.FileReferences(x.source.Request.Document(), *x.gen)
	var managed []string
	seen := map[string]bool{}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, resources.KindStrictFile+"_") || seen[ref] {
			continue
		}
		seen[ref] = true
		managed = append(managed, ref)
	}
	if len(managed) == 0 {
		return nil
	}
	if !x.strict() {
		param := "file_id"
		return invalidRequest("invalid_file_id", "Gateway-managed file references require a strict route.", &param)
	}
	bindings := make(map[string]interaction.AssetBinding, len(managed))
	for _, ref := range managed {
		binding, e := s.resolveFileAsset(ctx, x, ref)
		if e != nil {
			return e
		}
		bindings[ref] = *binding
	}
	x.resolvedAssets = bindings
	return nil
}

func (s *Server) resolveFileAsset(ctx context.Context, x *execution, ref string) (*interaction.AssetBinding, *Error) {
	res, contract, err := s.readDurable(ctx, resources.KindStrictFile, x.authority.ID, ref)
	if errors.Is(err, resources.ErrNotFound) {
		param := "file_id"
		return nil, invalidRequest("invalid_file_id", "file_id must name an inference file uploaded through this key.", &param)
	}
	if errors.Is(err, resources.ErrContract) {
		return nil, durableError(err)
	}
	if err != nil {
		return nil, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be read.")
	}
	if res.RouteSlug != x.route.Slug {
		param := "file_id"
		return nil, invalidRequest("invalid_file_id", "file_id must reference a file uploaded under this model.", &param)
	}
	if contract.Asset == nil || contract.Asset.Purpose == "" || contract.Asset.Purpose == "batch" {
		param := "file_id"
		return nil, invalidRequest("invalid_file_id", "The named file was not uploaded for inference use.", &param)
	}
	// The durable authority re-establishes the owning route revision, slot and
	// credential under the generation operation before any plan may consume
	// the native identity.
	if e := s.authorizeDurable(ctx, x, x.authority, res, contract, "generation"); e != nil {
		return nil, e
	}
	return &interaction.AssetBinding{
		LocalID: res.ID, Kind: res.Kind, NativeID: res.UpstreamID, Purpose: contract.Asset.Purpose,
		Serving: contract.Serving, Digest: contract.Asset.SHA256, MediaType: contract.Asset.ContentType, Size: contract.Asset.Size,
	}, nil
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
	var target runtime.Target
	for _, candidate := range route.Targets {
		if candidate.ID == attempt.TargetID {
			target = candidate
			break
		}
	}
	if target.ID == "" {
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
			return &pin{target: target, provider: provider, attempt: attempt, slot: *slot, model: attempt.UpstreamModel, hold: gate.hold}, nil
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
		f.attribute(&fact)
		fact.Committed = f.committed
		if fact.Interaction != nil && (x.family == openai.FamilyGeminiInteractions || x.family == openai.FamilyBatch || x.family == openai.FamilyFile) {
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
		return nil, finish(classConnect, &attemptFailure{origin: faultContract, scope: scopeContract})
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	if _, err := s.egress.ValidateEndpoint(parsed.String()); err != nil {
		return nil, finish(classConnect, &attemptFailure{origin: faultContract, scope: scopeContract})
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, finish(classConnect, &attemptFailure{origin: faultContract, scope: scopeRequest})
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "olp-go/gateway")
	req.Header.Set("Accept", "application/json")
	if x.mode == "streaming" && (x.family == openai.FamilyGeminiInteractions || x.family == openai.FamilyResponses) {
		req.Header.Set("Accept", "text/event-stream")
	}
	cfg := p.provider.Connector()
	credentialValues, err := s.auth.Apply(ctx, req, cfg, s.pinSecret(x, p), body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, finish(classCancelled, nil)
		}
		// Applying the credential is local machinery; the provider never saw
		// the request, so this is not provider-declared credential evidence.
		return nil, finish(classCredential, &attemptFailure{origin: faultContract, scope: scopeCredential})
	}
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		return nil, finish(classCredential, &attemptFailure{origin: faultContract, scope: scopeCredential})
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
		if fact.Interaction != nil && (x.family == openai.FamilyBatch || x.family == openai.FamilyFile) {
			fact.Interaction.UpstreamState = usage.UpstreamAccepted
		}
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
	if f.upstream != nil {
		f.upstream.Message = redactCredentials(f.upstream.Message, credentialValues)
	}
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
		code := f.upstream.Code
		if code == "" {
			code = "upstream_failed"
		}
		return &Error{Status: f.status, Type: f.upstream.Type, Code: code, Message: f.upstream.Message}
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

func (s *Server) fileObject(ctx context.Context, res *resources.Resource) ([]byte, error) {
	if res.Kind == resources.KindStrictFile {
		_, contract, err := s.readDurable(ctx, res.Kind, res.APIKeyID, res.ID)
		if err != nil {
			return nil, err
		}
		if len(contract.Result) != 0 {
			return durableProjection(contract.Result, res.ID, nil)
		}
		return json.Marshal(map[string]any{"id": res.ID, "object": "file", "status": res.State})
	}
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
	if res.Kind == resources.KindStrictBatch {
		_, contract, err := s.readDurable(ctx, res.Kind, res.APIKeyID, res.ID)
		if err != nil || len(contract.Result) == 0 {
			return nil, resources.ErrContract
		}
		doc, _, err := strictBatchResult(contract.Result, res.UpstreamID)
		if err != nil {
			return nil, err
		}
		if !strictBatchInputMatches(doc, contract.Effective) {
			return nil, resources.ErrContract
		}
		files := map[string]string{}
		for _, name := range []string{"input_file_id", "output_file_id", "error_file_id"} {
			field, present := doc.Root().Lookup(name)
			if !present || field.Kind() == oif.Null {
				continue
			}
			upstream, valid := field.Text()
			if !valid || upstream == "" {
				return nil, resources.ErrContract
			}
			mapped, err := s.mapUpstreamFile(ctx, res, upstream)
			if err != nil {
				return nil, err
			}
			files[name] = mapped.ID
		}
		return durableProjection(contract.Result, res.ID, files)
	}
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
	kind := resources.KindFile
	if owner.Kind == resources.KindStrictBatch {
		kind = resources.KindStrictFile
	}
	compatible := func(existing *resources.Resource) bool {
		return existing.RouteSlug == owner.RouteSlug && existing.ProviderRevisionID == owner.ProviderRevisionID && existing.SlotID == owner.SlotID &&
			(existing.CredentialID == nil) == (owner.CredentialID == nil) &&
			(existing.CredentialID == nil || *existing.CredentialID == *owner.CredentialID)
	}
	if existing, err := s.Resources.GetByUpstream(ctx, kind, owner.APIKeyID, owner.ProviderID, upstreamID); err == nil {
		if !compatible(existing) {
			return nil, resources.ErrContract
		}
		return existing, nil
	} else if !errors.Is(err, resources.ErrNotFound) {
		return nil, err
	}
	metadata := durableMetadata(resourceModel(owner))
	resource := &resources.Resource{
		Kind:               kind,
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
	}
	if kind == resources.KindStrictFile {
		_, batch, err := s.readDurable(ctx, owner.Kind, owner.APIKeyID, owner.ID)
		if err != nil {
			return nil, err
		}
		version := resources.DurableContractVersion
		resource.ContractVersion = &version
		resource.ExpiresAt = owner.ExpiresAt
		source, _ := json.Marshal(map[string]string{"batch_id": owner.ID, "upstream_file_id": upstreamID})
		payload, _ := json.Marshal(durableDocument{Version: version, Source: source, Binding: resourceModel(owner), Serving: batch.Serving})
		created, err := s.Resources.PutDurableContract(ctx, resource, payload)
		if err == nil {
			return created, nil
		}
		// Another authorized poll may have installed the same output-file
		// mapping concurrently. A collision owned by someone else still fails.
		if existing, readErr := s.Resources.GetByUpstream(ctx, kind, owner.APIKeyID, owner.ProviderID, upstreamID); readErr == nil && compatible(existing) {
			return existing, nil
		}
		return nil, err
	}
	created, err := s.Resources.Put(ctx, resource)
	if err == nil {
		return created, nil
	}
	if existing, readErr := s.Resources.GetByUpstream(ctx, kind, owner.APIKeyID, owner.ProviderID, upstreamID); readErr == nil && compatible(existing) {
		return existing, nil
	}
	return nil, err
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
	if x.strict() && (!authority.Policy.AllowProviderState || !s.Resources.Encrypted()) {
		s.stateFail(x, w, providerStateForbidden(), x.family)
		return
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		s.stateFail(x, w, permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."), x.family)
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
	// purpose=batch is the batch-input contract; any other purpose on a strict
	// route is an inference-file upload admitted only through the generation
	// operation and a profile-declared native purpose.
	inference := x.strict() && purpose != "batch"
	operation := "batch"
	if inference {
		operation = "generation"
	}
	if !slices.Contains(route.Operations, operation) {
		s.stateFail(x, w, invalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow "+operation+" operations.", nil), x.family)
		return
	}
	var strictSource []byte
	fileFirst := false
	if x.strict() {
		fields, normalized := form.SourceFields()
		fileFirst = len(fields) >= 2 && fields[0].Name == "file" && fields[0].File != nil
		fileLast := len(fields) >= 2 && fields[len(fields)-1].Name == "file" && fields[len(fields)-1].File != nil
		if normalized || (!fileFirst && !fileLast) || file.ContentType == "" ||
			strings.ContainsAny(file.Filename, "\"\\\r\n") || r.Header.Get("Idempotency-Key") != "" {
			s.stateFail(x, w, invalidRequest("target_capability", "Strict file upload requires one purpose field, one typed file field, and only qualified native options.", nil), x.family)
			return
		}
		purposeThenFile := len(fields) == 2 && fields[0].Name == "purpose" && fields[0].Text != nil && fields[1].Name == "file" && fields[1].File != nil
		fileThenPurpose := len(fields) == 2 && fields[0].Name == "file" && fields[0].File != nil && fields[1].Name == "purpose" && fields[1].Text != nil
		if !inference && !purposeThenFile && !fileThenPurpose {
			s.stateFail(x, w, invalidRequest("target_capability", "Strict batch file upload requires one purpose=batch and one typed file field without unqualified options.", nil), x.family)
			return
		}
		type sourceField struct {
			Name  string        `json:"name"`
			Value string        `json:"value,omitempty"`
			Asset *durableAsset `json:"asset,omitempty"`
		}
		ordered := make([]sourceField, 0, len(fields))
		for _, field := range fields {
			if field.Text != nil {
				ordered = append(ordered, sourceField{Name: field.Name, Value: *field.Text})
			} else {
				ordered = append(ordered, sourceField{Name: field.Name, Asset: &durableAsset{SHA256: file.Digest, Filename: file.Filename, ContentType: file.ContentType, Size: file.Size}})
			}
		}
		strictSource, _ = json.Marshal(ordered)
	}
	form.Disarm()
	defer func() {
		if err := s.transport().Spool.Remove(file.Handle); err != nil {
			s.log.Warn("file upload cleanup failed", "error", err)
		}
	}()
	var p *pin
	if inference {
		// The purpose and option contract is profile-owned; refuse clearly
		// when no route target admits this native upload shape at all.
		admitted := false
		for _, target := range route.Targets {
			provider, ok := snapshot.Providers[target.ProviderID]
			if !ok {
				continue
			}
			if profile, err := provider.Connector().Profile(); err == nil && fileUploadAdmitted(profile, purpose, extra) {
				admitted = true
				break
			}
		}
		if !admitted {
			s.stateFail(x, w, invalidRequest("target_capability", "The model `"+route.Slug+"` has no target admitting this file purpose and its options.", nil), x.family)
			return
		}
		// Inference-file uploads select their serving through the generation
		// contract so the committed binding is the same identity a later
		// strict generation plan requires.
		p, e = s.selectPinSurface(r.Context(), x, &route, "generation", "openai", "unary", func(provider *runtime.Provider, model string) bool {
			if !stateQualified(provider, model, "generation", "unary") {
				return false
			}
			profile, err := provider.Connector().Profile()
			return err == nil && fileUploadAdmitted(profile, purpose, extra)
		})
	} else {
		p, e = s.selectPin(r.Context(), x, &route, "batch", "unary")
	}
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	var strictServing oif.ServingIdentity
	var strictEndpoint string
	var strictItems int
	if x.strict() {
		if inference {
			template, ok := x.snapshot().InteractionTemplate(route.Slug, p.target.ID)
			if !ok {
				s.stateFail(x, w, invalidRequest("target_capability", "The selected target has no strict generation contract for managed files.", nil), x.family)
				return
			}
			strictServing = template.Serving()
		} else {
			template, ok := x.snapshot().DurableTemplate(route.Slug, p.target.ID)
			if !ok {
				s.stateFail(x, w, invalidRequest("target_capability", "The selected target has no strict batch contract.", nil), x.family)
				return
			}
			strictServing = template.Serving()
			var validationErr error
			strictEndpoint, strictItems, validationErr = s.validateBatchInput(file, template.Model(), &p.provider, &route)
			if validationErr != nil {
				s.stateFail(x, w, invalidRequest("target_capability", "The batch file has an unqualified item, duplicate identity, or mixed endpoint.", strPtr("file")), x.family)
				return
			}
		}
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
	resp, failure := s.uploadMultipart(ctx, x, p, endpoint, purpose, extra, file, fileFirst)
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
	if x.strict() {
		if _, _, err := strictResult(body, upstreamID); err != nil {
			s.stateFail(x, w, serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result."), x.family)
			return
		}
	}
	metadata, state, expires := fileMetadata(body, p.model)
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
			Asset: &durableAsset{SHA256: file.Digest, Filename: file.Filename, ContentType: file.ContentType, Size: file.Size, Endpoint: strictEndpoint, ItemCount: strictItems, Purpose: purpose}})
		if marshalErr != nil {
			s.stateFail(x, w, durableError(marshalErr), x.family)
			return
		}
		res, err = s.Resources.PutDurableContract(commitCtx, resource, payload)
	} else {
		res, err = s.Resources.Put(commitCtx, resource)
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be stored."), x.family)
		return
	}

	var out []byte
	if x.strict() {
		out, err = durableProjection(body, res.ID, nil)
	} else {
		out, err = rewriteID(body, "id", res.ID)
	}
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object."), x.family)
		return
	}
	s.writeStateJSON(w, x, out)
}

// fileUploadAdmitted reports whether the provider profile contract admits
// this managed upload: the native purpose is declared and every extra
// multipart field is a named native option carried verbatim.
func fileUploadAdmitted(profile connectors.Profile, purpose string, extra map[string]string) bool {
	if !slices.Contains(profile.FilePurposes, purpose) {
		return false
	}
	for name := range extra {
		if !slices.Contains(profile.FileOptions, name) {
			return false
		}
	}
	return true
}

func (s *Server) uploadMultipart(ctx context.Context, x *execution, p *pin, endpoint, purpose string, extra map[string]string, file *media.Part, fileFirst bool) (*http.Response, *attemptFailure) {
	pipeR, pipeW := io.Pipe()
	form := multipart.NewWriter(pipeW)
	sendDone := make(chan error, 1)
	go func() {
		failure := func(err error) error {
			pipeW.CloseWithError(err)
			return err
		}
		writeFile := func() error {
			opened, err := s.transport().Spool.Open(file.Handle)
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
		if fileFirst {
			if err := writeFile(); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		if err := form.WriteField("purpose", purpose); err != nil {
			sendDone <- failure(err)
			return
		}
		// Options replay in a fixed order; multipart field order carries no
		// meaning but the committed source records the caller's order.
		for _, name := range slices.Sorted(maps.Keys(extra)) {
			if err := form.WriteField(name, extra[name]); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		if !fileFirst {
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
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1)
	finish := func(class string, f *attemptFailure) *attemptFailure {
		if f == nil {
			f = &attemptFailure{}
		}
		f.class = class
		fact.Class = class
		f.attribute(&fact)
		fact.Committed = f.committed
		if fact.Interaction != nil && f.dispatched {
			if f.status > 0 {
				fact.Interaction.UpstreamState = usage.UpstreamTerminal
			} else {
				fact.Interaction.UpstreamState = usage.UpstreamUnknown
			}
		}
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(true)
		x.facts = append(x.facts, fact)
		return f
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pipeR)
	if err != nil {
		pipeR.CloseWithError(err)
		return nil, finish(classConnect, &attemptFailure{origin: faultContract, scope: scopeRequest})
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("User-Agent", "olp-go/gateway")
	req.Header.Set("Accept", "application/json")
	if _, err := s.auth.Apply(ctx, req, p.provider.Connector(), s.pinSecret(x, p), nil); err != nil {
		pipeR.CloseWithError(err)
		return nil, finish(classCredential, &attemptFailure{origin: faultContract, scope: scopeCredential})
	}
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		pipeR.CloseWithError(err)
		return nil, finish(classCredential, &attemptFailure{origin: faultContract, scope: scopeCredential})
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
	// A provider may reply before consuming the entire multipart body. Close
	// the read side so the writer cannot wait forever for a peer that has
	// already accepted or rejected partial work.
	_ = pipeR.CloseWithError(io.ErrClosedPipe)
	if sendErr := <-sendDone; sendErr != nil {
		resp.Body.Close()
		// The multipart body is produced from this gateway's own staged
		// uploads; its failure after the provider already answered is local
		// persistence, not endpoint evidence.
		return nil, finish(classConnect, &attemptFailure{dispatched: true, origin: faultProxyPersistence, scope: scopeRequest})
	}
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte = &received
	fact.Status = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fact.Class = "success"
		fact.Committed = true
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = usage.UpstreamAccepted
		}
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
	kinds := []string{resources.KindFile}
	if authority.Policy.AllowProviderState {
		kinds = append(kinds, resources.KindStrictFile)
	}
	rows, err := s.Resources.ListKinds(r.Context(), kinds, authority.ID, limit, after)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file list could not be read."), x.family)
		return
	}
	items := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if row.Kind == resources.KindStrictFile {
			_, contract, readErr := s.readDurable(r.Context(), row.Kind, authority.ID, row.ID)
			if readErr != nil || s.authorizeDurable(r.Context(), x, authority, row, contract, durableOperation(contract)) != nil {
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
				if err := s.Resources.Tombstone(ctx, res.ID); err != nil {
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
		metadata, state, expires := fileMetadata(body, resourceModel(res))
		var out []byte
		if res.Kind == resources.KindStrictFile {
			if _, _, err := strictResult(body, res.UpstreamID); err != nil {
				return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result.")
			}
			_, contract, err := s.readDurable(ctx, res.Kind, authority.ID, res.ID)
			if err != nil {
				return durableError(err)
			}
			contract.Result = body
			payload, err := json.Marshal(contract)
			if err != nil {
				return durableError(err)
			}
			commitCtx, stopCommit := resourceCommitContext(ctx)
			defer stopCommit()
			if _, _, err = s.Resources.UpdateDurableContract(commitCtx, res.Kind, authority.ID, res.ID, state, payload); err != nil {
				return durableError(err)
			}
			out, err = durableProjection(body, res.ID, nil)
		} else {
			if err := s.Resources.Update(ctx, res.ID, state, metadata, expires); err != nil {
				return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed file state could not be committed.")
			}
			res.State, res.Metadata, res.ExpiresAt = state, metadata, expires
			out, err = rewriteID(body, "id", res.ID)
		}
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
				commitCtx, stopCommit := resourceCommitContext(ctx)
				defer stopCommit()
				if err := s.Resources.Tombstone(commitCtx, res.ID); err != nil {
					return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
				}
				return notFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		x.dispatched = true
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
		commitCtx, stopCommit := resourceCommitContext(ctx)
		defer stopCommit()
		if err := s.Resources.Tombstone(commitCtx, res.ID); err != nil {
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
		if res.Kind == resources.KindStrictFile {
			w.Header().Set("X-OLP-Content-SHA256", artifact.Digest)
		}
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
	operation := durableOperation(contract)
	if e := s.authorizeDurable(r.Context(), x, authority, res, contract, operation); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	p, route, e := s.resolveResource(r.Context(), x, authority, res, operation)
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
	if len(x.facts) > 0 {
		if evidence := x.facts[len(x.facts)-1].Interaction; evidence != nil {
			evidence.ClientState = usage.ClientTerminal
		}
	}
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
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.cfg.MaxBodyBytes)})
	if err != nil || source.Root().Kind() != oif.Object {
		s.stateFail(x, w, invalidRequest("invalid_json", "The request body must be one JSON object.", nil), x.family)
		return
	}
	if _, supplied := source.Root().Lookup("id"); supplied {
		s.stateFail(x, w, invalidRequest("invalid_request", "A new batch cannot supply a provider resource ID.", strPtr("id")), x.family)
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
	p, route, e := s.resolveResource(r.Context(), x, authority, file, "batch")
	if e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	x.route = route
	if x.strict() {
		if fileKind != resources.KindStrictFile || fileContract == nil || !authority.Policy.AllowProviderState || !s.Resources.Encrypted() {
			s.stateFail(x, w, invalidRequest("resource_affinity", "Strict batches require a file uploaded under the same strict route and state-enabled key.", strPtr("input_file_id")), x.family)
			return
		}
		if e := s.authorizeDurable(r.Context(), x, authority, file, fileContract, "batch"); e != nil {
			s.stateFail(x, w, e, x.family)
			return
		}
		requestEndpoint, present := source.Root().Lookup("endpoint")
		path, valid := requestEndpoint.Text()
		if !present || !valid || fileContract.Asset == nil || fileContract.Asset.Endpoint != path || fileContract.Asset.ItemCount < 1 {
			s.stateFail(x, w, invalidRequest("resource_affinity", "The batch endpoint must match every qualified item in its uploaded file.", strPtr("endpoint")), x.family)
			return
		}
	}
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
	upstreamFile, _ := json.Marshal(file.UpstreamID)
	var upstream []byte
	var strictServing oif.ServingIdentity
	if x.strict() {
		template, ok := x.snapshot().DurableTemplate(route.Slug, p.target.ID)
		if !ok {
			s.stateFail(x, w, invalidRequest("target_capability", "The selected target has no strict batch contract.", nil), x.family)
			return
		}
		bound, bindErr := template.BindBatch(source, route.Slug, localFile, file.UpstreamID)
		if bindErr != nil {
			s.stateFail(x, w, invalidRequest("target_capability", "The batch source cannot satisfy this native target contract.", nil), x.family)
			return
		}
		upstream = bound.Effective.Document().Bytes()
		strictServing = bound.Receipt.Serving
	} else {
		changes := []oif.Change{{Pointer: "/input_file_id", Value: string(upstreamFile), Origin: oif.ResourceBinding, Reason: "owner-scoped uploaded file"}}
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
	if x.strict() {
		validated, _, err := strictBatchResult(result, upstreamID)
		if err != nil || !strictBatchInputMatches(validated, upstream) {
			s.stateFail(x, w, serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native batch result."), x.family)
			return
		}
	}
	metadata, state := batchMetadata(result, p.model)
	commitCtx, stopCommit := resourceCommitContext(ctx)
	defer stopCommit()
	kind := resources.KindBatch
	if x.strict() {
		kind = resources.KindStrictBatch
		metadata = durableMetadata(p.model)
	}
	resource := &resources.Resource{
		Kind:               kind,
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
	}
	var res *resources.Resource
	if x.strict() {
		version := resources.DurableContractVersion
		resource.ContractVersion = &version
		resource.ExpiresAt = s.durableExpiry(nil)
		payload, marshalErr := json.Marshal(durableDocument{Version: version, Source: source.Bytes(), Effective: upstream, Result: result, Binding: p.model, Serving: strictServing})
		if marshalErr != nil {
			s.stateFail(x, w, durableError(marshalErr), x.family)
			return
		}
		res, err = s.Resources.PutDurableContract(commitCtx, resource, payload)
	} else {
		res, err = s.Resources.Put(commitCtx, resource)
	}
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
	if e := s.authorizeDurable(r.Context(), x, authority, res, contract, "batch"); e != nil {
		s.stateFail(x, w, e, x.family)
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
	kinds := []string{resources.KindBatch}
	if authority.Policy.AllowProviderState {
		kinds = append(kinds, resources.KindStrictBatch)
	}
	rows, err := s.Resources.ListKinds(r.Context(), kinds, authority.ID, limit, after)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The batch list could not be read."), x.family)
		return
	}
	items := make([]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if row.Kind == resources.KindStrictBatch {
			_, contract, readErr := s.readDurable(r.Context(), row.Kind, authority.ID, row.ID)
			if readErr != nil || s.authorizeDurable(r.Context(), x, authority, row, contract, "batch") != nil {
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
		if res.Kind == resources.KindStrictBatch {
			switch res.State {
			case "completed", "failed", "expired", "cancelled":
				return serverError(http.StatusConflict, "resource_transition", "This batch has already reached a terminal state.")
			case "cancelling":
				out, err := s.batchObject(ctx, res)
				if err != nil {
					return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The accepted cancellation could not be projected.")
				}
				s.writeStateJSON(w, x, out)
				return nil
			}
		}
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
		contract.Result = result
		payload, err := json.Marshal(contract)
		if err != nil {
			return durableError(err)
		}
		if _, _, err = s.Resources.UpdateDurableContract(commitCtx, res.Kind, res.APIKeyID, res.ID, state, payload); err != nil {
			return durableError(err)
		}
	} else {
		if err := s.Resources.Update(commitCtx, res.ID, state, metadata, nil); err != nil {
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
