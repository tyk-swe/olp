package secrets

import (
	"encoding/hex"
	"encoding/json"
)

// DigestEndUser separates both the installation and project boundaries. A nil
// project names the installation's unassigned boundary, never a wildcard.
func DigestEndUser(auth *AuthKey, projectID *string, identifier string) string {
	value, _ := json.Marshal([]any{projectID, identifier})
	return hex.EncodeToString(auth.Digest(EndUserDigest, string(value)))
}
