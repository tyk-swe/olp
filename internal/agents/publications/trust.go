package publications

import (
	"reflect"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/domain/agentcontract"
)

// AuthorizeTrustChange keeps destination/card/credential trust installation-
// controlled even when a project manager may edit ordinary draft policy.
func AuthorizeTrustChange(p authz.Principal, current *agentcontract.Publication, next agentcontract.Draft) error {
	if next.Agent == nil {
		return nil
	}
	if current == nil || current.Draft.Agent == nil {
		return p.Authorize(authz.Access)
	}
	old := current.Draft.Agent
	value := next.Agent
	if old.Endpoint != value.Endpoint || old.Protocol != value.Protocol || old.CardDigest != value.CardDigest || !reflect.DeepEqual(old.CredentialID, value.CredentialID) || !reflect.DeepEqual(old.OAuth, value.OAuth) {
		return p.Authorize(authz.Access)
	}
	return nil
}
