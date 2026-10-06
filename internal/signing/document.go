package signing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrUnsupportedVersion reports a document in a format this build cannot read.
var ErrUnsupportedVersion = errors.New("format version is not supported")

// DecodeDocument parses a signed JSON document strictly into v: a document
// over maxBytes, in a format other than apiVersion, with unknown members or
// with trailing data is refused. name is the document in errors, such as
// "the catalog". DecodeDocument does not validate.
func DecodeDocument(document []byte, name, apiVersion string, maxBytes int, v any) error {
	if len(document) > maxBytes {
		return fmt.Errorf("%s exceeds its size limit", name)
	}
	var head struct {
		APIVersion string `json:"api_version"`
	}
	if err := json.Unmarshal(document, &head); err != nil {
		return fmt.Errorf("%s is not a JSON object: %w", name, err)
	}
	if head.APIVersion != apiVersion {
		return fmt.Errorf("%s %w: %q", name, ErrUnsupportedVersion, head.APIVersion)
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return fmt.Errorf("%s is malformed: %w", name, err)
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return fmt.Errorf("%s has data after its document", name)
	}
	return nil
}

// EncodeDocument renders a normalized document in the canonical form a
// signature covers: two-space indented JSON with a trailing newline and no
// HTML escaping.
func EncodeDocument(v any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
