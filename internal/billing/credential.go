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

func ValidateCredential(source, credential string) error {
	document, err := encodeCredential(source, credential)
	clear(document)
	return err
}

func encodeCredential(source, key string) ([]byte, error) {
	switch source {
	case SourceOpenAI, SourceAnthropic:
		if err := ValidCredential(key); err != nil {
			return nil, err
		}
	case SourceGoogle:
		return encodeGoogleCredential(key)
	case SourceAWS:
		return encodeAWSCredential(key)
	default:
		return nil, access.Invalid("credential", "unknown billing source")
	}
	return json.MarshalIndent(credentialDocument{Source: source, APIKey: key}, "", "  ")
}

func ValidateSourceCredential(source, credential string) error {
	document, err := encodeCredential(source, credential)
	clear(document)
	return err
}

var errIncompatibleCredential = errors.New("billing credential source does not match")

func pollingSource(source string) bool {
	return source == SourceOpenAI || source == SourceAnthropic
}

func decodeCredential(source string, data []byte) ([]byte, error) {
	if source == SourceGoogle {
		return decodeGoogleCredential(data)
	}
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
	if source == SourceAWS {
		return decodeAWSCredential(data)
	}
	if ValidCredential(document.APIKey) != nil {
		return nil, errors.New("invalid billing credential document")
	}
	return []byte(document.APIKey), nil
}
