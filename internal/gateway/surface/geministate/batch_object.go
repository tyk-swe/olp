package geministate

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
)

func cancelAcknowledged(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(body, &value) == nil && value != nil && len(value) == 0
}

type batchObject struct {
	outer, metadata map[string]json.RawMessage
	outputs         []string
}

func prepareBatch(body []byte, batch upstream.GeminiBatch, names inputNames) (batchObject, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return batchObject{}, kernel.ErrGeminiBatchFidelity
	}
	var metadata map[string]json.RawMessage
	if raw, present := object["metadata"]; present {
		if json.Unmarshal(raw, &metadata) != nil || metadata == nil {
			return batchObject{}, kernel.ErrGeminiBatchFidelity
		}
		if rawName, present := metadata["name"]; present {
			var name string
			if json.Unmarshal(rawName, &name) != nil || name != "batches/"+batch.ID {
				return batchObject{}, kernel.ErrGeminiBatchFidelity
			}
		}
	}
	var outputs []string
	for _, layer := range []map[string]json.RawMessage{object, metadata} {
		if err := projectInput(layer, names); err != nil {
			return batchObject{}, err
		}
		files, err := outputNames(layer)
		if err != nil {
			return batchObject{}, err
		}
		outputs = append(outputs, files...)
	}
	delete(object, "upstream_model")
	delete(object, "local_input_file_name")
	delete(object, "upstream_input_file_name")
	return batchObject{outer: object, metadata: metadata, outputs: slices.Compact(outputs)}, nil
}

func projectInput(object map[string]json.RawMessage, names inputNames) error {
	for _, field := range []string{"inputConfig", "input_config"} {
		raw, present := object[field]
		if !present {
			continue
		}
		var config map[string]json.RawMessage
		if json.Unmarshal(raw, &config) != nil || config == nil {
			return kernel.ErrGeminiBatchFidelity
		}
		for _, nameField := range []string{"fileName", "file_name"} {
			if rawName, present := config[nameField]; present {
				var name string
				if json.Unmarshal(rawName, &name) != nil || names.Local == "" || name != names.Upstream {
					return kernel.ErrGeminiBatchFidelity
				}
				config[nameField] = kernel.MustJSON(names.Local)
			}
		}
		object[field] = kernel.MustJSON(config)
	}
	return nil
}

func outputNames(object map[string]json.RawMessage) ([]string, error) {
	var files []string
	for _, field := range upstream.GeminiOutputFields() {
		raw, present := object[field]
		if !present {
			continue
		}
		var destination map[string]json.RawMessage
		if json.Unmarshal(raw, &destination) != nil || destination == nil {
			return nil, kernel.ErrGeminiBatchFidelity
		}
		if rawName, present := destination["responsesFile"]; present {
			var name string
			if json.Unmarshal(rawName, &name) != nil {
				return nil, kernel.ErrGeminiBatchFidelity
			}
			if _, err := kernel.GeminiFileID(name); err != nil {
				return nil, kernel.ErrGeminiBatchFidelity
			}
			files = append(files, name)
		}
	}
	return files, nil
}

func (b batchObject) project(localID, routeSlug string, outputFiles map[string]string) ([]byte, error) {
	localName := kernel.MustJSON("batches/" + localID)
	for _, layer := range []map[string]json.RawMessage{b.outer, b.metadata} {
		if _, present := layer["name"]; present {
			layer["name"] = localName
		}
		if _, present := layer["model"]; present {
			layer["model"] = kernel.MustJSON("models/" + routeSlug)
		}
		for _, field := range upstream.GeminiOutputFields() {
			if raw, present := layer[field]; present {
				var destination map[string]json.RawMessage
				if err := json.Unmarshal(raw, &destination); err != nil {
					return nil, err
				}
				if file, present := destination["responsesFile"]; present {
					var upstreamName string
					if json.Unmarshal(file, &upstreamName) != nil || outputFiles[upstreamName] == "" {
						return nil, kernel.ErrGeminiBatchFidelity
					}
					destination["responsesFile"] = kernel.MustJSON(outputFiles[upstreamName])
				}
				layer[field] = kernel.MustJSON(destination)
			}
		}
	}
	if b.metadata != nil {
		b.outer["metadata"] = kernel.MustJSON(b.metadata)
	}
	return json.Marshal(b.outer)
}

func settleResult(exchange *kernel.PinnedExchange, body []byte, batch upstream.GeminiBatch, model string, names inputNames) (batchObject, json.RawMessage, error) {
	object, metadata, err := acceptResult(body, batch, model, names)
	if err != nil {
		return batchObject{}, nil, err
	}
	exchange.Complete(kernel.ClassSuccess)
	return object, metadata, nil
}

func acceptResult(body []byte, batch upstream.GeminiBatch, model string, names inputNames) (batchObject, json.RawMessage, error) {
	object, err := prepareBatch(body, batch, names)
	if err != nil {
		return batchObject{}, nil, err
	}
	metadata, err := upstream.GeminiMetadata(body, model, names.Local, names.Upstream)
	if err != nil {
		return batchObject{}, nil, err
	}
	return object, metadata, nil
}

type inputNames struct {
	Local    string `json:"local_input_file_name"`
	Upstream string `json:"upstream_input_file_name"`
}

func readInputNames(resource *resources.Resource) (inputNames, error) {
	var names inputNames
	err := json.Unmarshal(resource.Metadata, &names)
	return names, err
}

func projectionError(err error) *fault.Error {
	if errors.Is(err, kernel.ErrGeminiBatchFidelity) {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an unprojectable Gemini batch object.")
	}
	return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The Gemini batch object could not be projected.")
}
