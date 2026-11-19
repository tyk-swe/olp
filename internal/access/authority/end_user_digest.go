package authority

import (
	"encoding/hex"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/secrets"
)

// DigestEndUser separates both the installation and project boundaries. A nil
// project names the installation's unassigned boundary, never a wildcard.
func DigestEndUser(auth *secrets.AuthKey, projectID *string, identifier string) string {
	value, _ := json.Marshal([]any{projectID, identifier})
	return hex.EncodeToString(auth.Digest(secrets.EndUserDigest, string(value)))
}
