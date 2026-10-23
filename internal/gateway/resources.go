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

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

const stateRouteHeader = "X-OLP-Route"

func (s *Server) registerState(mux *http.ServeMux) {
	if s.Resources == nil {
		return
	}
	s.registerRetainedLifecycle(mux)
	s.registerCloudBatchState(mux)
	if s.Media != nil {
		s.registerAnthropicState(mux)
		s.registerGeminiFilesState(mux)
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
	target   routing.Target
	provider routing.Provider
	attempt  routing.Attempt
	slot     routing.Slot
	model    string
	hold     *dispatchHold
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

func stateQualified(p *routing.Provider, model, operation, mode string) bool {
	if !p.Connector().Supports(operation, "openai", mode) {
		return false
	}
	if !p.Supports(model, operation, "openai", mode) {
		return false
	}
	return p.Connector().SupportsFilesBatches()
}

func fireworksBatchRefusal(x *call, route *routing.Route) *Error {
	if x == nil || route == nil {
		return nil
	}
	for _, target := range route.Targets {
		provider, ok := x.snapshot().Providers[target.ProviderID]
		if ok && provider.Kind == "openai_compatible" && provider.VendorID == "fireworks" {
			return policyUnavailable("target_capability",
				"Fireworks documents no OpenAI-compatible files/batches API; its dataset batch jobs return results only through provider-returned signed URLs, which the egress contract forbids.")
		}
	}
	return nil
}

func strictBatchTargetRefusal(p *pin) *Error {
	if p != nil && p.provider.Kind != "openai" && p.provider.Kind != "azure_openai" {
		return policyUnavailable("target_capability",
			"Strict routes serve files and batches only for OpenAI and Azure.")
	}
	return nil
}

func resourceURL(cfg connectors.Config, model, path string, query url.Values) (string, error) {
	return cfg.ResourceURL(model, path, query)
}

func resourceModel(res *resources.Resource) string {
	var meta struct {
		UpstreamModel string `json:"upstream_model"`
	}
	_ = json.Unmarshal(res.Metadata, &meta)
	return meta.UpstreamModel
}

func (s *Server) selectPin(ctx context.Context, x *call, route *routing.Route, operation, mode string) (*pin, *Error) {
	return s.selectPinSurface(ctx, x, route, operation, "openai", mode, func(p *routing.Provider, model string) bool {
		return stateQualified(p, model, operation, mode)
	})
}

func (s *Server) selectPinSurface(ctx context.Context, x *call, route *routing.Route, operation, surface, mode string, qualified func(*routing.Provider, string) bool) (*pin, *Error) {
	return s.selectBoundPinSurface(ctx, x, route, operation, surface, mode, nil, qualified)
}

func (s *Server) selectBoundPinSurface(ctx context.Context, x *call, route *routing.Route, operation, surface, mode string, bound *resources.Resource, qualified func(*routing.Provider, string) bool) (*pin, *Error) {
	if e := s.checkRouteFidelity(ctx, route); e != nil {
		return nil, e
	}
	if e := x.checkBody(route); e != nil {
		return nil, e
	}
	snapshot := x.request.release.Snapshot
	// Pinned work must select a target on this route rather than delegate.
	x.fixed = true
	options := s.selectionOptions(ctx, x)
	options.Accept = func(p routing.Provider, t routing.Target) error {
		if bound != nil && (bound.ProviderID != p.ID || bound.ProviderRevisionID != p.RevisionID || bound.TargetID != t.ID) {
			return errors.New("resource affinity unavailable")
		}
		if !qualified(&p, t.ProviderModel) {
			return errors.New("provider capability unavailable")
		}
		return nil
	}
	plan, err := routing.Select(snapshot, route.Slug, operation, surface, mode, x.affinity, options)
	if err != nil {
		return nil, requestError(err)
	}
	if len(plan.Attempts) == 0 {
		x.noteCostExceeded(plan)
		if x.costExcluded {
			return nil, costLimitExceeded()
		}
		return nil, selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, route.Slug)
	}
	attempt := plan.Attempts[0]
	provider, ok := snapshot.Providers[attempt.ProviderID]
	if !ok {
		return nil, selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, route.Slug)
	}
	if e := x.checkCallerTarget(&provider, attempt.UpstreamModel); e != nil {
		return nil, e
	}
	var target routing.Target
	for _, candidate := range route.Targets {
		if candidate.ID == attempt.TargetID {
			target = candidate
			break
		}
	}
	if target.ID == "" {
		return nil, selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, route.Slug)
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
		if bound != nil && (bound.SlotID != slot.ID || !sameCredential(bound.CredentialID, slot.CredentialID)) {
			continue
		}
		if !s.slotAvailable(x, attempt, slot) || s.cooling(ctx, provider.ID, slot) {
			continue
		}
		gate := s.gateAttempt(ctx, x, attempt, &provider, slot, max(resourceEstimate, x.estimate), deadline)
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

func (s *Server) pinnedDo(ctx context.Context, x *call, p *pin, method, endpoint string, body []byte, contentType string) (*http.Response, *attemptFailure) {
	exchange := s.beginPinned(ctx, x, p, method)
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, exchange.fail(s, x, classConnect, nil)
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	if _, err := s.egress.ValidateEndpoint(parsed.String()); err != nil {
		return nil, exchange.fail(s, x, classConnect, nil)
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, exchange.fail(s, x, classConnect, nil)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "olp/gateway")
	accept := "application/json"
	if x.anthropicFileBinary || x.geminiFileBinary {
		accept = "application/binary"
	}
	req.Header.Set("Accept", accept)
	if x.mode == "streaming" && (x.family == wire.GeminiInteractions || x.family == wire.Responses) {
		req.Header.Set("Accept", "text/event-stream")
	}
	if err := s.applySlotCredential(ctx, x, req, p.provider.Connector(), p.slot, body, &p.provider, p.model); err != nil {
		if ctx.Err() != nil {
			return nil, exchange.fail(s, x, classCancelled, nil)
		}
		return nil, exchange.fail(s, x, classCredential, nil)
	}
	if x.anthropicFilesBeta {
		req.Header.Set("Anthropic-Beta", anthropicFilesBeta)
	}
	return s.doPinnedRequest(ctx, x, p, exchange, req, openai.ParseErrorBody)
}

func upstreamError(f *attemptFailure) *Error {
	if f.upstream != nil && f.upstream.Message != "" {
		return &Error{Status: f.status, Type: f.upstream.Type, Code: f.upstream.Code, Message: f.upstream.Message}
	}
	return serverError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
}

func (s *Server) stateBegin(w http.ResponseWriter, r *http.Request, family wire.Format) (*call, access.Authority, bool) {
	x := &call{request: s.begin(w, r), family: family, actor: "api_key"}
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
	s.bindAuthority(x, authority)
	switch family {
	case wire.AnthropicFile, wire.AnthropicBatch, wire.GeminiFile, wire.GeminiBatch, wire.CloudBatch:
		if !authority.Policy.AllowProviderState {
			s.release(r.Context())
			s.stateFail(x, w, providerStateForbidden(), family)
			return x, authority, true
		}
	}
	if x.attribution, e = s.parseAttribution(r, authority); e != nil {
		s.release(r.Context())
		s.stateFail(x, w, e, family)
		return x, access.Authority{}, true
	}
	return x, authority, false
}

func (s *Server) stateFail(x *call, w http.ResponseWriter, e *Error, family wire.Format) {
	x.failure = e
	s.finish(x, nil, e.Status)
	writeSurfaceError(w, e, family.Surface())
}

func (s *Server) stateDeadline(ctx context.Context, route *routing.Route) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(route.OverallTimeout)*time.Millisecond)
}

func (s *Server) reserveState(ctx context.Context, x *call, authority access.Authority, overall time.Duration) *Error {
	if e := x.checkBody(x.route); e != nil {
		return e
	}
	var e *Error
	x.lease, e = s.Admission.reserveKey(ctx, x.admissionAuthority(authority), x.clientSurface(), max(resourceEstimate, x.estimate), overall, x.limitRoute())
	return e
}

func (s *Server) resourceSettle(ctx context.Context, x *call, p *pin) {
	if p != nil {
		s.settlePinHold(ctx, x, p.hold, nil)
	}
	s.settleCaps(ctx, x)
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

// nativeResourceID proves one unambiguous native identity before a provider
// result may create or update its retained mapping. The source parser refuses
// duplicate (including escaped) keys and invalid Unicode spellings.
func nativeResourceID(body []byte, maxBytes int64) (string, bool) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: int(maxBytes)})
	if err != nil {
		return "", false
	}
	id, present := source.Root().Lookup("id")
	upstream, valid := id.Text()
	return upstream, present && valid && upstream != "" && len(upstream) <= 512
}

func retainedIdentityMatches(body []byte, expected string, maxBytes int64) bool {
	upstream, valid := nativeResourceID(body, maxBytes)
	return valid && upstream == expected
}

func rewriteID(body []byte, name, value string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("provider result is not a JSON object")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	obj[name] = encoded
	return json.Marshal(obj)
}

// rewritePrevious replaces an echoed upstream previous_response_id with its
// local id, or with null when no local id is known, so a provider identifier
// never reaches the caller.
func rewritePrevious(body []byte, local string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	raw, present := obj["previous_response_id"]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return body, nil
	}
	obj["previous_response_id"] = json.RawMessage("null")
	if local != "" {
		encoded, err := json.Marshal(local)
		if err != nil {
			return nil, err
		}
		obj["previous_response_id"] = encoded
	}
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
	var adapter string
	if json.Unmarshal(meta["adapter"], &adapter) == nil && (adapter == "together" || adapter == "mistral" || adapter == "gemini_managed") {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(meta["provider_file"], &fields)
		obj := map[string]json.RawMessage{"object": json.RawMessage(`"file"`)}
		for _, name := range []string{"bytes", "created_at", "filename", "purpose"} {
			if value, ok := fields[name]; ok {
				obj[name] = value
			}
		}
		obj["id"], _ = json.Marshal(res.ID)
		return json.Marshal(obj)
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

func fileAdapter(res *resources.Resource) string {
	var metadata struct {
		Adapter string `json:"adapter"`
	}
	if res != nil {
		_ = json.Unmarshal(res.Metadata, &metadata)
	}
	return metadata.Adapter
}

func managedGeminiFilePath(res *resources.Resource) (string, error) {
	name := geminiManagedFileName(res)
	id, err := geminiFileID(name)
	if err != nil {
		return "", err
	}
	return "files/" + url.PathEscape(id), nil
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
			mapped, err := s.mapUpstreamFile(ctx, res, upstream, strings.TrimSuffix(name, "_file_id"))
			if err != nil {
				return nil, err
			}
			files[name] = mapped.ID
		}
		return durableProjection(contract.Result, res.ID, files)
	}
	if batchAdapter(res) == "gemini_managed" {
		return s.geminiManagedBatchObject(ctx, res)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(res.Metadata, &meta); err != nil {
		return nil, err
	}
	obj := map[string]json.RawMessage{}
	for name, raw := range meta {
		if name == "upstream_model" || name == batchLocalInputKey || name == "adapter" || name == "manifest" || name == "provider_file" || name == "role" {
			continue
		}
		obj[name] = raw
	}
	// The input file is the one the client named at create time. Projecting
	// it from the stored local ID keeps it stable after the client deletes
	// that file, instead of minting a new mapping for the upstream ID.
	if local, ok := meta[batchLocalInputKey]; ok {
		obj["input_file_id"] = local
	}
	// Stored provider fields are upstream identifiers, whatever their
	// spelling. Output and error files are first seen here, so they get a
	// mapping on demand.
	for _, name := range []string{"input_file_id", "output_file_id", "error_file_id"} {
		if name == "input_file_id" && batchLocalInput(res) != "" {
			continue
		}
		raw, ok := obj[name]
		if !ok {
			continue
		}
		var upstreamID string
		if json.Unmarshal(raw, &upstreamID) != nil || upstreamID == "" {
			continue
		}
		mapped, err := s.mapUpstreamFile(ctx, res, upstreamID, strings.TrimSuffix(name, "_file_id"))
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

func (s *Server) mapUpstreamFile(ctx context.Context, owner *resources.Resource, upstreamID, role string) (*resources.Resource, error) {
	kind := resources.KindFile
	if owner.Kind == resources.KindStrictBatch {
		kind = resources.KindStrictFile
	}
	identity := owner.NativeIdentity()
	identity.Kind, identity.UpstreamID = kind, upstreamID
	if existing, err := s.Resources.GetByUpstream(ctx, identity); err == nil {
		return existing, nil
	} else if !errors.Is(err, resources.ErrNotFound) {
		return nil, err
	}
	metadata := durableMetadata(resourceModel(owner))
	var ownerMetadata map[string]json.RawMessage
	_ = json.Unmarshal(owner.Metadata, &ownerMetadata)
	var adapter string
	if json.Unmarshal(ownerMetadata["adapter"], &adapter) == nil && (adapter == "together" || adapter == "mistral") {
		metadata = managedFileMetadata(metadata, nil, adapter, batchManifest{}, role)
	}
	resource := &resources.Resource{
		Kind:               kind,
		APIKeyID:           owner.APIKeyID,
		RouteSlug:          owner.RouteSlug,
		ProviderID:         owner.ProviderID,
		ProviderRevisionID: owner.ProviderRevisionID,
		RouteRevisionID:    owner.RouteRevisionID,
		TargetID:           owner.TargetID,
		SlotID:             owner.SlotID,
		CredentialID:       owner.CredentialID,
		UpstreamID:         upstreamID,
		ParentID:           new(owner.UUID),
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
		if existing, readErr := s.Resources.GetByUpstream(ctx, identity); readErr == nil {
			return existing, nil
		}
		return nil, err
	}
	created, err := s.Resources.Put(ctx, resource)
	if err == nil {
		return created, nil
	}
	if existing, readErr := s.Resources.GetByUpstream(ctx, identity); readErr == nil {
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

// batchLocalInputKey stores the gateway file ID a transformed batch was
// created from beside the provider's batch object.
const batchLocalInputKey = "local_input_file_id"

// batchLocalInput returns the gateway input file ID stored on a batch.
func batchLocalInput(res *resources.Resource) string {
	var meta map[string]json.RawMessage
	if json.Unmarshal(res.Metadata, &meta) != nil {
		return ""
	}
	var local string
	_ = json.Unmarshal(meta[batchLocalInputKey], &local)
	return local
}

func batchMetadata(body []byte, model, localInput string) (json.RawMessage, string) {
	meta := map[string]json.RawMessage{}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) == nil {
		for name, raw := range obj {
			if name == "id" || name == batchLocalInputKey {
				continue
			}
			meta[name] = raw
		}
	}
	if model != "" {
		encoded, _ := json.Marshal(model)
		meta["upstream_model"] = encoded
	}
	if localInput != "" {
		encoded, _ := json.Marshal(localInput)
		meta[batchLocalInputKey] = encoded
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
	x, authority, done := s.stateBegin(w, r, wire.File)
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
	if !slices.Contains(route.Operations, "batch") {
		s.stateFail(x, w, invalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow batch operations.", nil), x.family)
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
	var strictSource []byte
	fileFirst := false
	if x.strict() {
		fields, normalized := form.SourceFields()
		fileFirst = len(fields) == 2 && fields[0].Name == "file" && fields[0].File != nil && fields[1].Name == "purpose" && fields[1].Text != nil
		purposeFirst := len(fields) == 2 && fields[0].Name == "purpose" && fields[0].Text != nil && fields[1].Name == "file" && fields[1].File != nil
		if normalized || !purposeFirst && !fileFirst || len(extra) != 0 || purpose != "batch" ||
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
	adapter := p.provider.Connector().FilesBatchesAdapter()
	uploadFile := file
	uploadPurpose, uploadExtra := purpose, extra
	var manifest batchManifest
	if p.provider.VendorID == "groq" && purpose != "batch" {
		param := "purpose"
		s.releaseHold(r.Context(), p.hold)
		s.stateFail(x, w, invalidRequest("unsupported_parameter", "Groq files used by batches require purpose=batch.", &param), x.family)
		return
	}
	if adapter == "together" || adapter == "mistral" || adapter == "gemini_managed" || adapter == "openai" && !x.strict() && purpose == "batch" {
		if adapter != "openai" && (purpose != "batch" || len(extra) > 0) {
			param := "purpose"
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, invalidRequest("unsupported_parameter", "Managed batch uploads require purpose=batch and no extra fields.", &param), x.family)
			return
		}
		var translated *media.Part
		translated, manifest, e = s.translateManagedBatchInput(r.Context(), p, &route, file, adapter)
		if e != nil {
			s.releaseHold(r.Context(), p.hold)
			s.stateFail(x, w, e, x.family)
			return
		}
		uploadFile = translated
		defer func() {
			if err := s.transport().Spool.Remove(translated.Handle); err != nil {
				s.log.Warn("translated batch spool cleanup failed", "error", err)
			}
		}()
		if adapter == "together" {
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
	if adapter == "gemini_managed" {
		var failure *attemptFailure
		body, upstreamID, failure = s.uploadGeminiManagedFile(ctx, x, p, uploadFile)
		if failure != nil {
			x.dispatched = failure.dispatched
			s.stateFail(x, w, upstreamError(failure), x.family)
			return
		}
	} else {
		filePath := "files"
		if adapter == "together" {
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
	metadata, state, expires := fileMetadata(body, p.model)
	if manifest.Adapter == "openai" {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(metadata, &fields)
		fields["batch_manifest"] = mustJSON(manifest)
		metadata = mustJSON(fields)
	}
	if adapter == "together" || adapter == "mistral" || adapter == "gemini_managed" {
		metadata = managedFileMetadata(metadata, body, adapter, manifest, "input")
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
	} else if adapter == "together" || adapter == "mistral" || adapter == "gemini_managed" {
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

func (s *Server) uploadMultipart(ctx context.Context, x *call, p *pin, endpoint, purpose string, extra map[string]string, file *media.Part, fileFirst bool) (*http.Response, *attemptFailure) {
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
	rows, err := s.Resources.ListKinds(r.Context(), kinds, authority.ID, limit+1, after)
	if err != nil {
		s.stateFail(x, w, serverError(http.StatusInternalServerError, "internal_error", "The file list could not be read."), x.family)
		return
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	items := make([]json.RawMessage, 0, len(rows))
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
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
	s.fileCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, authority access.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
		path := "files/" + url.PathEscape(res.UpstreamID)
		if fileAdapter(res) == "gemini_managed" {
			var err error
			path, err = managedGeminiFilePath(res)
			if err != nil {
				return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
			}
		}
		endpoint, err := resourceURL(p.provider.Connector(), p.model, path, nil)
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
		adapter := fileAdapter(res)
		if adapter == "gemini_managed" {
			var object map[string]json.RawMessage
			var name string
			if json.Unmarshal(body, &object) != nil || json.Unmarshal(object["name"], &name) != nil || name != geminiManagedFileName(res) {
				return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini file name.")
			}
		} else if res.Kind != resources.KindStrictFile {
			if !retainedIdentityMatches(body, res.UpstreamID, s.cfg.MaxResponseBytes) {
				return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed file object.")
			}
		}
		metadata, state, expires := fileMetadata(body, resourceModel(res))
		if res.Kind != resources.KindStrictFile {
			var prior map[string]json.RawMessage
			_ = json.Unmarshal(res.Metadata, &prior)
			if manifest, ok := prior["batch_manifest"]; ok {
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(metadata, &fields)
				fields["batch_manifest"] = manifest
				metadata = mustJSON(fields)
			}
			var adapter, role string
			_ = json.Unmarshal(prior["adapter"], &adapter)
			_ = json.Unmarshal(prior["role"], &role)
			if adapter == "together" || adapter == "mistral" || adapter == "gemini_managed" {
				metadata = managedFileMetadata(res.Metadata, body, adapter, batchManifest{}, role)
			}
		}
		var out []byte
		if res.Kind == resources.KindStrictFile {
			if _, _, err := strictResult(body, res.UpstreamID); err != nil {
				return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid native file result.")
			}
			x.completePinned()
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
			x.completePinned()
			if err := s.Resources.Update(ctx, res.ID, state, metadata, expires); err != nil {
				return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed file state could not be committed.")
			}
			res.State, res.Metadata, res.ExpiresAt = state, metadata, expires
			if adapter == "together" || adapter == "mistral" || adapter == "gemini_managed" {
				out, err = s.fileObject(ctx, res)
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
	s.fileCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, authority access.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
		writeDeleted := func() {
			s.writeStateJSON(w, x, mustJSON(map[string]any{"id": res.ID, "object": "file", "deleted": true}))
		}
		if fileAdapter(res) == "gemini_managed" {
			var fields struct {
				Role string `json:"role"`
			}
			_ = json.Unmarshal(res.Metadata, &fields)
			if fields.Role == "output" || fields.Role == "error" {
				commitCtx, stop := resourceCommitContext(ctx)
				defer stop()
				detached, pending, err := s.Resources.DetachGeminiResultView(commitCtx, res, geminiManagedFileName(res))
				if errors.Is(err, resources.ErrNotFound) {
					return notFoundError("not_found", "No file with this identifier exists for this key.")
				}
				if err != nil {
					return serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "The result file dependencies could not be read.")
				}
				if detached {
					writeDeleted()
					return nil
				}
				if pending {
					return serverError(http.StatusConflict, "batch_settlement_pending", "The result file is still needed for settlement; retry after settlement closes.")
				}
			}
		}
		if fileAdapter(res) != "gemini_managed" && res.ParentID != nil {
			if err := s.requireBatchEvidenceSettled(ctx, *res.ParentID); err != nil {
				return err
			}
		}
		path := "files/" + url.PathEscape(res.UpstreamID)
		if fileAdapter(res) == "gemini_managed" {
			var err error
			path, err = managedGeminiFilePath(res)
			if err != nil {
				return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The Gemini file mapping is malformed.")
			}
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
				if err := s.Resources.Tombstone(commitCtx, res.ID); err != nil {
					return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The missing file state could not be recorded.")
				}
				return notFoundError("not_found", "No file with this identifier exists for this key.")
			}
			return upstreamError(failure)
		}
		defer resp.Body.Close()
		x.dispatched = true
		// A successful DELETE status is the operation's terminal contract.
		x.completePinned()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
		commitCtx, stopCommit := resourceCommitContext(ctx)
		defer stopCommit()
		if err := s.Resources.Tombstone(commitCtx, res.ID); err != nil {
			return serverError(http.StatusInternalServerError, "internal_error", "The file mapping could not be deleted.")
		}
		writeDeleted()
		return nil
	})
}

func (s *Server) fileContent(w http.ResponseWriter, r *http.Request) {
	s.fileCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, authority access.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
		if fileAdapter(res) == "gemini_managed" {
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
				fact.Class = classProtocol
			}
			var tooLarge *media.TooLargeError
			if errors.As(err, &tooLarge) {
				return serverError(http.StatusBadGateway, "upstream_response_too_large", "The provider file exceeded the configured response bound.")
			}
			return serverError(http.StatusBadGateway, "media_content_unavailable", "The provider file could not be staged.")
		}
		x.completePinned()
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

func (s *Server) fileCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, access.Authority, *resources.Resource, *pin, *routing.Route) *Error) {
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
	p, route, e := s.admitDurable(r.Context(), x, authority, res, contract, "batch", use)
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
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
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

func (s *Server) writeStateJSON(w http.ResponseWriter, x *call, body []byte) {
	s.writeStateJSONStatus(w, x, body, http.StatusOK)
}

func (s *Server) writeStateJSONStatus(w http.ResponseWriter, x *call, body []byte, status int) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if body != nil {
		w.Header().Set("Content-Type", "application/json")
	}
	x.responseHeaders(w.Header(), false)
	w.WriteHeader(status)
	n, err := w.Write(body)
	if err == nil && n == len(body) {
		err = http.NewResponseController(w).Flush()
	}
	if err != nil || n != len(body) {
		x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The resource result could not be delivered.")
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class = classCancelled
			if fact.Interaction != nil && n > 0 {
				fact.Interaction.ClientState = usage.ClientPartial
			}
		}
		s.finish(x, &outcome{err: x.failure, committed: true, cancelled: true}, 0)
		return
	}
	if n > 0 {
		x.delivered(s.now())
	}
	if len(x.facts) > 0 {
		if evidence := x.facts[len(x.facts)-1].Interaction; evidence != nil {
			evidence.ClientState = usage.ClientTerminal
		}
	}
	s.finish(x, &outcome{committed: true}, status)
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

// listBody writes the OpenAI cursor page shape: SDK auto-pagination follows
// has_more and resumes after the last item's id.
func listBody(items []json.RawMessage, ids []string, hasMore bool) []byte {
	data, _ := json.Marshal(items)
	more, _ := json.Marshal(hasMore)
	body := map[string]json.RawMessage{
		"object":   json.RawMessage(`"list"`),
		"data":     data,
		"has_more": more,
	}
	if len(ids) > 0 {
		body["first_id"], _ = json.Marshal(ids[0])
		body["last_id"], _ = json.Marshal(ids[len(ids)-1])
	}
	out, _ := json.Marshal(body)
	return out
}

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
	if candidate, exists := x.snapshot().Routes[file.RouteSlug]; exists {
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
		if fileKind != resources.KindStrictFile || fileContract == nil || !authority.Policy.AllowProviderState || !s.Resources.Encrypted() {
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
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
	if gate.verdict != gateAdmitted {
		s.stateFail(x, w, gateError(gate), x.family)
		return
	}
	p.hold = gate.hold
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
			if cleanupErr := s.Batches.DeleteSubmission(cleanupCtx, submission.resource.UUID); cleanupErr != nil {
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
			if err := s.Batches.MarkUnknown(unknownCtx, submission.resource.UUID, "submission_unknown", s.now()); err != nil {
				s.log.Warn("batch submission outcome could not be recorded", "resource_id", submission.resource.UUID, "error", err)
			}
			s.stateFail(x, w, serverError(http.StatusBadGateway, "submission_unknown", "The provider may have accepted this batch, but its outcome is unknown."), x.family)
			return
		}
		cleanupCtx, stopCleanup := resourceCommitContext(r.Context())
		defer stopCleanup()
		if err := s.Batches.DeleteSubmission(cleanupCtx, submission.resource.UUID); err != nil {
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
			if markErr := s.Batches.MarkUnknown(unknownCtx, submission.resource.UUID, "submission_unknown", s.now()); markErr != nil {
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
			if markErr := s.Batches.MarkUnknown(unknownCtx, submission.resource.UUID, "submission_unknown", s.now()); markErr != nil {
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
			if markErr := s.Batches.MarkUnknown(commitCtx, resource.UUID, "submission_unknown", s.now()); markErr != nil {
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
		tx, beginErr := s.Resources.Begin(commitCtx)
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
			created, beginErr = s.Resources.PutDurableContractTx(commitCtx, tx, resource, payload)
		} else {
			created, beginErr = s.Resources.PutTx(commitCtx, tx, resource)
		}
		if beginErr != nil {
			return nil, beginErr
		}
		acceptedExpected := expectedResults
		if acceptedExpected == nil {
			acceptedExpected = batchExpectedResults(result)
		}
		if beginErr = s.Batches.ActivateTx(commitCtx, tx, resource.UUID, upstreamID, acceptedExpected); beginErr != nil {
			return nil, beginErr
		}
		if beginErr = tx.Commit(commitCtx); beginErr != nil {
			return nil, beginErr
		}
		return created, nil
	}()
	if err != nil {
		if markErr := s.Batches.MarkUnknown(commitCtx, resource.UUID, "submission_commit_unavailable", s.now()); markErr != nil {
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

func (s *Server) batchCall(w http.ResponseWriter, r *http.Request, use retainedUse, op func(context.Context, *call, access.Authority, *resources.Resource, *pin, *routing.Route) *Error) {
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
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.stateFail(x, w, e, x.family)
		return
	}
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(time.Duration(route.OverallTimeout)*time.Millisecond), "")
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
	rows, err := s.Resources.ListKinds(r.Context(), kinds, authority.ID, limit+1, after)
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
	s.batchCall(w, r, retainedRetrieval, func(ctx context.Context, x *call, authority access.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
		return s.batchRefresh(ctx, x, res, p, http.MethodGet, managedBatchPath(res, false), nil, w)
	})
}

func (s *Server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	s.batchCall(w, r, retainedHousekeeping, func(ctx context.Context, x *call, authority access.Authority, res *resources.Resource, p *pin, route *routing.Route) *Error {
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
		if _, _, err = s.Resources.UpdateDurableContract(commitCtx, res.Kind, res.APIKeyID, res.ID, state, payload); err != nil {
			return durableError(err)
		}
	} else if adapter == "gemini_managed" {
		upstreamID, identityErr := geminiBatchName(result)
		if identityErr != nil || upstreamID != res.UpstreamID {
			return serverError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned a mismatched Gemini batch identity.")
		}
		x.completePinned()
		if err := s.Resources.Update(commitCtx, res.ID, state, metadata, nil); err != nil {
			return serverError(http.StatusServiceUnavailable, "provider_resource_unavailable", "The refreshed batch state could not be committed.")
		}
		res.State, res.Metadata = state, metadata
	} else {
		if !retainedIdentityMatches(result, res.UpstreamID, s.cfg.MaxResponseBytes) {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed batch object.")
		}
		x.completePinned()
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
