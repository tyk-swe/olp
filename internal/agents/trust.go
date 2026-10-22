package agents

import (
	"github.com/tyk-swe/olp/internal/access"
	"reflect"
)

// AuthorizeTrustChange keeps destination/card/credential trust installation-
// controlled even when a project manager may edit ordinary draft policy.
func AuthorizeTrustChange(p access.Principal, current *Publication, next Draft) error {
	if next.Agent == nil {
		return nil
	}
	if current == nil || current.Draft.Agent == nil {
		return p.Authorize(access.Access)
	}
	old := current.Draft.Agent
	value := next.Agent
	if old.Endpoint != value.Endpoint || old.Protocol != value.Protocol || old.CardDigest != value.CardDigest || !reflect.DeepEqual(old.CredentialID, value.CredentialID) {
		return p.Authorize(access.Access)
	}
	return nil
}
