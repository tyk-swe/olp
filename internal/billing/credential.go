package billing

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/access"
)

type credentialDocument struct {
	Source     string          `json:"source"`
	APIKey     string          `json:"api_key,omitempty"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

var errIncompatibleCredential = errors.New("billing credential source does not match")

func pollingSource(source string) bool {
	return source == SourceOpenAI || source == SourceAnthropic
}

func ValidCredential(credential string) error {
	if n := len(credential); n < 20 || n > 512 {
		return access.Invalid("credential", "credential must be 20 to 512 bytes")
	}
	for _, r := range credential {
		if r < 0x21 || r > 0x7e {
			return access.Invalid("credential", "credential must be printable ASCII without whitespace")
		}
	}
	return nil
}

func ValidateCredential(source, credential string) error {
	document, err := encodeCredential(source, credential)
	clear(document)
	return err
}

func encodeAPIKeyCredential(source, key string) ([]byte, error) {
	if err := ValidCredential(key); err != nil {
		return nil, err
	}
	return json.MarshalIndent(credentialDocument{Source: source, APIKey: key}, "", "  ")
}

func parseCredentialDocument(data []byte) (credentialDocument, error) {
	var document credentialDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return credentialDocument{}, errors.New("invalid billing credential document")
	}
	return document, nil
}

func decodePollingCredential(data []byte) ([]byte, error) {
	if !bytes.ContainsRune(data, '\n') {
		return data, nil
	}
	document, err := parseCredentialDocument(data)
	if err != nil {
		return nil, err
	}
	if !pollingSource(document.Source) {
		return nil, errIncompatibleCredential
	}
	if ValidCredential(document.APIKey) != nil {
		return nil, errors.New("invalid billing credential document")
	}
	return []byte(document.APIKey), nil
}

func rawCredentialInput(raw json.RawMessage) (string, error) {
	return string(raw), nil
}

func stringCredentialInput(raw json.RawMessage) (string, error) {
	var secret string
	if err := json.Unmarshal(raw, &secret); err != nil {
		return "", access.Invalid("credential", "credential must be a string")
	}
	return secret, nil
}

func googleCredentialInput(raw json.RawMessage) (string, error) {
	if raw[0] == '{' {
		return string(raw), nil
	}
	return stringCredentialInput(raw)
}
