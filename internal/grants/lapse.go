package grants

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// workerAgent is the user agent family of what a worker records in audit,
// where it acts on no user's or management token's behalf.
const workerAgent = "olp-worker"

// permanent reports whether a refresh failure means the grant can no longer
// be refreshed, which lapses it: the plugin reports that the upstream won't
// refresh it, or that it refreshes no grants, or the refresh authorizes
// another account. Every other failure is transient.
func permanent(failure error) bool {
	if reported, ok := errors.AsType[*abi.Error](failure); ok {
		return reported.Code == abi.CodeInvalidGrant || reported.Code == abi.CodeUnknownMethod
	}
	return errors.Is(failure, errAnotherAccount)
}

// lapse records, in tx, that a grant lapsed for reason: it can no longer be
// refreshed (ADR 0006). The grant's refresh token is discarded and nothing
// refreshes it again, the lapse is audited with the worker as the actor, each
// notification rule subscribed to grant lapses is sent one delivery of it, and
// key authority advances, so gateways stop serving the grant's credential
// version within one authority poll. Lapse is terminal: only a new grant
// enrollment, which creates another credential version, replaces the grant.
func lapse(ctx context.Context, tx pgx.Tx, g *dueGrant, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE olp.provider_grants SET lapsed_at=now(),refresh_token_id=NULL,refresh_at=NULL,
		refresh_failures=refresh_failures+1,refresh_failure=$2,updated_at=now() WHERE credential_id=$1`, g.credentialID, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", g.refreshTokenID, RefreshPurpose); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.audit(id,action,resource_type,resource_id,outcome,user_agent_family)
		VALUES($1,'provider.grant.lapse','provider_credential',$2,'success',$3)`, access.NewID(), g.credentialID, workerAgent); err != nil {
		return err
	}
	if err := usage.NotifyGrantLapsed(ctx, tx, g.credentialID); err != nil {
		return err
	}
	_, err := access.AdvanceAuthority(ctx, tx)
	return err
}
