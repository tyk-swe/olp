package providers

import (
	"context"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/access"
)

// credentialBoundary includes inputs that can change where or how a secret
// is sent. Credential IDs are excluded so rotation preserves the boundary.
func (c *Configuration) credentialBoundary() string {
	encoded, _ := json.Marshal([]any{c.Kind, c.AuthMode, c.Endpoint, c.CloudRegion, c.CloudProject,
		c.CredentialSource, c.ProfileID, c.ProfileRevision, c.Options.CredentialHeaders,
		c.credentialNetwork(), c.Options.PluginOptions})
	return string(encoded)
}

func (c *Configuration) credentialNetwork() [2]string {
	if c.Options.Network == nil {
		return [2]string{}
	}
	return [2]string{c.Options.Network.ProxyURL, c.Options.Network.TrustRootsPEM}
}

// PreserveCredentialBoundary keeps every selectable credential version bound
// to its destination across API edits, imports and restores. Detached versions
// still count: a draft can select them again. A grant may move to another
// build or profile because each grant checks that identity before use.
func PreserveCredentialBoundary(ctx context.Context, q access.Queryer, providerID string, current, next *Configuration) error {
	if current.credentialBoundary() == next.credentialBoundary() {
		return nil
	}
	// Profile provenance permits changing only the enrolling identity while
	// old grants remain available to their pinned revisions. It must not
	// permit moving an existing grant's destination by cycling A -> B -> A.
	sameIdentity := *next
	sameIdentity.ProfileID, sameIdentity.ProfileRevision = current.ProfileID, current.ProfileRevision
	grantMigration := current.Grant() && next.Grant() && current.credentialBoundary() == sameIdentity.credentialBoundary()
	if grantMigration {
		// The revision is the destination: a grant build binds its upstream
		// and authority origins into the manifest, so a migration may only
		// land on a build declaring the same origins. A missing build or
		// different origins is a destination change, not a migration.
		if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT a.manifest::jsonb->'origins' FROM olp.plugins a WHERE a.digest=$1)
			= (SELECT b.manifest::jsonb->'origins' FROM olp.plugins b WHERE b.digest=$2), false)`,
			current.ProfileRevision, next.ProfileRevision).Scan(&grantMigration); err != nil {
			return err
		}
	}
	networkChanged := current.credentialNetwork() != next.credentialNetwork() || value(current.Endpoint) != value(next.Endpoint)
	var stored bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_credentials
		WHERE provider_id=$1 AND revoked_at IS NULL AND (NOT $2 OR plugin_digest IS NULL))
		OR ($3 AND EXISTS(SELECT 1 FROM olp.provider_network_credentials WHERE provider_id=$1 AND revoked_at IS NULL))`,
		providerID, grantMigration, networkChanged).Scan(&stored); err != nil {
		return err
	}
	if stored {
		return access.Fail(422, "credential_destination_changed", "This provider owns unrevoked credential versions bound to its authentication, endpoint, network and profile. Create a new provider or revoke every old credential before supplying credentials for a different destination.")
	}
	return nil
}
