package notifications

import (
	"errors"
	"github.com/tyk-swe/olp/internal/access"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/secrets"
)

// These closed specifications share the conditional mutation protocol while
// retaining each resource's dependencies and accounting/delivery history.
type deletableResource struct {
	table, parameter, kind string
}

func (s *Server) deleteNotificationDestination(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.deleteResource(r, p, deletableResource{"notification_destinations", "notification_destination_id", "notification_destination"})
}
func (s *Server) deleteNotificationRule(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.deleteResource(r, p, deletableResource{"notification_rules", "notification_rule_id", "notification_rule"})
}

func (s *Server) deleteResource(r *http.Request, _ access.Principal, spec deletableResource) (access.Reply, error) {
	id, err := access.IDParam(r, spec.parameter)
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	claim, replayed, err := s.access.Replay(r, tx, p, map[string]string{spec.parameter: id})
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {

		return *replayed, tx.Commit(r.Context())
	}
	table := pgx.Identifier{"olp", spec.table}.Sanitize()
	var etag string
	var projectID *string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text,project_id::text FROM "+table+" WHERE id=$1"+" FOR UPDATE", id).Scan(&etag, &projectID); err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(projectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	var secretID *string
	if spec.kind == "notification_destination" {
		if err = tx.QueryRow(r.Context(), "SELECT secret_id::text FROM "+table+" WHERE id=$1", id).Scan(&secretID); err != nil {
			return access.Reply{}, err
		}
	}
	_, err = tx.Exec(r.Context(), "DELETE FROM "+table+" WHERE id=$1", id)

	if err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23503" {
			return access.Reply{}, access.Fail(409, spec.kind+"_in_use", "The resource still has dependent resources or retained history.")
		}
		return access.Reply{}, err
	}
	if secretID != nil {
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *secretID, secrets.NotificationSecret); err != nil {
			return access.Reply{}, err
		}
	}
	if err = access.AuditForProject(r.Context(), tx, r, p.Actor(), spec.kind+".delete", spec.kind, id, "success", projectID); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusNoContent}
	if err = s.access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}
