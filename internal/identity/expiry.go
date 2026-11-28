package identity

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	purgeSessionsSQL = `WITH expired AS (
      SELECT ctid FROM olp.sessions WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.sessions target USING expired WHERE target.ctid = expired.ctid`

	purgeInvitationsSQL = `WITH expired AS (
      SELECT ctid FROM olp.invitations
       WHERE expires_at <= $1 AND accepted_at IS NULL AND revoked_at IS NULL
       LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.invitations target USING expired WHERE target.ctid = expired.ctid`

	purgeSAMLFlowsSQL = `WITH expired AS (
      SELECT ctid FROM olp.saml_flows WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.saml_flows target USING expired WHERE target.ctid = expired.ctid`

	purgeSAMLAssertionReplaysSQL = `WITH expired AS (
      SELECT ctid FROM olp.saml_assertion_replays WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.saml_assertion_replays target USING expired WHERE target.ctid = expired.ctid`

	purgeOIDCFlowsSQL = `WITH expired AS (
      SELECT ctid FROM olp.oidc_flows WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.oidc_flows target USING expired WHERE target.ctid = expired.ctid`

	purgeMFAChallengesSQL = `WITH expired AS (
      SELECT ctid FROM olp.mfa_challenges WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.mfa_challenges target USING expired WHERE target.ctid = expired.ctid`

	purgeRecentAuthSQL = `WITH expired AS (
      SELECT ctid FROM olp.recent_auth WHERE expires_at <= $1 LIMIT $2 FOR UPDATE SKIP LOCKED
    )
    DELETE FROM olp.recent_auth target USING expired WHERE target.ctid = expired.ctid`
)

func PurgeExpiredSessions(ctx context.Context, tx pgx.Tx, before time.Time, limit int) (int64, error) {
	return purgeExpired(ctx, tx, purgeSessionsSQL, before, limit)
}

func PurgeExpiredInvitations(ctx context.Context, tx pgx.Tx, before time.Time, limit int) (int64, error) {
	return purgeExpired(ctx, tx, purgeInvitationsSQL, before, limit)
}

func PurgeExpiredSAMLFlows(ctx context.Context, tx pgx.Tx, before time.Time, limit int) (int64, error) {
	return purgeExpired(ctx, tx, purgeSAMLFlowsSQL, before, limit)
}

func PurgeExpiredSAMLAssertionReplays(ctx context.Context, tx pgx.Tx, before time.Time, limit int) (int64, error) {
	return purgeExpired(ctx, tx, purgeSAMLAssertionReplaysSQL, before, limit)
}

func PurgeExpiredOIDCFlows(ctx context.Context, tx pgx.Tx, before time.Time, limit int) (int64, error) {
	return purgeExpired(ctx, tx, purgeOIDCFlowsSQL, before, limit)
}

func PurgeExpiredMFAChallenges(ctx context.Context, tx pgx.Tx, before time.Time, limit int) (int64, error) {
	return purgeExpired(ctx, tx, purgeMFAChallengesSQL, before, limit)
}

func PurgeExpiredRecentAuth(ctx context.Context, tx pgx.Tx, before time.Time, limit int) (int64, error) {
	return purgeExpired(ctx, tx, purgeRecentAuthSQL, before, limit)
}

func purgeExpired(ctx context.Context, tx pgx.Tx, statement string, before time.Time, limit int) (int64, error) {
	tag, err := tx.Exec(ctx, statement, before, limit)
	return tag.RowsAffected(), err
}
