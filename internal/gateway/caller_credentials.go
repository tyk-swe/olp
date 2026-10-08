package gateway

import (
	"net/http"
	"strings"
	"sync"

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
	// A video list polls its jobs concurrently, so binding is atomic.
	mu     sync.Mutex
	target string
}

// bind reports whether the credential may serve target, binding it to the
// first target that asks.
func (c *callerCredential) bind(target string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.target == "" {
		c.target = target
	}
	return c.target == target
}

// boundElsewhere reports whether the credential already serves another target.
func (c *callerCredential) boundElsewhere(target string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target != "" && c.target != target
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
	secret, err := x.bindCallerSecret(provider, model)
	if err != nil {
		return nil, err
	}
	// Remember even malformed credentials before parsing/application so errors
	// cannot echo their original representation.
	x.sensitive.Add(string(secret))
	return secret, nil
}

// bindCallerSecret binds the caller credential to a target without recording
// it, for calls that redact with their own collection.
func (x *execution) bindCallerSecret(provider, model string) ([]byte, error) {
	c := x.request.callerCredential
	if c == nil || c.invalid || x.origin != "" || !c.bind(provider+"\x00"+model) {
		return nil, connectors.ErrCredentialRejected
	}
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
	if c.boundElsewhere(provider.ID + "\x00" + model) {
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
