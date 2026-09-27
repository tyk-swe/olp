package grants

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// workerAgent is the user agent family of what a worker records in audit,
// where it acts on no user's or management token's behalf.
const workerAgent = "olp-worker"

// permanent reports whether a refresh failure means the grant can no longer
// be refreshed, which lapses it: the plugin reports that the upstream won't
// refresh it, or that it refreshes no grants; the plugin that enrolled it is
// no longer installed, or no longer approved, which only reinstalling it and
// enrolling again could undo; or the refresh authorizes another account.
// Every other failure is transient, including an unconfined plugin that the
// deployment's configuration or image does not serve: that may differ between
// replicas during a rolling deployment, and is undone by deploying again.
func permanent(failure error) bool {
	if reported, ok := errors.AsType[*abi.Error](failure); ok {
		return reported.Code == abi.CodeInvalidGrant || reported.Code == abi.CodeUnknownMethod
	}
	if refusal, ok := errors.AsType[*plugins.Error](failure); ok {
		return refusal.Code == plugins.CodeNotInstalled || refusal.Code == plugins.CodeNotApproved
	}
	return errors.Is(failure, errAnotherAccount)
}

// lapse records, in tx, that a grant lapsed for reason: it can no longer be
// refreshed (ADR 0008). The grant's refresh token is discarded and nothing
// refreshes it again, the lapse is audited with the worker as the actor, each
// notification rule subscribed to grant lapses is sent one delivery of it, and
// key authority advances, so gateways stop serving the grant's credential
// version within one authority poll. Lapse is terminal: only a new grant
// enrollment, which creates another credential version, replaces the grant. A
// grant that ended meanwhile, no longer holding the refresh token the worker
// read, is left as it is. lapse reports whether it lapsed the grant.
func lapse(ctx context.Context, tx pgx.Tx, g *dueGrant, reason string) (bool, error) {
	if err := serialize(ctx, tx); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_grants SET lapsed_at=now(),refresh_token_id=NULL,refresh_at=NULL,
		refresh_failures=refresh_failures+1,refresh_failure=$3,updated_at=now() WHERE credential_id=$1 AND refresh_token_id=$2`, g.credentialID, g.refreshTokenID, reason)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if err = ended(ctx, tx, g, "provider.grant.lapse"); err != nil {
		return false, err
	}
	return true, usage.NotifyGrantLapsed(ctx, tx, g.credentialID)
}

// retire retires, in tx, a due grant that no configuration uses any more
// (using): a superseded credential version that re-enrollment unbound, or
// one a draft enrolled before moving to another plugin build. Refreshing it
// would spend the upstream's refresh quota and keep an authorization alive
// that nothing serves. Like a lapse, retirement discards the grant's refresh
// token for good and makes its credential version ineligible, should a
// restored revision select it again, until a new grant enrollment; but since
// nothing served the grant, nobody is notified. It is audited, as the reason
// the version lapsed. retire reports whether it retired the grant: one that a
// configuration uses again is left due, for the next pass to refresh.
func retire(ctx context.Context, tx pgx.Tx, g *dueGrant) (bool, error) {
	if err := serialize(ctx, tx); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE olp.provider_grants g SET lapsed_at=now(),refresh_token_id=NULL,refresh_at=NULL,updated_at=now()
		FROM olp.provider_credentials c WHERE c.id=g.credential_id AND g.credential_id=$1 AND g.refresh_token_id=$2 AND `+using+` IS NULL`,
		g.credentialID, g.refreshTokenID)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	return true, ended(ctx, tx, g, "provider.grant.retire")
}

// serialize orders tx behind provider writes, which change what uses a
// credential version and revoke versions, and behind every other transaction
// that ends a grant, by holding the installation row as they do.
func serialize(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT FROM olp.installation WHERE singleton FOR UPDATE")
	return err
}

// ended completes, in tx, the end of a due grant whose lapsed_at is now set:
// its refresh token is deleted, action is audited with the worker as the
// actor, and key authority advances, so gateways stop serving the grant's
// credential version within one authority poll.
func ended(ctx context.Context, tx pgx.Tx, g *dueGrant, action string) error {
	if _, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", g.refreshTokenID, secrets.ProviderGrantRefresh); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.audit(id,action,resource_type,resource_id,outcome,user_agent_family)
		VALUES($1,$2,'provider_credential',$3,'success',$4)`, access.NewID(), action, g.credentialID, workerAgent); err != nil {
		return err
	}
	_, err := access.AdvanceAuthority(ctx, tx)
	return err
}

// Revoke ends the grant beneath a credential version as an operator revokes
// the version, in the revocation's transaction: a revoked version never
// serves again, so the grant's refresh token is deleted and nothing refreshes
// the grant again. A version without a grant has nothing to end.
func Revoke(ctx context.Context, tx pgx.Tx, credentialID string) error {
	var refreshTokenID *string
	err := tx.QueryRow(ctx, `UPDATE olp.provider_grants SET refresh_token_id=NULL,refresh_at=NULL,updated_at=now()
		WHERE credential_id=$1 RETURNING old.refresh_token_id::text`, credentialID).Scan(&refreshTokenID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && refreshTokenID == nil {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *refreshTokenID, secrets.ProviderGrantRefresh)
	return err
}
