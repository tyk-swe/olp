package signing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var ErrUnsupportedVersion = errors.New("format version is not supported")

func DecodeDocument[T any](document []byte, name, apiVersion string, maxBytes int) (T, error) {
	var zero T
	if len(document) > maxBytes {
		return zero, fmt.Errorf("%s exceeds its size limit", name)
	}
	var head struct {
		APIVersion string `json:"api_version"`
	}
	if err := json.Unmarshal(document, &head); err != nil {
		return zero, fmt.Errorf("%s is not a JSON object: %w", name, err)
	}
	if head.APIVersion != apiVersion {
		return zero, fmt.Errorf("%s %w: %q", name, ErrUnsupportedVersion, head.APIVersion)
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var decoded T
	if err := decoder.Decode(&decoded); err != nil {
		return zero, fmt.Errorf("%s is malformed: %w", name, err)
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return zero, fmt.Errorf("%s has data after its document", name)
	}
	return decoded, nil
}

func EncodeDocument[T any](v T) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
