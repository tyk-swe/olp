package apikeys

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

func (a *API) lookupEndUser(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "api_key_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var projectID *string
	if err = a.server.Pool.QueryRow(r.Context(), "SELECT project_id::text FROM olp.api_keys WHERE id=$1", id).Scan(&projectID); err != nil {
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
	return httpkit.OK(map[string]string{"end_user_digest": secrets.DigestEndUser(a.server.Auth, projectID, input.Identifier)}), nil
}
