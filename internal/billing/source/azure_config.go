package source

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
)

const azureMaxDocument = 16384

var (
	azureStorageAccount = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	azureContainer      = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{1,61}[a-z0-9])$`)
	azureResourceScope  = regexp.MustCompile(`(?i)^/(?:providers/Microsoft\.Billing/billingAccounts/[^/]+(?:/billingProfiles/[^/]+(?:/invoiceSections/[^/]+)?|/departments/[^/]+|/enrollmentAccounts/[^/]+)?|providers/Microsoft\.Management/managementGroups/[^/]+|subscriptions/[0-9a-f-]{36}(?:/resourceGroups/[^/]+)?)$`)
)

type azureScope struct {
	BillingScope    string   `json:"billing_scope"`
	ExportName      string   `json:"export_name"`
	StorageAccount  string   `json:"storage_account"`
	Container       string   `json:"container"`
	Prefix          string   `json:"prefix,omitempty"`
	SubscriptionIDs []string `json:"subscription_ids,omitempty"`
}

func validAzurePath(value string, allowSlash bool) bool {
	return len(value) <= 1024 && !strings.ContainsAny(value, "\\?#\x00\r\n") && (allowSlash || !strings.Contains(value, "/")) &&
		!slices.ContainsFunc(strings.Split(value, "/"), func(part string) bool { return part == "." || part == ".." })
}

func canonicalUUID(value string) (string, bool) {
	parsed, err := uuid.Parse(value)
	return parsed.String(), err == nil && parsed.String() == strings.ToLower(value)
}

func parseAzureScope(raw json.RawMessage) (azureScope, error) {
	var scope azureScope
	if len(raw) > azureMaxDocument || decodeStrict(raw, &scope) != nil || !azureResourceScope.MatchString(scope.BillingScope) ||
		!validAzurePath(scope.BillingScope, true) || scope.ExportName == "" || !validAzurePath(scope.ExportName, false) ||
		!azureStorageAccount.MatchString(scope.StorageAccount) || !azureContainer.MatchString(scope.Container) ||
		strings.Contains(scope.Container, "--") || !validAzurePath(scope.Prefix, true) || len(scope.SubscriptionIDs) > 100 {
		return azureScope{}, access.Invalid("scope", "Provide a billing scope, export name, storage account, container, and optional prefix and subscription IDs.")
	}
	if strings.HasPrefix(strings.ToLower(scope.BillingScope), "/subscriptions/") {
		if _, ok := canonicalUUID(strings.Split(scope.BillingScope, "/")[2]); !ok {
			return azureScope{}, access.Invalid("scope.billing_scope", "The subscription billing scope must contain a valid subscription ID.")
		}
	}
	if slices.ContainsFunc(scope.SubscriptionIDs, func(id string) bool { _, ok := canonicalUUID(id); return !ok }) {
		return azureScope{}, access.Invalid("scope.subscription_ids", "Subscription IDs must be UUIDs.")
	}
	return azureScope{
		BillingScope:   scope.BillingScope,
		ExportName:     scope.ExportName,
		StorageAccount: scope.StorageAccount,
		Container:      scope.Container,
		Prefix:         strings.Trim(scope.Prefix, "/"),
		SubscriptionIDs: slices.Compact(slices.Sorted(func(yield func(string) bool) {
			for _, id := range scope.SubscriptionIDs {
				if !yield(strings.ToLower(id)) {
					return
				}
			}
		})),
	}, nil
}

type azureCredential interface {
	azureAuth() (mode connectors.AuthMode, secret azureSecret)
}

type azureDefault struct{}

type azureClientSecret struct {
	TenantID     string
	ClientID     string
	ClientSecret string
}

type azureSecret struct {
	TenantID     string `json:"tenant_id"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func (azureDefault) azureAuth() (connectors.AuthMode, azureSecret) {
	return connectors.AuthAzureDefault, azureSecret{}
}

func (c azureClientSecret) azureAuth() (connectors.AuthMode, azureSecret) {
	return connectors.AuthAzureClientSecret, azureSecret(c)
}

type azureCredentialDocument struct {
	AuthMode     string `json:"auth_mode"`
	TenantID     string `json:"tenant_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
}

func (c azureClientSecret) MarshalJSON() ([]byte, error) { return azureDocument(c) }

func (c azureDefault) MarshalJSON() ([]byte, error) { return azureDocument(c) }

func azureDocument(c azureCredential) ([]byte, error) {
	mode, secret := c.azureAuth()
	return json.Marshal(azureCredentialDocument{AuthMode: string(mode), TenantID: secret.TenantID, ClientID: secret.ClientID, ClientSecret: secret.ClientSecret})
}

func azureCredentialPart(value string, limit int) bool {
	return len(value) > 0 && len(value) <= limit && !strings.ContainsAny(value, "\x00\r\n")
}

func parseAzureCredential(text string) (azureCredential, error) {
	var doc azureCredentialDocument
	if len(text) > azureMaxDocument || decodeStrict(json.RawMessage(text), &doc) != nil {
		return nil, access.Invalid("credential", "Provide an Azure credential document.")
	}
	switch doc.AuthMode {
	case "azure_default":
		if doc.TenantID != "" || doc.ClientID != "" || doc.ClientSecret != "" {
			return nil, access.Invalid("credential", "azure_default does not accept client credentials.")
		}
		return azureDefault{}, nil
	case "azure_client_secret":
		if !azureCredentialPart(doc.TenantID, 128) || !azureCredentialPart(doc.ClientID, 128) || !azureCredentialPart(doc.ClientSecret, 1024) {
			return nil, access.Invalid("credential", "azure_client_secret requires tenant_id, client_id, and client_secret.")
		}
		return azureClientSecret{TenantID: doc.TenantID, ClientID: doc.ClientID, ClientSecret: doc.ClientSecret}, nil
	default:
		return nil, access.Invalid("credential", "Choose azure_default or azure_client_secret.")
	}
}
