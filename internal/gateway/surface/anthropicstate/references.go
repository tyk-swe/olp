package anthropicstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/tyk-swe/olp/internal/resources"
)

func decodeObject(body []byte) (map[string]any, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid Anthropic request")
	}
	return value, nil
}

// Only protocol-defined content blocks own file sources. Tool inputs are
// application JSON and must never be traversed as Anthropic resource metadata.
func visitFileSources(value map[string]any, visit func(map[string]any) error) error {
	var content func(any) error
	content = func(raw any) error {
		blocks, _ := raw.([]any)
		for _, rawBlock := range blocks {
			block, _ := rawBlock.(map[string]any)
			switch block["type"] {
			case "container_upload":
				return fmt.Errorf("container-upload file references are unsupported in batch requests")
			case "image", "document":
				source, _ := block["source"].(map[string]any)
				if source["type"] == "file" {
					id, ok := source["file_id"].(string)
					if !ok || id == "" || len(source) != 2 {
						return fmt.Errorf("invalid Anthropic file source")
					}
					if err := visit(source); err != nil {
						return err
					}
				}
			case "tool_result":
				if err := content(block["content"]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := content(value["system"]); err != nil {
		return err
	}
	messages, _ := value["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if err := content(message["content"]); err != nil {
			return err
		}
	}
	return nil
}

func fileReferences(body []byte) ([]string, error) {
	value, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	refs := []string{}
	err = visitFileSources(value, func(source map[string]any) error {
		refs = append(refs, source["file_id"].(string))
		return nil
	})
	return refs, err
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; !exists {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return out
}

func sameAffinity(resources []*resources.Resource) bool {
	if len(resources) < 2 {
		return true
	}
	first := resources[0]
	for _, file := range resources[1:] {
		if file.ProviderID != first.ProviderID || file.ProviderRevisionID != first.ProviderRevisionID || file.TargetID != first.TargetID || file.SlotID != first.SlotID ||
			!equalCredentialID(file.CredentialID, first.CredentialID) {
			return false
		}
	}
	return true
}

func equalCredentialID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func replaceFileReferences(body []byte, upstream map[string]string) ([]byte, error) {
	value, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	replace := func(source map[string]any) error {
		id := source["file_id"].(string)
		bound, ok := upstream[id]
		if !ok {
			return fmt.Errorf("unknown file")
		}
		source["file_id"] = bound
		return nil
	}
	if requests, ok := value["requests"].([]any); ok {
		for _, raw := range requests {
			request, _ := raw.(map[string]any)
			params, _ := request["params"].(map[string]any)
			if err := visitFileSources(params, replace); err != nil {
				return nil, err
			}
		}
	} else if err := visitFileSources(value, replace); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
