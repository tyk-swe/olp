package access

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/tyk-swe/olp/internal/secrets"
)

var endUserIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ValidEndUserIdentifier bounds identifiers before hashing or forwarding.
// Whitespace, email addresses and arbitrary text are not machine tokens.
func ValidEndUserIdentifier(identifier string) bool {
	return endUserIdentifier.MatchString(identifier)
}

// DigestEndUser separates both the installation and project boundaries. A nil
// project names the installation's unassigned boundary, never a wildcard.
func DigestEndUser(auth *secrets.AuthKey, projectID *string, identifier string) string {
	value, _ := json.Marshal([]any{projectID, identifier})
	return hex.EncodeToString(auth.Digest(secrets.EndUserDigest, string(value)))
}

func (s *Server) lookupEndUser(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "api_key_id")
	if err != nil {
		return Reply{}, err
	}
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT project_id::text FROM olp.api_keys WHERE id=$1", id).Scan(&projectID); err != nil {
		return Reply{}, err
	}
	if err = p.Project(projectID, View); err != nil {
		return Reply{}, err
	}
	var input struct {
		Identifier string `json:"identifier"`
	}
	if err = Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if !ValidEndUserIdentifier(input.Identifier) {
		return Reply{}, Invalid("identifier", "Provide an end-user machine token of 1–128 characters.")
	}
	// Deliberately no mutation replay, audit payload, or raw-identifier query.
	return OK(map[string]string{"end_user_digest": DigestEndUser(s.Auth, projectID, input.Identifier)}), nil
}
