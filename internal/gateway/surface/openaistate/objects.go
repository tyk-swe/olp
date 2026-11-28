package openaistate

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/resources"
)

func (s surface) fileObject(ctx context.Context, res *resources.Resource) ([]byte, error) {
	if res.Kind == resources.KindStrictFile {
		_, contract, err := s.kernel.ReadDurable(ctx, res.Kind, res.APIKeyID, res.ID)
		if err != nil {
			return nil, err
		}
		if len(contract.Result) != 0 {
			return durableProjection(contract.Result, res.ID, nil)
		}
		return json.Marshal(map[string]any{"id": res.ID, "object": "file", "status": res.State})
	}
	adapter, err := managedbatch.ResourceAdapter(res)
	if err != nil {
		return nil, err
	}
	if adapter.Managed() {
		record, err := managedbatch.DecodeFileRecord(res.Metadata)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			ID     string `json:"id"`
			Object string `json:"object"`
			managedbatch.FileObject
		}{ID: res.ID, Object: "file", FileObject: record.ProviderFile})
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

func (s surface) batchObject(ctx context.Context, res *resources.Resource) ([]byte, error) {
	if res.Kind == resources.KindStrictBatch {
		_, contract, err := s.kernel.ReadDurable(ctx, res.Kind, res.APIKeyID, res.ID)
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
		return durableProjection(contract.Result, res.ID, files)
	}
	adapter, err := managedbatch.ResourceAdapter(res)
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
	obj["id"] = kernel.MustJSON(res.ID)
	if _, ok := obj["object"]; !ok {
		obj["object"] = json.RawMessage(`"batch"`)
	}
	return json.Marshal(obj)
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

func (s surface) batchFileProjection(ctx context.Context, res *resources.Resource, record batchRecord) (map[string]json.RawMessage, error) {
	upstreamFiles := map[string]*string{"output_file_id": record.OutputFileID, "error_file_id": record.ErrorFileID}
	projected := map[string]json.RawMessage{}
	if record.LocalInputFileID != "" {
		projected["input_file_id"] = kernel.MustJSON(record.LocalInputFileID)
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
		projected[name] = kernel.MustJSON(mapped.ID)
	}
	return projected, nil
}

func resultFileKind(owner *resources.Resource) string {
	if owner.Kind == resources.KindStrictBatch {
		return resources.KindStrictFile
	}
	return resources.KindFile
}

func (s surface) mapUpstreamFile(ctx context.Context, owner *resources.Resource, upstreamID, role string) (*resources.Resource, error) {
	kind := resultFileKind(owner)
	identity := owner.NativeIdentity()
	identity.Kind, identity.UpstreamID = kind, upstreamID
	if existing, err := s.store.GetBatchFileByUpstream(ctx, identity, owner.UUID); err == nil {
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
		_, batch, err := s.kernel.ReadDurable(ctx, owner.Kind, owner.APIKeyID, owner.ID)
		if err != nil {
			return nil, err
		}
		version := resources.DurableContractVersion
		resource.ContractVersion = &version
		resource.ExpiresAt = owner.ExpiresAt
		source := kernel.MustJSON(map[string]string{"batch_id": owner.ID, "upstream_file_id": upstreamID})
		payload := kernel.MustJSON(kernel.DurableDocument{Version: version, Source: source, Binding: kernel.ResourceModel(owner), Serving: batch.Serving})
		created, err := s.store.PutDurableContract(ctx, resource, payload)
		if err == nil {
			return created, nil
		}
		// Another authorized poll may have installed the same output-file
		// mapping concurrently. A collision owned by someone else still fails.
		if existing, readErr := s.store.GetBatchFileByUpstream(ctx, identity, owner.UUID); readErr == nil {
			return existing, nil
		}
		return nil, err
	}
	created, err := s.store.Put(ctx, resource)
	if err == nil {
		return created, nil
	}
	if existing, readErr := s.store.GetBatchFileByUpstream(ctx, identity, owner.UUID); readErr == nil {
		return existing, nil
	}
	return nil, err
}

func resultFileMetadata(owner *resources.Resource, role string) (json.RawMessage, error) {
	metadata := kernel.DurableMetadata(kernel.ResourceModel(owner))
	adapter, err := managedbatch.ResourceAdapter(owner)
	if err != nil {
		return nil, err
	}
	if adapter != connectors.BatchTogether && adapter != connectors.BatchMistral {
		return metadata, nil
	}
	return managedbatch.FileMetadata(metadata, nil, adapter, managedbatch.Manifest{}, role)
}

func retainedIdentityMatches(body []byte, expected string, maxBytes int64) bool {
	upstream, valid := kernel.NativeResourceID(body, maxBytes)
	return valid && upstream == expected
}

func resourceListError(err error, message string) *fault.Error {
	if errors.Is(err, resources.ErrNotFound) {
		return fault.InvalidRequest("invalid_value", "after must be a gateway identifier of the listed resource kind.", new("after"))
	}
	return fault.ServerError(http.StatusInternalServerError, "internal_error", message)
}

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
