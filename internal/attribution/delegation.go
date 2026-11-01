package attribution

import (
	"encoding/hex"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/secrets"
)

func DelegationPair(auth *secrets.AuthKey, project, key, agent string, member *string) string {
	if member == nil {
		pair, _ := json.Marshal([3]string{project, key, agent})
		return hex.EncodeToString(auth.Digest(secrets.DelegationPairDigest, string(pair)))
	}
	pair, _ := json.Marshal([4]string{project, "member", *member, agent})
	return hex.EncodeToString(auth.Digest(secrets.DelegationPairDigest, string(pair)))
}
