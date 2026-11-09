package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
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
	"github.com/tyk-swe/olp/internal/usage"
)

const stateRouteHeader = "X-OLP-Route"

func (s *Server) registerState(mux *http.ServeMux) {
	if s.deps.Resources == nil {
		return
	}
	s.registerRetainedLifecycle(mux)
	s.registerRetainedGeneric(mux)
	s.registerRetrieval(mux)
	s.registerCloudBatchState(mux)
	if s.deps.Media != nil {
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
	if s.deps.Resources.Encrypted() {
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
		provider, ok := x.snapshot().FindProvider(target.ProviderID)
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

type retainedSlotQualifier func(*routing.Provider, *routing.Slot, string) bool

func (s *Server) selectBoundPinSurface(ctx context.Context, x *call, route *routing.Route, operation, surface, mode string, bound *resources.Resource, qualified func(*routing.Provider, string) bool, slotQualifiers ...retainedSlotQualifier) (*pin, *Error) {
	if e := s.checkRouteFidelity(ctx, route); e != nil {
		return nil, e
	}
	if e := x.checkBody(route); e != nil {
		return nil, e
	}
	snapshot := x.request.release.Snapshot()
	// Pinned work must select a target on this route rather than delegate.
	x.fixed = true
	qualifiesSlot := func(provider *routing.Provider, slot *routing.Slot, model string) bool {
		for _, qualify := range slotQualifiers {
			if qualify != nil && !qualify(provider, slot, model) {
				return false
			}
		}
		return true
	}
	options := s.selectionOptions(ctx, x)
	options.Prepare = func(p routing.Provider, t routing.Target) (*routing.CandidatePreparation, error) {
		if bound != nil && (bound.ProviderID != p.ID || bound.ProviderRevisionID != p.RevisionID || bound.TargetID != t.ID) {
			return nil, errors.New("resource affinity unavailable")
		}
		if !qualified(&p, t.ProviderModel) {
			return nil, errors.New("provider capability unavailable")
		}
		if len(slotQualifiers) > 0 {
			attempt := routing.Attempt{ProviderID: p.ID, UpstreamModel: t.ProviderModel}
			for i := range p.Slots {
				slot := &p.Slots[i]
				if bound != nil && (bound.SlotID != slot.ID || !sameCredential(bound.CredentialID, slot.CredentialID)) {
					continue
				}
				if qualifiesSlot(&p, slot, t.ProviderModel) && s.slotAvailable(x, attempt, slot) && !s.cooling(ctx, p.ID, slot) {
					return nil, nil
				}
			}
			return nil, errors.New("credential model authority unavailable")
		}
		return nil, nil
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
	provider, ok := snapshot.FindProvider(attempt.ProviderID)
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
	if mode == "realtime" || mode == "webrtc" {
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
		if !qualifiesSlot(&provider, slot, attempt.UpstreamModel) {
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
	// Skills use the GA contract the pinned SDK parses (id-addressed
	// versions, latest_version_id). The dated beta header would opt
	// into legacy response shapes with incompatible identities, so
	// it is deliberately not sent.
	return s.doPinnedRequest(ctx, x, p, exchange, req, openai.ParseErrorBody)
}

func upstreamError(f *attemptFailure) *Error {
	if f.upstream != nil && f.upstream.Message != "" {
		return &Error{Status: f.status, Type: f.upstream.Type, Code: f.upstream.Code, Message: f.upstream.Message}
	}
	return serverError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
}

func (s *Server) stateBegin(w http.ResponseWriter, r *http.Request, family wire.Format) (*call, keyauthority.Authority, bool) {
	x := &call{request: s.begin(w, r), family: family, actor: "api_key"}
	if !s.admit(r.Context()) {
		s.stateFail(x, w, overloaded, family)
		return x, keyauthority.Authority{}, true
	}
	authority, e := s.authenticate(r, "inference")
	if e != nil {
		s.release(r.Context())
		s.stateFail(x, w, e, family)
		return x, keyauthority.Authority{}, true
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
		return x, keyauthority.Authority{}, true
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

func (s *Server) reserveState(ctx context.Context, x *call, authority keyauthority.Authority, overall time.Duration) *Error {
	if e := x.checkBody(x.route); e != nil {
		return e
	}
	var e *Error
	x.lease, e = s.deps.Admission.reserveKey(ctx, x.admissionAuthority(authority), x.clientSurface(), max(resourceEstimate, x.estimate), overall, x.limitRoute())
	return e
}

func (s *Server) reserveStateAndSlot(ctx context.Context, x *call, authority keyauthority.Authority, p *pin) *Error {
	overall := time.Duration(x.route.OverallTimeout) * time.Millisecond
	if e := s.reserveState(ctx, x, authority, overall); e != nil {
		return e
	}
	gate := s.gateSlot(ctx, &p.provider, &p.slot, resourceEstimate, s.now().Add(overall), "")
	if gate.verdict != gateAdmitted {
		return gateError(gate)
	}
	p.hold = gate.hold
	return nil
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
	adapter, err := resourceAdapter(res)
	if err != nil {
		return nil, err
	}
	if adapter.Managed() {
		record, err := decodeManagedFileRecord(res.Metadata)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			ID     string `json:"id"`
			Object string `json:"object"`
			managedFileObject
		}{ID: res.ID, Object: "file", managedFileObject: record.ProviderFile})
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(res.Metadata, &meta); err != nil {
		return nil, err
	}
	obj := map[string]json.RawMessage{}
	for _, name := range resources.RetainedFileMetadataFields() {
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

func managedGeminiFilePath(res *resources.Resource) (string, error) {
	name, err := geminiManagedFileName(res)
	if err != nil {
		return "", err
	}
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
		for _, name := range batchFileFields() {
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
	adapter, err := resourceAdapter(res)
	if err != nil {
		return nil, err
	}
	if adapter == connectors.BatchGeminiManaged {
		return s.geminiManagedBatchObject(ctx, res)
	}
	record, err := decodeBatchRecord(res)
	if err != nil {
		return nil, err
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(res.Metadata, &stored); err != nil {
		return nil, err
	}
	files, err := s.batchFileProjection(ctx, res, record)
	if err != nil {
		return nil, err
	}
	obj := maps.Clone(stored)
	maps.DeleteFunc(obj, func(name string, _ json.RawMessage) bool { return slices.Contains(privateBatchFields[:], name) })
	maps.Copy(obj, files)
	obj["id"] = mustJSON(res.ID)
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
	if existing, err := s.deps.Resources.GetByUpstream(ctx, identity); err == nil {
		return existing, nil
	} else if !errors.Is(err, resources.ErrNotFound) {
		return nil, err
	}
	metadata, err := resultFileMetadata(owner, role)
	if err != nil {
		return nil, err
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
		source := mustJSON(map[string]string{"batch_id": owner.ID, "upstream_file_id": upstreamID})
		payload := mustJSON(durableDocument{Version: version, Source: source, Binding: resourceModel(owner), Serving: batch.Serving})
		created, err := s.deps.Resources.PutDurableContract(ctx, resource, payload)
		if err == nil {
			return created, nil
		}
		// Another authorized poll may have installed the same output-file
		// mapping concurrently. A collision owned by someone else still fails.
		if existing, readErr := s.deps.Resources.GetByUpstream(ctx, identity); readErr == nil {
			return existing, nil
		}
		return nil, err
	}
	created, err := s.deps.Resources.Put(ctx, resource)
	if err == nil {
		return created, nil
	}
	if existing, readErr := s.deps.Resources.GetByUpstream(ctx, identity); readErr == nil {
		return existing, nil
	}
	return nil, err
}

func fileMetadata(body []byte, model string) (json.RawMessage, string, *time.Time) {
	meta := map[string]json.RawMessage{}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) == nil {
		for _, name := range resources.RetainedFileMetadataFields() {
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

var privateBatchFields = [...]string{"upstream_model", "local_input_file_id", "adapter", "manifest", "provider_file", "role"}

type batchRecord struct {
	LocalInputFileID string          `json:"local_input_file_id,omitempty"`
	CompletionWindow string          `json:"completion_window,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
	InputFileID      *string         `json:"input_file_id"`
	OutputFileID     *string         `json:"output_file_id"`
	ErrorFileID      *string         `json:"error_file_id"`
}

func decodeBatchRecord(res *resources.Resource) (batchRecord, error) {
	var record batchRecord
	err := json.Unmarshal(res.Metadata, &record)
	return record, err
}

func (s *Server) batchFileProjection(ctx context.Context, res *resources.Resource, record batchRecord) (map[string]json.RawMessage, error) {
	upstreamFiles := map[string]*string{"output_file_id": record.OutputFileID, "error_file_id": record.ErrorFileID}
	projected := map[string]json.RawMessage{}
	if record.LocalInputFileID != "" {
		projected["input_file_id"] = mustJSON(record.LocalInputFileID)
	} else {
		upstreamFiles["input_file_id"] = record.InputFileID
	}
	for name, upstreamID := range upstreamFiles {
		if upstreamID == nil || *upstreamID == "" {
			continue
		}
		mapped, err := s.mapUpstreamFile(ctx, res, *upstreamID, strings.TrimSuffix(name, "_file_id"))
		if err != nil {
			return nil, err
		}
		projected[name] = mustJSON(mapped.ID)
	}
	return projected, nil
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

func resultFileMetadata(owner *resources.Resource, role string) (json.RawMessage, error) {
	metadata := durableMetadata(resourceModel(owner))
	adapter, err := resourceAdapter(owner)
	if err != nil {
		return nil, err
	}
	if adapter != connectors.BatchTogether && adapter != connectors.BatchMistral {
		return metadata, nil
	}
	return managedFileMetadata(metadata, nil, adapter, batchManifest{}, role)
}

type refreshedFile struct {
	metadata json.RawMessage
	state    string
	expires  *time.Time
}

func refreshedFileMetadata(res *resources.Resource, body []byte) (refreshedFile, error) {
	metadata, state, expires := fileMetadata(body, resourceModel(res))
	if res.Kind == resources.KindStrictFile {
		return refreshedFile{metadata: metadata, state: state, expires: expires}, nil
	}
	adapter, err := resourceAdapter(res)
	if err != nil {
		return refreshedFile{}, err
	}
	refreshed, err := refreshedProviderMetadata(res, body, metadata, adapter)
	if err != nil {
		return refreshedFile{}, err
	}
	return refreshedFile{metadata: refreshed, state: state, expires: expires}, nil
}

func refreshedProviderMetadata(res *resources.Resource, body []byte, metadata json.RawMessage, adapter connectors.BatchAdapter) (json.RawMessage, error) {
	var prior struct {
		Role          string          `json:"role"`
		BatchManifest json.RawMessage `json:"batch_manifest,omitempty"`
		FileOperation json.RawMessage `json:"file_operation,omitempty"`
	}
	if err := json.Unmarshal(res.Metadata, &prior); err != nil {
		return nil, err
	}
	base := metadata
	if adapter.Managed() {
		managed, err := managedFileMetadata(res.Metadata, body, adapter, batchManifest{}, prior.Role)
		if err != nil {
			return nil, err
		}
		base = managed
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		return nil, err
	}
	retained := map[string]json.RawMessage{"batch_manifest": prior.BatchManifest, "file_operation": prior.FileOperation}
	maps.DeleteFunc(retained, func(_ string, value json.RawMessage) bool { return len(value) == 0 })
	maps.Copy(fields, retained)
	return json.Marshal(fields)
}

func (x *call) completeFileControl() {
	if x.retainedAdmission != nil {
		_ = observeRetainedUsage(x, oif.Document{}, false)
	}
	x.completePinned()
}
