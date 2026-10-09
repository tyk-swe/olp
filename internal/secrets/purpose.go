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
	SAMLStateDigest       = DigestPurpose{"saml_state"}
	SAMLCookieDigest      = DigestPurpose{"saml_cookie"}
	SAMLAssertionDigest   = DigestPurpose{"saml_assertion"}
	MFAChallengeDigest    = DigestPurpose{"mfa_challenge"}
	MFARecoveryDigest     = DigestPurpose{"mfa_recovery"}
	WorkloadDigest        = DigestPurpose{"workload_identity"}
	APIKeyDigest          = DigestPurpose{"api_key"}
	EndUserDigest         = DigestPurpose{"end_user"}
	ManagementTokenDigest = DigestPurpose{"management_token"}
	SessionDigest         = DigestPurpose{"session"}
	RecentAuthDigest      = DigestPurpose{"recent_auth"}
	CSRFDigest            = DigestPurpose{"csrf"}
	OIDCStateDigest       = DigestPurpose{"oidc_state"}
	OIDCCookieDigest      = DigestPurpose{"oidc_cookie"}
	InvitationDigest      = DigestPurpose{"invitation"}
	AdmissionDigest       = DigestPurpose{"admission"}
	MutationDigest        = DigestPurpose{"mutation"}
	InstallationDigest    = DigestPurpose{"installation"}
)

// DigestPurposes lists every digest purpose.
func DigestPurposes() []DigestPurpose {
	return []DigestPurpose{SAMLStateDigest, SAMLCookieDigest, SAMLAssertionDigest, MFAChallengeDigest, MFARecoveryDigest, APIKeyDigest, WorkloadDigest, EndUserDigest, ManagementTokenDigest, SessionDigest, RecentAuthDigest, CSRFDigest,
		OIDCStateDigest, OIDCCookieDigest, InvitationDigest, AdmissionDigest, MutationDigest, InstallationDigest}
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
	MutationReplay       = SealPurpose{"mutation_replay"}
	OIDCClientSecret     = SealPurpose{"oidc_client"}
	OIDCFlow             = SealPurpose{"oidc_flow"}
	MediaJobSource       = SealPurpose{"media_job_source"}
	// ProviderGrantRefresh is a refresh token read only by control and workers.
	ProviderGrantRefresh = SealPurpose{"provider_grant_refresh"}
	// GrantEnrollment holds short-lived plugin enrollment state.
	GrantEnrollment = SealPurpose{"grant_enrollment"}
)

// SealPurposes lists every seal purpose.
func SealPurposes() []SealPurpose {
	return []SealPurpose{SAMLKey, SAMLFlow, MFATOTP, MFAWebAuthn, ProviderCredential, ProviderContinuation, NotificationSecret, MutationReplay,
		OIDCClientSecret, OIDCFlow, MediaJobSource, ProviderGrantRefresh, GrantEnrollment, SinkCredential}
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
