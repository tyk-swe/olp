package identity

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type sessionAuth struct {
	Method string
	MFA    bool
}

func withPoolTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func admitAuth(s *access.Server, r *http.Request, action, target string) error {
	source := s.ClientIP(r)
	sourceLimit := 60
	if action == "invitation" {
		sourceLimit = 30
	}
	type admissionBucket struct {
		key   string
		limit int
	}
	buckets := []admissionBucket{{"global", 10000}, {"source:" + source, sourceLimit}}
	if target != "" {
		buckets = append(buckets,
			admissionBucket{"target:" + source + ":" + target, 5},
			admissionBucket{"account:" + target, 30},
		)
	}
	admitted := true
	err := withPoolTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		for _, bucket := range buckets {
			var count int
			err := tx.QueryRow(r.Context(), `INSERT INTO olp.auth_admission(action,digest,attempts) VALUES($1,$2,1)
				ON CONFLICT(action,digest) DO UPDATE SET attempts=CASE WHEN auth_admission.window_started_at<=now()-interval '1 minute' THEN 1 ELSE LEAST(auth_admission.attempts+1,$3+1) END,
				window_started_at=CASE WHEN auth_admission.window_started_at<=now()-interval '1 minute' THEN now() ELSE auth_admission.window_started_at END RETURNING attempts`,
				action, s.Auth.Digest(secrets.AdmissionDigest, bucket.key), bucket.limit).Scan(&count)
			if err != nil {
				return err
			}
			if count > bucket.limit {
				admitted = false
				break
			}
		}
		_, err := tx.Exec(r.Context(), "DELETE FROM olp.auth_admission WHERE ctid IN(SELECT ctid FROM olp.auth_admission WHERE window_started_at<now()-interval '10 minutes' LIMIT 1000)")
		return err
	})
	if err != nil {
		return err
	}
	if !admitted {
		return httpkit.Fail(429, "authentication_rate_limited", "Too many attempts. Try again in a minute.")
	}
	return nil
}

func isLocalLoginEnabled(ctx context.Context, s *access.Server, q database.Queryer) (bool, error) {
	var enabled bool
	err := q.QueryRow(ctx, "SELECT COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true)").Scan(&enabled)
	return enabled && !s.LocalLoginDisabled(), err
}

func sessionStrength(ctx context.Context, q database.Queryer, sessionID string) (sessionAuth, error) {
	var strength sessionAuth
	err := q.QueryRow(ctx, "SELECT auth_method,mfa_verified FROM olp.sessions WHERE id=$1", sessionID).Scan(&strength.Method, &strength.MFA)
	return strength, err
}

func mfaConfigured(ctx context.Context, q database.Queryer, userID string) (bool, error) {
	var enabled bool
	err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.mfa_factors WHERE user_id=$1)", userID).Scan(&enabled)
	return enabled, err
}

func retireIssuedInvitations(ctx context.Context, tx pgx.Tx, r *http.Request, issuer string, actor authz.Actor, revokedBy string) error {
	result, err := tx.Exec(ctx, `UPDATE olp.invitations
		SET revoked_at=now(),revoked_by=NULLIF($2::text,'')::uuid
		WHERE invited_by=$1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at>now()`, issuer, revokedBy)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return nil
	}
	req := r
	if req == nil {
		req = &http.Request{}
	}
	return access.Audit(ctx, tx, req, actor, "invitation.revoke_for_access_loss", "user", issuer, "success")
}

func scimManagedProjects(ctx context.Context, tx pgx.Tx) ([]string, error) {
	var ids []string
	err := tx.QueryRow(ctx, "SELECT COALESCE(array_agg(DISTINCT project_id::text),'{}'::text[]) FROM olp.effective_project_members WHERE role='manager' AND project_id IN (SELECT project_id FROM olp.scim_group_projects WHERE role='manager')").Scan(&ids)
	return ids, err
}

func scimOrphansProject(ctx context.Context, tx pgx.Tx, projects []string) (bool, error) {
	if len(projects) == 0 {
		return false, nil
	}
	var orphaned bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.projects p WHERE p.id=ANY($1::uuid[]) AND NOT EXISTS(SELECT 1 FROM olp.effective_project_members m WHERE m.project_id=p.id AND m.role='manager'))", projects).Scan(&orphaned)
	return orphaned, err
}

func keepSCIMManagers(ctx context.Context, tx pgx.Tx, projects []string) error {
	orphaned, err := scimOrphansProject(ctx, tx, projects)
	if err == nil && orphaned {
		err = httpkit.Fail(409, "last_project_manager", "Keep at least one project manager.")
	}
	return err
}
