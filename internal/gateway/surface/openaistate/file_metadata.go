package openaistate

import (
	"encoding/json"
	"maps"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/resources"
)

func retainedFileMetadata(body []byte, model string) (json.RawMessage, string, *time.Time) {
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
	encoded, _ := json.Marshal(meta)
	return encoded, retainedFileState(meta), retainedFileExpiry(meta)
}

func retainedFileState(meta map[string]json.RawMessage) string {
	var status string
	if raw, ok := meta["status"]; ok && json.Unmarshal(raw, &status) == nil && status != "" {
		return status
	}
	return "uploaded"
}

func retainedFileExpiry(meta map[string]json.RawMessage) *time.Time {
	var seconds int64
	raw, ok := meta["expires_at"]
	if !ok || json.Unmarshal(raw, &seconds) != nil || seconds <= 0 {
		return nil
	}
	return new(time.Unix(seconds, 0).UTC())
}

type refreshedFile struct {
	Metadata json.RawMessage
	State    string
	Expires  *time.Time
}

func refreshedFileMetadata(res *resources.Resource, body []byte, adapter connectors.BatchAdapter) (refreshedFile, error) {
	metadata, state, expires := retainedFileMetadata(body, kernel.ResourceModel(res))
	if res.Kind == resources.KindStrictFile {
		return refreshedFile{Metadata: metadata, State: state, Expires: expires}, nil
	}
	refreshed, err := refreshedProviderMetadata(res, body, metadata, adapter)
	if err != nil {
		return refreshedFile{}, err
	}
	return refreshedFile{Metadata: refreshed, State: state, Expires: expires}, nil
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
		managed, err := managedbatch.FileMetadata(res.Metadata, body, adapter, managedbatch.Manifest{}, prior.Role)
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
