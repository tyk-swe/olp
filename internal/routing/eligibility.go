package routing

// Eligibility is whether a credential version may serve and, when it may not,
// why. Planning, slot availability and dispatch all ask for it, so plan and
// attempt records name the same reason.
type Eligibility string

const (
	// Eligible is the only eligibility that serves.
	Eligible Eligibility = ""
	// Revoked marks a credential version an operator revoked.
	Revoked Eligibility = "revoked"
	// Lapsed marks a credential version whose grant lapsed: it can no longer
	// be refreshed, or whose original profile was not recorded. Only a new
	// grant enrollment replaces it.
	Lapsed Eligibility = "lapsed"
	// StaleAuthority withholds every credential version: the last authority
	// read is too old to vouch for any of them.
	StaleAuthority Eligibility = "stale_authority"
	// ExternalUnavailable makes a failed or expired store resolution visible in
	// route plans; a static credential is never substituted for a reference.
	ExternalUnavailable Eligibility = "external_secret_unavailable"
)
