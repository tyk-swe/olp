package source

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"regexp"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/management/contract"
)

const googleMaxCredential = 65536

var (
	googleBillingAccount = regexp.MustCompile(`^[A-Z0-9]{6}-[A-Z0-9]{6}-[A-Z0-9]{6}$`)
	googleProjectID      = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)+:)?[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	googleDatasetID      = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	googleTableID        = regexp.MustCompile(`^[\p{L}\p{M}\p{N}\p{Pc}\p{Pd}\p{Zs}]+$`)
)

type googleScope struct {
	BillingAccountID  string   `json:"billing_account_id"`
	BigQueryProjectID string   `json:"bigquery_project_id"`
	DatasetID         string   `json:"dataset_id"`
	TableID           string   `json:"table_id"`
	ProjectIDs        []string `json:"project_ids,omitempty"`
}

func parseGoogleScope(raw json.RawMessage) (googleScope, error) {
	var scope googleScope
	if err := decodeStrict(raw, &scope); errors.Is(err, errTrailingData) {
		return googleScope{}, access.Invalid("scope", "scope must be a single JSON document")
	} else if err != nil {
		return googleScope{}, access.Invalid("scope", "Google scope must be an object containing only supported fields")
	}
	if !googleBillingAccount.MatchString(scope.BillingAccountID) || !googleProjectID.MatchString(scope.BigQueryProjectID) ||
		len(scope.DatasetID) > 1024 || len(scope.TableID) > 1024 || !googleDatasetID.MatchString(scope.DatasetID) || !googleTableID.MatchString(scope.TableID) {
		return googleScope{}, access.Invalid("scope", "Google billing account, BigQuery project, dataset and table must be valid identifiers")
	}
	if len(scope.ProjectIDs) > 100 {
		return googleScope{}, access.Invalid("scope.project_ids", "at most 100 project IDs")
	}
	if slices.ContainsFunc(scope.ProjectIDs, func(id string) bool { return !googleProjectID.MatchString(id) }) {
		return googleScope{}, access.Invalid("scope.project_ids", "project IDs must be valid Google project identifiers")
	}
	return googleScope{
		BillingAccountID:  scope.BillingAccountID,
		BigQueryProjectID: scope.BigQueryProjectID,
		DatasetID:         scope.DatasetID,
		TableID:           scope.TableID,
		ProjectIDs:        slices.Sorted(slices.Values(scope.ProjectIDs)),
	}, nil
}

type googleCredential interface {
	googleAuth() (mode connectors.AuthMode, secret []byte)
}

type googleServiceAccount struct{ key json.RawMessage }

type googleADC struct{}

func (c googleServiceAccount) googleAuth() (connectors.AuthMode, []byte) {
	return connectors.AuthServiceAccount, c.key
}
func (googleADC) googleAuth() (connectors.AuthMode, []byte) { return connectors.AuthADC, nil }

type googleCredentialDocument struct {
	Source         Kind            `json:"source,omitempty"`
	AuthMode       string          `json:"auth_mode"`
	ServiceAccount json.RawMessage `json:"service_account,omitempty"`
}

type googleServiceAccountKey struct {
	Type        string `json:"type"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

func (c googleServiceAccount) MarshalJSON() ([]byte, error) {
	return json.Marshal(googleCredentialDocument{AuthMode: "service_account", ServiceAccount: c.key})
}

func (googleADC) MarshalJSON() ([]byte, error) {
	return json.Marshal(googleCredentialDocument{AuthMode: "adc"})
}

func googleCredentialInput(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	if json.Unmarshal(raw, new(map[string]json.RawMessage)) != nil {
		return "", access.Invalid("credential", "credential must be a string")
	}
	return string(raw), nil
}

func parseGoogleCredential(text string) (googleCredential, error) {
	if len(text) > googleMaxCredential {
		return nil, access.Invalid("credential", "Google billing credential exceeds 65536 bytes")
	}
	var doc googleCredentialDocument
	if err := decodeStrict(json.RawMessage(text), &doc); errors.Is(err, errTrailingData) {
		return nil, access.Invalid("credential", "credential must be a single JSON document")
	} else if err != nil {
		return nil, access.Invalid("credential", "Google billing credential must be a credential document")
	}
	if doc.Source != "" && doc.Source != contract.GoogleBigquery {
		return nil, access.Invalid("credential", "credential source does not match")
	}
	switch doc.AuthMode {
	case "service_account":
		key, ok := canonicalServiceAccount(doc.ServiceAccount)
		if !ok {
			return nil, access.Invalid("credential", "service_account requires a Google service account key document")
		}
		return googleServiceAccount{key: key}, nil
	case "adc":
		if len(doc.ServiceAccount) != 0 {
			return nil, access.Invalid("credential", "adc does not accept a service account key")
		}
		return googleADC{}, nil
	default:
		return nil, access.Invalid("credential", "Google auth_mode must be service_account or adc")
	}
}

func canonicalServiceAccount(raw json.RawMessage) (json.RawMessage, bool) {
	var key googleServiceAccountKey
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &key) != nil || key.Type != "service_account" || !validGooglePrivateKey(key.PrivateKey) ||
		key.ClientEmail == "" || key.TokenURI != "https://oauth2.googleapis.com/token" || json.Unmarshal(raw, &fields) != nil {
		return nil, false
	}
	canonical, err := json.Marshal(fields)
	return canonical, err == nil
}

func validGooglePrivateKey(value string) bool {
	block, rest := pem.Decode([]byte(value))
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return false
	}
	switch block.Type {
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		privateKey, ok := key.(*rsa.PrivateKey)
		return err == nil && ok && privateKey.Validate() == nil
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		return err == nil && key.Validate() == nil
	default:
		return false
	}
}
