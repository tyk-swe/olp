package billing

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
)

type azureScope struct {
	BillingScope    string   `json:"billing_scope"`
	ExportName      string   `json:"export_name"`
	StorageAccount  string   `json:"storage_account"`
	Container       string   `json:"container"`
	Prefix          string   `json:"prefix,omitempty"`
	SubscriptionIDs []string `json:"subscription_ids,omitempty"`
}

type azureCredential struct {
	AuthMode     string `json:"auth_mode"`
	TenantID     string `json:"tenant_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
}

var azureStorageAccount = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
var azureContainer = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,61}[a-z0-9])$`)
var azureResourceScope = regexp.MustCompile(`(?i)^/(?:providers/Microsoft\.Billing/billingAccounts/[^/]+(?:/billingProfiles/[^/]+(?:/invoiceSections/[^/]+)?|/departments/[^/]+|/enrollmentAccounts/[^/]+)?|providers/Microsoft\.Management/managementGroups/[^/]+|subscriptions/[0-9a-f-]{36}(?:/resourceGroups/[^/]+)?)$`)

func validAzurePath(value string, allowSlash bool) bool {
	if len(value) > 1024 || strings.ContainsAny(value, "\\?#\x00\r\n") || (!allowSlash && strings.Contains(value, "/")) {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validateAzureScope(raw json.RawMessage) (json.RawMessage, error) {
	scope, err := decodeAzureDocument[azureScope](raw)
	if err != nil || !azureResourceScope.MatchString(scope.BillingScope) ||
		!validAzurePath(scope.BillingScope, true) || scope.ExportName == "" || !validAzurePath(scope.ExportName, false) ||
		!azureStorageAccount.MatchString(scope.StorageAccount) || !azureContainer.MatchString(scope.Container) ||
		strings.Contains(scope.Container, "--") || !validAzurePath(scope.Prefix, true) || len(scope.SubscriptionIDs) > 100 {
		return nil, access.Invalid("scope", "Provide a billing scope, export name, storage account, container, and optional prefix and subscription IDs.")
	}
	if strings.HasPrefix(strings.ToLower(scope.BillingScope), "/subscriptions/") {
		subscription := strings.Split(scope.BillingScope, "/")[2]
		if parsed, err := uuid.Parse(subscription); err != nil || parsed.String() != strings.ToLower(subscription) {
			return nil, access.Invalid("scope.billing_scope", "The subscription billing scope must contain a valid subscription ID.")
		}
	}
	ids := make([]string, 0, len(scope.SubscriptionIDs))
	for _, id := range scope.SubscriptionIDs {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != strings.ToLower(id) {
			return nil, access.Invalid("scope.subscription_ids", "Subscription IDs must be UUIDs.")
		}
		ids = append(ids, parsed.String())
	}
	scope.SubscriptionIDs = slices.Compact(slices.Sorted(slices.Values(ids)))
	scope.Prefix = strings.Trim(scope.Prefix, "/")
	return json.Marshal(scope)
}

func validateAzureCredential(raw []byte) ([]byte, error) {
	credential, err := decodeAzureDocument[azureCredential](raw)
	if err != nil {
		return nil, access.Invalid("credential", "Provide an Azure credential document.")
	}
	switch credential.AuthMode {
	case "azure_default":
		if credential.TenantID != "" || credential.ClientID != "" || credential.ClientSecret != "" {
			return nil, access.Invalid("credential", "azure_default does not accept client credentials.")
		}
	case "azure_client_secret":
		if !azureCredentialPart(credential.TenantID, 128) || !azureCredentialPart(credential.ClientID, 128) || !azureCredentialPart(credential.ClientSecret, 1024) {
			return nil, access.Invalid("credential", "azure_client_secret requires tenant_id, client_id, and client_secret.")
		}
	default:
		return nil, access.Invalid("credential", "Choose azure_default or azure_client_secret.")
	}
	return json.Marshal(credential)
}

func azureCredentialPart(value string, max int) bool {
	return len(value) > 0 && len(value) <= max && !strings.ContainsAny(value, "\x00\r\n")
}

func decodeAzureDocument[T any](raw []byte) (T, error) {
	var value T
	request := &http.Request{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}
	err := access.DecodeUnique(request, &value, 16384)
	return value, err
}
