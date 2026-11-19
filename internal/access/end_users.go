package access

import (
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/secrets"
)

// DigestEndUser separates both the installation and project boundaries. A nil
// project names the installation's unassigned boundary, never a wildcard.
func DigestEndUser(auth *secrets.AuthKey, projectID *string, identifier string) string {
	value, _ := json.Marshal([]any{projectID, identifier})
	return hex.EncodeToString(auth.Digest(secrets.EndUserDigest, string(value)))
}

func (s *Server) lookupEndUser(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "api_key_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT project_id::text FROM olp.api_keys WHERE id=$1", id).Scan(&projectID); err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(projectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	var input struct {
		Identifier string `json:"identifier"`
	}
	if err = httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if !authority.ValidEndUserIdentifier(input.Identifier) {
		return httpkit.Reply{}, httpkit.Invalid("identifier", "Provide an end-user machine token of 1–128 characters.")
	}
	// Deliberately no mutation replay, audit payload, or raw-identifier query.
	return httpkit.OK(map[string]string{"end_user_digest": DigestEndUser(s.Auth, projectID, input.Identifier)}), nil
}
