package secrets

import "database/sql/driver"

// DigestPurpose domain-separates an HMAC digest: a digest computed for one
// purpose never verifies for another. Its name is bound into every stored
// digest, so a name can never change. The type cannot be built outside this
// package, so every purpose is declared here.
type DigestPurpose struct{ name string }

func (p DigestPurpose) String() string { return p.name }

// Digest purposes.
var (
	SAMLStateDigest          = DigestPurpose{"saml_state"}
	SAMLCookieDigest         = DigestPurpose{"saml_cookie"}
	SAMLAssertionDigest      = DigestPurpose{"saml_assertion"}
	MFAChallengeDigest       = DigestPurpose{"mfa_challenge"}
	MFARecoveryDigest        = DigestPurpose{"mfa_recovery"}
	WorkloadDigest           = DigestPurpose{"workload_identity"}
	WorkloadAgentDigest      = DigestPurpose{"workload_agent_identity"}
	CacheKeyDigest           = DigestPurpose{"cache_key"}
	APIKeyDigest             = DigestPurpose{"api_key"}
	EndUserDigest            = DigestPurpose{"end_user"}
	ManagementTokenDigest    = DigestPurpose{"management_token"}
	SessionDigest            = DigestPurpose{"session"}
	RecentAuthDigest         = DigestPurpose{"recent_auth"}
	DelegationConsentDigest  = DigestPurpose{"delegation_consent"}
	CSRFDigest               = DigestPurpose{"csrf"}
	OIDCStateDigest          = DigestPurpose{"oidc_state"}
	OIDCCookieDigest         = DigestPurpose{"oidc_cookie"}
	InvitationDigest         = DigestPurpose{"invitation"}
	AdmissionDigest          = DigestPurpose{"admission"}
	MutationDigest           = DigestPurpose{"mutation"}
	InstallationDigest       = DigestPurpose{"installation"}
	BatchSubmissionDigest    = DigestPurpose{"batch_submission"}
	RetainedSubmissionDigest = DigestPurpose{"retained_submission"}
	ApprovalDigest           = DigestPurpose{"agent_approval"}
	AgentPrincipalDigest     = DigestPurpose{"agent_principal"}
	DelegationTokenDigest    = DigestPurpose{"delegation_token"}
	DelegationPairDigest     = DigestPurpose{"delegation_pair"}
	BatchLineDigest          = DigestPurpose{"batch_line"}
	ToolInputDigest          = DigestPurpose{"tool_input"}
	ToolCallDigest           = DigestPurpose{"tool_call"}
	MCPEnrollmentDigest      = DigestPurpose{"mcp_enrollment"}
)

// DigestPurposes lists every digest purpose.
func DigestPurposes() []DigestPurpose {
	return []DigestPurpose{CacheKeyDigest, SAMLStateDigest, SAMLCookieDigest, SAMLAssertionDigest, MFAChallengeDigest, MFARecoveryDigest, APIKeyDigest, WorkloadDigest, WorkloadAgentDigest, EndUserDigest, ManagementTokenDigest, SessionDigest, RecentAuthDigest, CSRFDigest,
		OIDCStateDigest, OIDCCookieDigest, InvitationDigest, AdmissionDigest, MutationDigest, InstallationDigest, BatchSubmissionDigest, BatchLineDigest, ApprovalDigest, AgentPrincipalDigest, DelegationTokenDigest, DelegationConsentDigest, DelegationPairDigest, RetainedSubmissionDigest, ToolInputDigest, ToolCallDigest, MCPEnrollmentDigest}
}

// SealPurpose binds an encrypted secret to what it is for: a ciphertext sealed
// for one purpose never opens as another. Its name is authenticated data in
// every stored ciphertext and a column value the database constrains, so a
// name can never change. The type cannot be built outside this package, so
// every purpose is declared here.
type SealPurpose struct{ name string }

func (p SealPurpose) String() string { return p.name }

// Value stores the purpose's name, so queries bind a purpose rather than
// spelling it.
func (p SealPurpose) Value() (driver.Value, error) { return p.name, nil }

// Seal purposes.
var (
	SAMLKey  = SealPurpose{"saml_key"}
	SAMLFlow = SealPurpose{"saml_flow"}
	// ProviderCredential is a provider API or network (TLS and proxy) secret.
	ProviderCredential = SealPurpose{"provider_credential"}
	// ProviderContinuation is a retained provider resource's contract.
	ProviderContinuation = SealPurpose{"provider_continuation"}
	MFATOTP              = SealPurpose{"mfa_totp"}
	MFAWebAuthn          = SealPurpose{"mfa_webauthn"}
	NotificationSecret   = SealPurpose{"notification_secret"}
	SinkCredential       = SealPurpose{"sink_credential"}
	AgentCredential      = SealPurpose{"agent_credential"}
	MCPCredential        = SealPurpose{"mcp_credential"}
	MCPGrantAccess       = SealPurpose{"mcp_grant_access"}
	MCPGrantRefresh      = SealPurpose{"mcp_grant_refresh"}
	MCPEntraClient       = SealPurpose{"mcp_entra_client"}
	MCPDelegationSigning = SealPurpose{"mcp_delegation_signing"}
	MCPGrantEnrollment   = SealPurpose{"mcp_grant_enrollment"}
	MutationReplay       = SealPurpose{"mutation_replay"}
	OIDCClientSecret     = SealPurpose{"oidc_client"}
	OIDCFlow             = SealPurpose{"oidc_flow"}
	MediaJobSource       = SealPurpose{"media_job_source"}
	// ProviderGrantRefresh is a refresh token read only by control and workers.
	ProviderGrantRefresh = SealPurpose{"provider_grant_refresh"}
	// GrantEnrollment holds short-lived plugin enrollment state.
	GrantEnrollment = SealPurpose{"grant_enrollment"}
	// BillingCredential is a provider admin/billing API key; only the billing
	// importer reads it, and it never serves inference.
	BillingCredential     = SealPurpose{"billing_credential"}
	GuardrailCredential   = SealPurpose{"guardrail_credential"}
	ResponseCache         = SealPurpose{"response_cache"}
	ProviderUploadSession = SealPurpose{"provider_upload_session"}
	WorkflowContinuation  = SealPurpose{"workflow_continuation"}
)

// SealPurposes lists every seal purpose.
func SealPurposes() []SealPurpose {
	return []SealPurpose{SAMLKey, SAMLFlow, MFATOTP, MFAWebAuthn, ProviderCredential, ProviderContinuation, NotificationSecret, MutationReplay,
		OIDCClientSecret, OIDCFlow, MediaJobSource, ProviderGrantRefresh, GrantEnrollment, SinkCredential, MCPCredential, MCPGrantAccess, MCPGrantRefresh, MCPGrantEnrollment, MCPEntraClient, MCPDelegationSigning, BillingCredential, GuardrailCredential, ResponseCache, ProviderUploadSession, AgentCredential, WorkflowContinuation}
}

// ParseSealPurpose returns the seal purpose a stored record names.
func ParseSealPurpose(name string) (SealPurpose, bool) {
	for _, purpose := range SealPurposes() {
		if purpose.name == name {
			return purpose, true
		}
	}
	return SealPurpose{}, false
}
