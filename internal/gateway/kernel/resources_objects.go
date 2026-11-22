package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

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
func NativeResourceID(body []byte, maxBytes int64) (string, bool) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: int(maxBytes)})
	if err != nil {
		return "", false
	}
	id, present := source.Root().Lookup("id")
	upstream, valid := id.Text()
	return upstream, present && valid && upstream != "" && len(upstream) <= 512
}

func RetainedIdentityMatches(body []byte, expected string, maxBytes int64) bool {
	upstream, valid := NativeResourceID(body, maxBytes)
	return valid && upstream == expected
}

func RewriteID(body []byte, name, value string) ([]byte, error) {
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

func (s *Server) FileObject(ctx context.Context, res *resources.Resource) ([]byte, error) {
	if res.Kind == resources.KindStrictFile {
		_, contract, err := s.ReadDurable(ctx, res.Kind, res.APIKeyID, res.ID)
		if err != nil {
			return nil, err
		}
		if len(contract.Result) != 0 {
			return DurableProjection(contract.Result, res.ID, nil)
		}
		return json.Marshal(map[string]any{"id": res.ID, "object": "file", "status": res.State})
	}
	adapter, err := ResourceAdapter(res)
	if err != nil {
		return nil, err
	}
	if adapter.Managed() {
		record, err := DecodeManagedFileRecord(res.Metadata)
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

func ManagedGeminiFilePath(res *resources.Resource) (string, error) {
	name, err := GeminiManagedFileName(res)
	if err != nil {
		return "", err
	}
	id, err := GeminiFileID(name)
	if err != nil {
		return "", err
	}
	return "files/" + url.PathEscape(id), nil
}

func (s *Server) BatchObject(ctx context.Context, res *resources.Resource) ([]byte, error) {
	if res.Kind == resources.KindStrictBatch {
		_, contract, err := s.ReadDurable(ctx, res.Kind, res.APIKeyID, res.ID)
		if err != nil || len(contract.Result) == 0 {
			return nil, resources.ErrContract
		}
		doc, _, err := StrictBatchResult(contract.Result, res.UpstreamID)
		if err != nil {
			return nil, err
		}
		if !StrictBatchInputMatches(doc, contract.Effective) {
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
			if name == "input_file_id" {
				var source struct {
					InputFileID string `json:"input_file_id"`
				}
				if err := json.Unmarshal(contract.Source, &source); err != nil || source.InputFileID == "" {
					return nil, resources.ErrContract
				}
				files[name] = source.InputFileID
				continue
			}
			mapped, err := s.mapUpstreamFile(ctx, res, upstream, strings.TrimSuffix(name, "_file_id"))
			if err != nil {
				return nil, err
			}
			files[name] = mapped.ID
		}
		return DurableProjection(contract.Result, res.ID, files)
	}
	adapter, err := ResourceAdapter(res)
	if err != nil {
		return nil, err
	}
	if adapter == connectors.BatchGeminiManaged {
		return s.geminiManagedBatchObject(ctx, res)
	}
	record, err := DecodeBatchRecord(res)
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
	obj["id"] = MustJSON(res.ID)
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
	if existing, err := s.Deps.Resources.GetBatchFileByUpstream(ctx, identity, owner.UUID); err == nil {
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
		_, batch, err := s.ReadDurable(ctx, owner.Kind, owner.APIKeyID, owner.ID)
		if err != nil {
			return nil, err
		}
		version := resources.DurableContractVersion
		resource.ContractVersion = &version
		resource.ExpiresAt = owner.ExpiresAt
		source := MustJSON(map[string]string{"batch_id": owner.ID, "upstream_file_id": upstreamID})
		payload := MustJSON(DurableDocument{Version: version, Source: source, Binding: ResourceModel(owner), Serving: batch.Serving})
		created, err := s.Deps.Resources.PutDurableContract(ctx, resource, payload)
		if err == nil {
			return created, nil
		}
		// Another authorized poll may have installed the same output-file
		// mapping concurrently. A collision owned by someone else still fails.
		if existing, readErr := s.Deps.Resources.GetBatchFileByUpstream(ctx, identity, owner.UUID); readErr == nil {
			return existing, nil
		}
		return nil, err
	}
	created, err := s.Deps.Resources.Put(ctx, resource)
	if err == nil {
		return created, nil
	}
	if existing, readErr := s.Deps.Resources.GetBatchFileByUpstream(ctx, identity, owner.UUID); readErr == nil {
		return existing, nil
	}
	return nil, err
}

func FileMetadata(body []byte, model string) (json.RawMessage, string, *time.Time) {
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

type BatchRecord struct {
	LocalInputFileID string          `json:"local_input_file_id,omitempty"`
	CompletionWindow string          `json:"completion_window,omitempty"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
	InputFileID      *string         `json:"input_file_id"`
	OutputFileID     *string         `json:"output_file_id"`
	ErrorFileID      *string         `json:"error_file_id"`
}

func DecodeBatchRecord(res *resources.Resource) (BatchRecord, error) {
	var record BatchRecord
	err := json.Unmarshal(res.Metadata, &record)
	return record, err
}

func (s *Server) batchFileProjection(ctx context.Context, res *resources.Resource, record BatchRecord) (map[string]json.RawMessage, error) {
	upstreamFiles := map[string]*string{"output_file_id": record.OutputFileID, "error_file_id": record.ErrorFileID}
	projected := map[string]json.RawMessage{}
	if record.LocalInputFileID != "" {
		projected["input_file_id"] = MustJSON(record.LocalInputFileID)
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
		projected[name] = MustJSON(mapped.ID)
	}
	return projected, nil
}

func gateError(gate gateResult) *fault.Error {
	switch gate.verdict {
	case gateRejected:
		return gate.rejection.Fault()
	case gateExpired:
		return (&AttemptFailure{class: ClassTimeout}).Fault()
	}
	return fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "The pinned credential cannot currently serve this object.")
}

func (s *Server) WriteStateJSON(w http.ResponseWriter, x *Call, body []byte, exchanges ...*PinnedExchange) {
	s.WriteStateJSONStatus(w, x, body, http.StatusOK, exchanges...)
}

func (s *Server) WriteStateJSONStatus(w http.ResponseWriter, x *Call, body []byte, status int, exchanges ...*PinnedExchange) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	if body != nil {
		w.Header().Set("Content-Type", "application/json")
	}
	x.ResponseHeaders(w.Header(), false)
	w.WriteHeader(status)
	n, err := w.Write(body)
	if err == nil && n == len(body) {
		err = http.NewResponseController(w).Flush()
	}
	if err != nil || n != len(body) {
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The resource result could not be delivered.")
		for _, exchange := range exchanges {
			exchange.Delivery(DeliveryState(n))
		}
		s.Finish(x, &Outcome{Err: x.Failure, Committed: true, Cancelled: true}, 0)
		return
	}
	if n > 0 {
		x.Delivered(s.Now())
	}
	for _, exchange := range exchanges {
		exchange.Delivery(usage.ClientTerminal)
	}
	s.Finish(x, &Outcome{Committed: true}, status)
}

func ResourceListError(err error, message string) *fault.Error {
	if errors.Is(err, resources.ErrNotFound) {
		return fault.InvalidRequest("invalid_value", "after must be a gateway identifier of the listed resource kind.", new("after"))
	}
	return fault.ServerError(http.StatusInternalServerError, "internal_error", message)
}

func ListArgs(r *http.Request) (int, string, *fault.Error) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxResourceList {
			param := "limit"
			return 0, "", fault.InvalidRequest("invalid_value", "limit must be an integer between 1 and 100.", &param)
		}
		limit = parsed
	}
	after := r.URL.Query().Get("after")
	if after != "" && !strings.Contains(after, "_") {
		param := "after"
		return 0, "", fault.InvalidRequest("invalid_value", "after must be a gateway object identifier.", &param)
	}
	return limit, after, nil
}

// ListBody writes the OpenAI cursor page shape: SDK auto-pagination follows
// has_more and resumes after the last item's id.
func ListBody(items []json.RawMessage, ids []string, hasMore bool) []byte {
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
	metadata := DurableMetadata(ResourceModel(owner))
	adapter, err := ResourceAdapter(owner)
	if err != nil {
		return nil, err
	}
	if adapter != connectors.BatchTogether && adapter != connectors.BatchMistral {
		return metadata, nil
	}
	return ManagedFileMetadata(metadata, nil, adapter, BatchManifest{}, role)
}

type RefreshedFile struct {
	Metadata json.RawMessage
	State    string
	Expires  *time.Time
}

func RefreshedFileMetadata(res *resources.Resource, body []byte, adapter connectors.BatchAdapter) (RefreshedFile, error) {
	metadata, state, expires := FileMetadata(body, ResourceModel(res))
	if res.Kind == resources.KindStrictFile {
		return RefreshedFile{Metadata: metadata, State: state, Expires: expires}, nil
	}
	refreshed, err := refreshedProviderMetadata(res, body, metadata, adapter)
	if err != nil {
		return RefreshedFile{}, err
	}
	return RefreshedFile{Metadata: refreshed, State: state, Expires: expires}, nil
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
		managed, err := ManagedFileMetadata(res.Metadata, body, adapter, BatchManifest{}, prior.Role)
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

func (e *PinnedExchange) CompleteRetainedControl() {
	if e.request.retainedAdmission != nil {
		_ = ObserveRetainedUsage(e, oif.Document{}, false)
	}
	e.Complete(ClassSuccess)
}
