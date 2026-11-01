package billing

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"

	"github.com/tyk-swe/olp/internal/access"
)

type googleCredential struct {
	Source         string          `json:"source,omitempty"`
	AuthMode       string          `json:"auth_mode"`
	ServiceAccount json.RawMessage `json:"service_account,omitempty"`
}

type googleServiceAccount struct {
	Type        string `json:"type"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

func encodeGoogleCredential(raw string) ([]byte, error) {
	if len(raw) > 65536 {
		return nil, access.Invalid("credential", "Google billing credential exceeds 65536 bytes")
	}
	var credential googleCredential
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credential); err != nil {
		return nil, access.Invalid("credential", "Google billing credential must be a credential document")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, access.Invalid("credential", "credential must be a single JSON document")
	}
	if credential.Source != "" && credential.Source != SourceGoogle {
		return nil, access.Invalid("credential", "credential source does not match")
	}
	switch credential.AuthMode {
	case "service_account":
		var account googleServiceAccount
		if json.Unmarshal(credential.ServiceAccount, &account) != nil || account.Type != "service_account" ||
			!validGooglePrivateKey(account.PrivateKey) || account.ClientEmail == "" || account.TokenURI != "https://oauth2.googleapis.com/token" {
			return nil, access.Invalid("credential", "service_account requires a Google service account key document")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(credential.ServiceAccount, &fields); err != nil {
			return nil, access.Invalid("credential", "service_account requires a Google service account key document")
		}
		canonical, err := json.Marshal(fields)
		if err != nil {
			return nil, access.Invalid("credential", "service_account requires a Google service account key document")
		}
		return json.Marshal(googleCredential{Source: SourceGoogle, AuthMode: credential.AuthMode, ServiceAccount: canonical})
	case "adc":
		if len(credential.ServiceAccount) != 0 {
			return nil, access.Invalid("credential", "adc does not accept a service account key")
		}
	default:
		return nil, access.Invalid("credential", "Google auth_mode must be service_account or adc")
	}
	return json.Marshal(googleCredential{Source: SourceGoogle, AuthMode: credential.AuthMode})
}

func validGooglePrivateKey(value string) bool {
	block, rest := pem.Decode([]byte(value))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return false
	}
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return false
		}
		privateKey, ok := key.(*rsa.PrivateKey)
		return ok && privateKey.Validate() == nil
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		return err == nil && key.Validate() == nil
	default:
		return false
	}
}

func decodeGoogleCredential(data []byte) ([]byte, error) {
	var credential googleCredential
	if err := json.Unmarshal(data, &credential); err != nil {
		return nil, errors.New("invalid billing credential document")
	}
	if credential.Source != SourceGoogle {
		return nil, errIncompatibleCredential
	}
	return json.Marshal(googleCredential{AuthMode: credential.AuthMode, ServiceAccount: credential.ServiceAccount})
}
