package billing

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/access"
)

type credentialDocument struct {
	Source string `json:"source"`
	APIKey string `json:"api_key"`
}

func encodeCredential(source, key string) ([]byte, error) {
	switch source {
	case SourceOpenAI, SourceAnthropic:
		if err := ValidCredential(key); err != nil {
			return nil, err
		}
	default:
		return nil, access.Invalid("credential", "unknown billing source")
	}
	return json.MarshalIndent(credentialDocument{Source: source, APIKey: key}, "", "  ")
}

func decodeCredential(source string, data []byte) ([]byte, error) {
	if !bytes.ContainsRune(data, '\n') {
		return data, nil
	}
	var document credentialDocument
	if err := json.Unmarshal(data, &document); err != nil || document.Source != source || ValidCredential(document.APIKey) != nil {
		return nil, errors.New("invalid billing credential document")
	}
	return []byte(document.APIKey), nil
}
