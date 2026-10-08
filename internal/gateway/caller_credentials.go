package gateway

import (
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
)

// Use net/http canonical spelling to avoid allocating during absent-header lookups.
const callerCredentialHeader = "X-Olp-Provider-Credential"

// callerCredential is request-only state. It binds the first consuming target;
// neither routing fallbacks nor classifier/shadow work can copy it elsewhere.
type callerCredential struct {
	secret  []byte
	invalid bool
	target  string
}

func readCallerCredential(headers http.Header) *callerCredential {
	values := headers.Values(callerCredentialHeader)
	if len(values) == 0 {
		return nil
	}
	c := &callerCredential{}
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 16384 || strings.TrimSpace(values[0]) != values[0] || strings.ContainsAny(values[0], "\r\n\x00") {
		c.invalid = true
		return c
	}
	c.secret = []byte(values[0])
	return c
}

func (x *execution) callerSecret(provider, model string) ([]byte, error) {
	c := x.request.callerCredential
	if c == nil || c.invalid || x.origin != "" {
		return nil, connectors.ErrCredentialRejected
	}
	target := provider + "\x00" + model
	if c.target != "" && c.target != target {
		return nil, connectors.ErrCredentialRejected
	}
	c.target = target
	// Remember even malformed credentials before parsing/application so errors
	// cannot echo their original representation.
	x.sensitive.Add(string(c.secret))
	return c.secret, nil
}

func (x *execution) checkCallerTarget(provider *runtime.Provider, model string) *Error {
	if x.callerCostExempt() && provider.CredentialSource != "caller" {
		return invalidRequest("caller_cost_policy_mismatch", "This retained target is incompatible with the caller-paid route policy.", nil)
	}
	if provider.CredentialSource != "caller" || x.origin == "probe" {
		return nil
	}
	c := x.request.callerCredential
	if c == nil || c.invalid || x.origin != "" {
		return invalidRequest("provider_credential_required", "Provide one bounded X-OLP-Provider-Credential header for this connection.", nil)
	}
	if c.target != "" && c.target != provider.ID+"\x00"+model {
		return invalidRequest("provider_credential_scope", "The caller credential cannot be reused for another target.", nil)
	}
	return nil
}

// The named ingress policy governs all attempts in a request.
func (x *execution) callerCostExempt() bool {
	if x == nil || x.origin != "" || x.named() == nil {
		return false
	}
	if x.request.release != nil && x.request.release.Snapshot != nil {
		if current, ok := x.request.release.Snapshot.Routes[x.named().Slug]; ok {
			return current.CallerCostExempt
		}
	}
	return x.named().CallerCostExempt
}
