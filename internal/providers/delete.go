package providers

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

// Deletion is deliberately separate from disable: it removes an unused draft,
// while published revisions and retained evidence remain addressable.
func (s *Server) deleteProvider(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, map[string]string{"provider_id": id})
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	current, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	var used bool
	err = tx.QueryRow(r.Context(), `SELECT
 EXISTS(SELECT 1 FROM olp.provider_revisions WHERE provider_id=$1)
 OR EXISTS(SELECT 1 FROM olp.route_drafts d,jsonb_array_elements(d.targets) t WHERE t->>'provider_id'=$1::text)
 OR EXISTS(SELECT 1 FROM olp.prices WHERE provider_id=$1)
 OR EXISTS(SELECT 1 FROM olp.notification_deliveries d JOIN olp.provider_credentials c ON c.id=d.credential_id WHERE c.provider_id=$1)`, id).Scan(&used)
	if err != nil {
		return access.Reply{}, err
	}
	if current.State != "draft" || used {
		return access.Reply{}, access.Fail(409, "provider_in_use", "Disable published providers; deletion requires an unused draft without retained history.")
	}
	rows, err := tx.Query(r.Context(), `SELECT id::text FROM olp.provider_credentials WHERE provider_id=$1
 UNION SELECT id::text FROM olp.provider_network_credentials WHERE provider_id=$1
 UNION SELECT g.refresh_token_id::text FROM olp.provider_grants g JOIN olp.provider_credentials c ON c.id=g.credential_id WHERE c.provider_id=$1 AND g.refresh_token_id IS NOT NULL
 UNION SELECT id::text FROM olp.grant_enrollments WHERE provider_id=$1`, id)
	if err != nil {
		return access.Reply{}, err
	}
	secretIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.providers WHERE id=$1", id); err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23503" {
			return access.Reply{}, access.Fail(409, "provider_in_use", "The provider has dependent resources or retained history.")
		}
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), `DELETE FROM olp.secrets WHERE id=ANY($1::uuid[]) AND purpose IN ($2,$3,$4)`, secretIDs, secrets.ProviderCredential, secrets.ProviderGrantRefresh, secrets.GrantEnrollment); err != nil {
		return access.Reply{}, err
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return access.Reply{}, err
	}
	if err = access.AuditForProject(r.Context(), tx, r, p.Actor(), "provider.delete", "provider", id, "success", current.ProjectID); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusNoContent}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}
