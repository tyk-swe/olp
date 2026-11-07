package promotion

import (
	"encoding/json"
	"strings"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/retrieval"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/workload"
)

// PublicationEntry promotes configuration, never runs, approvals, messages or
// billing records. Immutable destination pins are checked, not silently rebased.
type PublicationEntry struct {
	Project                string                         `json:"project"`
	Kind                   string                         `json:"kind"`
	Draft                  agentcontract.Draft            `json:"draft"`
	Published              *agentcontract.Draft           `json:"published,omitempty"`
	Labels                 map[string]agentcontract.Draft `json:"labels,omitempty"`
	CredentialRef          *string                        `json:"credential_ref,omitempty"`
	PublishedCredentialRef *string                        `json:"published_credential_ref,omitempty"`
}

// BillingImportEntry is a provider's billing import in a configuration
// document. The credential travels only through secret_bindings under
// credential_ref; it is never part of the document.
type BillingImportEntry struct {
	Enabled       bool            `json:"enabled"`
	Scope         json.RawMessage `json:"scope"`
	CredentialRef string          `json:"credential_ref"`
}

type CodeConfiguration struct {
	Accounts []CodeAccountEntry `json:"accounts"`
	Pools    []CodePoolEntry    `json:"pools"`
	Routes   []CodeRouteEntry   `json:"routes"`
	Budgets  []CodeBudgetEntry  `json:"budgets"`
}

type CodeBindings struct {
	Credentials map[string]string `json:"credentials"`
	Users       map[string]string `json:"users"`
	APIKeys     map[string]string `json:"api_keys"`
}

type CodeAccountEntry struct {
	ID                string   `json:"id"`
	Project           string   `json:"project"`
	Provider          string   `json:"provider"`
	CredentialRef     string   `json:"credential_ref"`
	Name              string   `json:"name"`
	Enabled           bool     `json:"enabled"`
	Models            []string `json:"models"`
	ExtraUsageEnabled *bool    `json:"extra_usage_enabled"`
	CycleAnchor       *string  `json:"cycle_anchor,omitempty"`
}

type CodePoolEntry struct {
	ID         string           `json:"id"`
	Project    string           `json:"project"`
	Name       string           `json:"name"`
	Kind       string           `json:"kind"`
	OwnerRef   *string          `json:"owner_ref"`
	Accounts   map[string]int64 `json:"accounts"`
	APIKeyRefs []string         `json:"api_key_refs"`
}

type CodePaidTarget struct {
	Provider      string `json:"provider"`
	Slot          string `json:"slot"`
	CredentialRef string `json:"credential_ref,omitempty"`
}

type CodePaidConnection struct {
	Provider    ProviderEntry       `json:"provider"`
	SlotKeyRefs map[string][]string `json:"slot_key_refs"`
}

type CodeRoutePolicy struct {
	ResponsesToGrok                     bool                      `json:"responses_to_grok,omitempty"`
	MessagesToGrok                      bool                      `json:"messages_to_grok,omitempty"`
	MessagesGrokSafetyIdentifier        bool                      `json:"messages_grok_safety_identifier,omitempty"`
	GrokCLIIdentity                     bool                      `json:"grok_cli_identity,omitempty"`
	CodexCLIIdentity                    bool                      `json:"codex_cli_identity,omitempty"`
	PaidFallbackTargets                 map[string]CodePaidTarget `json:"paid_fallback_targets,omitempty"`
	ChatToAntigravity                   bool                      `json:"chat_to_antigravity,omitempty"`
	PaidParticipation                   bool                      `json:"paid_participation"`
	PaidTargets                         map[string]CodePaidTarget `json:"paid_targets,omitempty"`
	Pool                                string                    `json:"pool"`
	Models                              []string                  `json:"models"`
	Enabled                             bool                      `json:"enabled"`
	ModelPools                          map[string][]string       `json:"model_pools"`
	InitialRecoveryAttempts             int                       `json:"initial_recovery_attempts"`
	ModelRewriting                      bool                      `json:"model_rewriting"`
	Aliases                             map[string]string         `json:"aliases"`
	ChatToCodex                         bool                      `json:"chat_to_codex,omitempty"`
	MessagesToCodex                     bool                      `json:"messages_to_codex"`
	MessagesToAntigravity               bool                      `json:"messages_to_antigravity,omitempty"`
	ResponsesToAntigravity              bool                      `json:"responses_to_antigravity,omitempty"`
	MessagesAntigravitySafetyIdentifier bool                      `json:"messages_antigravity_safety_identifier,omitempty"`
	MessagesCodexSafetyIdentifier       bool                      `json:"messages_codex_safety_identifier,omitempty"`
	AllowanceOrdering                   bool                      `json:"allowance_ordering,omitempty"`
	Tools                               *codemode.ResearchTools   `json:"tools"`
	MaxBodyBytes                        *int64                    `json:"max_body_bytes"`
}

type CodeConnectionEntry struct {
	Configuration        providers.Configuration `json:"configuration"`
	NetworkCredentialRef *string                 `json:"network_credential_ref,omitempty"`
}

type CodePublication struct {
	PaidConnections map[string]CodePaidConnection  `json:"paid_connections,omitempty"`
	PaidResolved    map[string]routing.Provider    `json:"-"`
	Policy          CodeRoutePolicy                `json:"policy"`
	Connections     map[string]CodeConnectionEntry `json:"connections"`
}

type CodeRouteEntry struct {
	ID        string           `json:"id"`
	Project   string           `json:"project"`
	Slug      string           `json:"slug"`
	Draft     CodeRoutePolicy  `json:"draft"`
	Published *CodePublication `json:"published,omitempty"`
}

type CodeBudgetEntry struct {
	ID            string  `json:"id"`
	Project       string  `json:"project"`
	Route         *string `json:"route"`
	APIKeyRef     *string `json:"api_key_ref"`
	DailyTokens   *int64  `json:"daily_tokens"`
	MonthlyTokens *int64  `json:"monthly_tokens"`
	Enabled       bool    `json:"enabled"`
}

type GuardrailPolicyRule struct {
	Guardrail string `json:"guardrail"`
	Phase     string `json:"phase"`
	Action    string `json:"action"`
	Monitor   bool   `json:"monitor,omitempty"`
	Mandatory bool   `json:"mandatory,omitempty"`
}

type GuardrailPolicyEntry struct {
	Name    string                `json:"name"`
	Project string                `json:"project"`
	Entries []GuardrailPolicyRule `json:"entries"`
}

type GuardrailAttachmentEntry struct {
	Scope         string `json:"scope"`
	Name          string `json:"name,omitempty"`
	Project       string `json:"project,omitempty"`
	Policy        string `json:"policy"`
	PolicyProject string `json:"policy_project"`
	Order         int    `json:"order"`
}

type GuardrailEntry struct {
	Configuration json.RawMessage       `json:"configuration,omitempty"`
	TimeoutMS     int64                 `json:"timeout_ms,omitempty"`
	OnError       string                `json:"on_error,omitempty"`
	Name          string                `json:"name"`
	Project       string                `json:"project"`
	Type          string                `json:"type"`
	Policy        *contentpolicy.Policy `json:"policy,omitempty"`
}

type MCPServerEntry struct {
	Delegation           *routing.MCPDelegation   `json:"delegation,omitempty"`
	Stdio                *routing.MCPStdioBinding `json:"stdio,omitempty"`
	OAuth                *routing.MCPOAuth        `json:"oauth,omitempty"`
	Name                 string                   `json:"name"`
	Project              string                   `json:"project"`
	Transport            string                   `json:"transport"`
	Endpoint             string                   `json:"endpoint"`
	Enabled              bool                     `json:"enabled"`
	CatalogDigest        string                   `json:"catalog_digest"`
	CredentialRef        *string                  `json:"credential_ref,omitempty"`
	UnitPrice            *string                  `json:"unit_price"`
	ToolPrices           map[string]string        `json:"tool_prices,omitempty"`
	CallsPerMinute       *int64                   `json:"calls_per_minute"`
	MaxConcurrency       *int64                   `json:"max_concurrency"`
	PerKeyCallsPerMinute *int64                   `json:"per_key_calls_per_minute"`
	PerKeyMaxConcurrency *int64                   `json:"per_key_max_concurrency"`
}

type OrganizationEntry struct {
	Name   string                     `json:"name"`
	Budget *keyauthority.BudgetPolicy `json:"budget,omitempty"`
}

// RetrievalBindingEntry references destination keys by exact project-local
// names. Ambiguous or absent names block promotion; grants are never widened
// to all keys or copied by coincidentally equal UUIDs across installations.
type RetrievalBindingEntry struct {
	Name           string           `json:"name"`
	Project        string           `json:"project"`
	Route          string           `json:"route"`
	TargetProvider string           `json:"target_provider"`
	Backend        string           `json:"backend"`
	Resource       string           `json:"backend_resource"`
	Fields         retrieval.Fields `json:"fields"`
	KeyGrants      []string         `json:"key_grants"`
	Enabled        bool             `json:"enabled"`
}

type SCIMGroupMapping struct {
	Name     string               `json:"name"`
	Role     *string              `json:"role"`
	Scope    string               `json:"access_scope"`
	Projects []SCIMProjectMapping `json:"projects"`
}

type SCIMProjectMapping struct {
	Project string `json:"project"`
	Role    string `json:"role"`
}

type SinkEntry struct {
	Name          string   `json:"name"`
	Project       *string  `json:"project,omitempty"`
	Type          string   `json:"type"`
	Destination   string   `json:"destination"`
	Streams       []string `json:"streams"`
	Enabled       bool     `json:"enabled"`
	CredentialRef *string  `json:"credential_ref,omitempty"`
}

// WorkloadIssuerEntry replaces installation-local project IDs with project names.
// Principals, digests, signing keys and creator identities are never promoted.
type WorkloadIssuerEntry struct {
	workload.Config
	Mappings []WorkloadMappingEntry `json:"mappings"`
}

type WorkloadMappingEntry struct {
	AgentPrincipal     bool              `json:"agent_principal,omitempty"`
	Name               string            `json:"name"`
	Match              map[string]string `json:"match"`
	Project            string            `json:"project"`
	LimitTemplate      string            `json:"limit_template"`
	RouteGroups        []string          `json:"route_groups"`
	AllowedToolsets    []string          `json:"allowed_toolsets,omitempty"`
	AllowedAgents      []string          `json:"allowed_agents,omitempty"`
	Scopes             []string          `json:"scopes"`
	EndUserClaim       *string           `json:"end_user_claim"`
	AllowProviderState bool              `json:"allow_provider_state"`
}

func SinkKey(entry SinkEntry) [2]string {
	project := ""
	if entry.Project != nil {
		project = strings.ToLower(*entry.Project)
	}
	return [2]string{project, strings.ToLower(entry.Name)}
}

func RetrievalKey(e RetrievalBindingEntry) string {
	return strings.ToLower(e.Project) + "\x00" + strings.ToLower(e.Name)
}

func DiscountScope(kind string, provider, vendor *string) string {
	return kind + "\x00" + Lower(provider) + "\x00" + Lower(vendor)
}

func CardKey(owner CardOwnerEntry) string {
	switch owner.Kind {
	case "organization":
		return "organization\x00" + Lower(owner.Organization)
	case "project":
		return "project\x00" + Lower(owner.Project)
	default:
		return "installation"
	}
}

func MCPKey(e MCPServerEntry) [2]string {
	return [2]string{strings.ToLower(e.Project), strings.ToLower(e.Name)}
}

func PublicationKey(e PublicationEntry) string {
	// Names may contain delimiters; encode the complete identity as a tuple.
	key, _ := json.Marshal([3]string{strings.ToLower(e.Project), e.Kind, strings.ToLower(e.Draft.Name)})
	return string(key)
}
