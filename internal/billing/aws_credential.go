package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
)

type awsCredential struct {
	AuthMode        string `json:"auth_mode"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`
}

type awsCredentialDocument struct {
	Source string `json:"source"`
	awsCredential
}

func validateAWSCredential(input string) (awsCredential, error) {
	var credential awsCredential
	if len(input) > 8192 {
		return credential, access.Invalid("credential", "AWS credential exceeds 8192 bytes")
	}
	dec := json.NewDecoder(strings.NewReader(input))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&credential); err != nil {
		return credential, access.Invalid("credential", "AWS credential must be a valid credential document")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return credential, access.Invalid("credential", "AWS credential must be one document")
	}
	switch credential.AuthMode {
	case "static":
		if len(credential.AccessKeyID) < 1 || len(credential.AccessKeyID) > 128 || len(credential.SecretAccessKey) < 1 || len(credential.SecretAccessKey) > 4096 || len(credential.SessionToken) > 4096 || strings.ContainsAny(credential.AccessKeyID+credential.SecretAccessKey+credential.SessionToken, "\r\n\x00") {
			return credential, access.Invalid("credential", "AWS static credentials require an access key ID and secret access key")
		}
	case "workload":
		if credential.AccessKeyID != "" || credential.SecretAccessKey != "" || credential.SessionToken != "" {
			return credential, access.Invalid("credential", "AWS workload credentials cannot contain static keys")
		}
	default:
		return credential, access.Invalid("credential", "AWS auth_mode must be static or workload")
	}
	return credential, nil
}

func encodeAWSCredential(input string) ([]byte, error) {
	credential, err := validateAWSCredential(input)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(awsCredentialDocument{Source: SourceAWS, awsCredential: credential}, "", "  ")
}

func decodeAWSCredential(data []byte) ([]byte, error) {
	var document awsCredentialDocument
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&document); err != nil || document.Source != SourceAWS {
		return nil, errors.New("invalid AWS billing credential")
	}
	encoded, err := json.Marshal(document.awsCredential)
	if err != nil {
		return nil, err
	}
	if _, err := validateAWSCredential(string(encoded)); err != nil {
		return nil, errors.New("invalid AWS billing credential")
	}
	return encoded, nil
}
