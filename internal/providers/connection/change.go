package connection

import (
	"bytes"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/providerconfig"
)

type Change struct {
	Changed    bool
	Transport  bool
	Enrollment bool
}

func Diff(before, after Configuration) (Change, error) {
	previous, err := json.Marshal(before)
	if err != nil {
		return Change{}, err
	}
	next, err := json.Marshal(after)
	if err != nil {
		return Change{}, err
	}
	return Change{
		Changed:    !bytes.Equal(previous, next),
		Transport:  before.Fingerprint() != after.Fingerprint(),
		Enrollment: before.enrollmentBoundary() != after.enrollmentBoundary(),
	}, nil
}

type CredentialMove struct {
	Destination   bool
	GrantIdentity bool
	Network       bool
}

// MoveCredentials compares where and how current and next send a secret. A
// grant may move to another build or profile because each grant checks that
// identity before use, so GrantIdentity reports a move of nothing else.
func MoveCredentials(current, next Configuration) CredentialMove {
	if current.credentialBoundary() == next.credentialBoundary() {
		return CredentialMove{}
	}
	sameIdentity := next
	sameIdentity.ProfileID, sameIdentity.ProfileRevision = current.ProfileID, current.ProfileRevision
	return CredentialMove{
		Destination:   true,
		GrantIdentity: current.Grant() && next.Grant() && current.credentialBoundary() == sameIdentity.credentialBoundary(),
		Network:       current.credentialNetwork() != next.credentialNetwork() || providerconfig.Value(current.Endpoint) != providerconfig.Value(next.Endpoint),
	}
}

// credentialBoundary includes inputs that can change where or how a secret
// is sent. Credential IDs are excluded so rotation preserves the boundary.
func (c Configuration) credentialBoundary() string {
	encoded, _ := json.Marshal([]any{c.Kind, c.AuthMode, c.Endpoint, c.CloudRegion, c.CloudProject, c.BatchStorage,
		c.CredentialSource, c.ProfileID, c.ProfileRevision, c.Options.CredentialHeaders,
		c.credentialNetwork(), c.Options.PluginOptions})
	return string(encoded)
}

// An enrollment also pins the concrete network credential version used by
// its challenge. Ordinary credentials can rotate within the same destination.
func (c Configuration) enrollmentBoundary() string {
	if c.Options.Network == nil {
		return c.credentialBoundary() + "\x00"
	}
	return c.credentialBoundary() + "\x00" + c.Options.Network.CredentialID
}

func (c Configuration) credentialNetwork() [2]string {
	if c.Options.Network == nil {
		return [2]string{}
	}
	return [2]string{c.Options.Network.ProxyURL, c.Options.Network.TrustRootsPEM}
}
