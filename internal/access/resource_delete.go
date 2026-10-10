package access

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/secrets"
)

// These closed specifications share the conditional mutation protocol while
// retaining each resource's dependencies and accounting/delivery history.
type deletableResource struct {
	table, parameter, kind string
	authority              bool
}

func (s *Server) deleteBudgetGroup(r *http.Request, p Principal) (Reply, error) {
	return s.deleteResource(r, p, deletableResource{"budget_groups", "budget_group_id", "budget_group", true})
}
func (s *Server) deleteNotificationDestination(r *http.Request, p Principal) (Reply, error) {
	return s.deleteResource(r, p, deletableResource{"notification_destinations", "notification_destination_id", "notification_destination", false})
}
func (s *Server) deleteNotificationRule(r *http.Request, p Principal) (Reply, error) {
	return s.deleteResource(r, p, deletableResource{"notification_rules", "notification_rule_id", "notification_rule", false})
}

func (s *Server) deleteResource(r *http.Request, _ Principal, spec deletableResource) (Reply, error) {
	id, err := IDParam(r, spec.parameter)
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, map[string]string{spec.parameter: id})
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	table := pgx.Identifier{"olp", spec.table}.Sanitize()
	var etag string
	var projectID *string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text,project_id::text FROM "+table+" WHERE id=$1 FOR UPDATE", id).Scan(&etag, &projectID); err != nil {
		return Reply{}, err
	}
	if err = p.Project(projectID, Change); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if spec.kind == "budget_group" {
		var used bool
		if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.notification_rules WHERE subject_kind='budget_group' AND subject_id=$1)`, id).Scan(&used); err != nil {
			return Reply{}, err
		}
		if used {
			return Reply{}, Fail(409, "budget_group_in_use", "The budget group still has notification rules or retained accounting dependencies.")
		}
	}
	var secretID *string
	if spec.kind == "notification_destination" {
		if err = tx.QueryRow(r.Context(), "SELECT secret_id::text FROM "+table+" WHERE id=$1", id).Scan(&secretID); err != nil {
			return Reply{}, err
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM "+table+" WHERE id=$1", id); err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23503" {
			return Reply{}, Fail(409, spec.kind+"_in_use", "The resource still has dependent resources or retained history.")
		}
		return Reply{}, err
	}
	if secretID != nil {
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *secretID, secrets.NotificationSecret); err != nil {
			return Reply{}, err
		}
	}
	if spec.authority {
		if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
			return Reply{}, err
		}
	}
	if err = AuditForProject(r.Context(), tx, r, p.Actor(), spec.kind+".delete", spec.kind, id, "success", projectID); err != nil {
		return Reply{}, err
	}
	result := Reply{Status: http.StatusNoContent}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}
