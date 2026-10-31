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

func encodeCredential(source, key string) ([]byte, error) {
	switch source {
	case SourceOpenAI, SourceAnthropic:
		if err := ValidCredential(key); err != nil {
			return nil, err
		}
	case SourceAzureCostManagement:
		credential, err := validateAzureCredential([]byte(key))
		if err != nil {
			return nil, err
		}
		return json.MarshalIndent(credentialDocument{Source: source, Credential: credential}, "", "  ")
	default:
		return nil, access.Invalid("credential", "unknown billing source")
	}
	return json.MarshalIndent(credentialDocument{Source: source, APIKey: key}, "", "  ")
}

var errIncompatibleCredential = errors.New("billing credential source does not match")

func pollingSource(source string) bool {
	return source == SourceOpenAI || source == SourceAnthropic
}

func decodeCredential(source string, data []byte) ([]byte, error) {
	if !bytes.ContainsRune(data, '\n') {
		if !pollingSource(source) {
			return nil, errIncompatibleCredential
		}
		return data, nil
	}
	var document credentialDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, errors.New("invalid billing credential document")
	}
	if document.Source != source && !(pollingSource(document.Source) && pollingSource(source)) {
		return nil, errIncompatibleCredential
	}
	if source == SourceAzureCostManagement {
		return validateAzureCredential(document.Credential)
	}
	if ValidCredential(document.APIKey) != nil {
		return nil, errors.New("invalid billing credential document")
	}
	return []byte(document.APIKey), nil
}

func ValidateSourceCredential(source, credential string) error {
	encoded, err := encodeCredential(source, credential)
	clear(encoded)
	return err
}
