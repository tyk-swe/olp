package access

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/secrets"
)

const authenticationCapabilitiesSQL = `SELECT COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.local_login_enabled'),true),
 COALESCE((SELECT (document->>'enabled')::boolean FROM olp.oidc_configuration WHERE singleton),false),
 COALESCE((SELECT (document->>'enabled')::boolean FROM olp.saml_configuration WHERE singleton),false),
 COALESCE((SELECT enabled FROM olp.capture_configuration WHERE singleton),false)`

func SecretValueFor(ctx context.Context, tx pgx.Tx, keys *secrets.KeyRing, purpose secrets.SealPurpose, installation, id string) ([]byte, error) {
	return keys.Read(ctx, tx, installation, id, purpose)
}
