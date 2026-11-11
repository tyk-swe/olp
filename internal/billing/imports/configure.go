package imports

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing/source"
	"github.com/tyk-swe/olp/internal/secrets"
)

var (
	ErrCredentialRequired     = errors.New("a billing credential is required")
	ErrCredentialIncompatible = errors.New("the stored billing credential does not fit the billing source")
)

type Vault struct {
	Keys         *secrets.KeyRing
	Installation string
}

type Config struct {
	Enabled      bool
	Scope        source.Scope
	CredentialID *string
}

type Desired struct {
	Enabled    bool
	Scope      source.Scope
	Credential *source.Credential
}

func Read(ctx context.Context, q access.Queryer, providerID string) (Config, bool, error) {
	r, found, err := readRecord(ctx, q, providerID)
	if err != nil || !found {
		return Config{}, false, err
	}
	src, ok := source.Lookup(r.Source)
	if !ok {
		return Config{}, false, errors.New("unsupported billing source " + string(r.Source))
	}
	scope, err := src.Scope(r.Scope)
	if err != nil {
		return Config{}, false, err
	}
	return Config{Enabled: r.Enabled, Scope: scope, CredentialID: r.CredentialID}, true, nil
}

func (v Vault) Fits(ctx context.Context, q access.Queryer, src source.Source, credentialID *string) (bool, error) {
	_, fits, err := v.open(ctx, q, src, credentialID)
	return fits, err
}

func (v Vault) open(ctx context.Context, q access.Queryer, src source.Source, credentialID *string) (source.Credential, bool, error) {
	if credentialID == nil {
		return source.Credential{}, false, nil
	}
	stored, err := v.Keys.Read(ctx, q, v.Installation, *credentialID, secrets.BillingCredential)
	if errors.Is(err, pgx.ErrNoRows) {
		return source.Credential{}, false, nil
	}
	if err != nil {
		return source.Credential{}, false, err
	}
	credential, err := src.Stored(stored)
	return credential, err == nil, nil
}

func (v Vault) choose(ctx context.Context, tx pgx.Tx, src source.Source, current *string, desired *source.Credential) (string, error) {
	if desired == nil {
		return v.keep(ctx, tx, src, current)
	}
	if stored, fits, err := v.open(ctx, tx, src, current); err == nil && fits && stored == *desired {
		return *current, nil
	}
	sealed, err := desired.Seal()
	if err != nil {
		return "", err
	}
	id := access.NewID()
	return id, v.Keys.Store(ctx, tx, v.Installation, id, secrets.BillingCredential, sealed, nil)
}

func (v Vault) keep(ctx context.Context, tx pgx.Tx, src source.Source, current *string) (string, error) {
	fits, err := v.Fits(ctx, tx, src, current)
	switch {
	case err != nil:
		return "", err
	case current == nil:
		return "", ErrCredentialRequired
	case !fits:
		return "", ErrCredentialIncompatible
	}
	return *current, nil
}

func Save(ctx context.Context, tx pgx.Tx, vault Vault, providerID string, desired Desired, createdBy string) (string, error) {
	existing, found, err := lockRecord(ctx, tx, providerID)
	if err != nil {
		return "", err
	}
	current := existing.CredentialID
	credentialID, err := vault.choose(ctx, tx, desired.Scope.Source(), current, desired.Credential)
	if err != nil {
		return "", err
	}
	etag := access.NewID()
	if _, err := tx.Exec(ctx, `INSERT INTO olp.provider_billing_imports
		(provider_id,source,credential_id,scope,enabled,etag,created_by,next_import_at)
		VALUES($1::uuid,$2,$3::uuid,$4::jsonb,$5,$6,$7::uuid,now())
		ON CONFLICT (provider_id) DO UPDATE SET source=EXCLUDED.source,credential_id=EXCLUDED.credential_id,
			scope=EXCLUDED.scope,enabled=EXCLUDED.enabled,etag=EXCLUDED.etag,updated_at=now()`,
		providerID, desired.Scope.Source().Kind(), credentialID, desired.Scope.JSON(), desired.Enabled, etag, createdBy); err != nil {
		return "", err
	}
	if found && current != nil && *current != credentialID {
		if err := deleteSecret(ctx, tx, *current); err != nil {
			return "", err
		}
	}
	return etag, syncRevision(ctx, tx, providerID, desired.Scope)
}

func Remove(ctx context.Context, tx pgx.Tx, providerID string) error {
	existing, found, err := lockRecord(ctx, tx, providerID)
	if err != nil || !found {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.provider_billing_contexts bc SET
		imported_from=bi.imported_from,imported_through=bi.imported_through,last_success_at=bi.last_success_at
		FROM olp.provider_billing_imports bi WHERE bi.provider_id=$1::uuid AND bc.provider_id=bi.provider_id`, providerID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM olp.provider_billing_imports WHERE provider_id=$1::uuid`, providerID); err != nil {
		return err
	}
	if existing.CredentialID == nil {
		return nil
	}
	return deleteSecret(ctx, tx, *existing.CredentialID)
}

func deleteSecret(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `DELETE FROM olp.secrets WHERE id=$1::uuid AND purpose=$2`, id, secrets.BillingCredential)
	return err
}

func syncRevision(ctx context.Context, tx pgx.Tx, providerID string, scope source.Scope) error {
	kind := scope.Source().Kind()
	if _, err := tx.Exec(ctx, `INSERT INTO olp.provider_billing_contexts(provider_id,source,scope)
		VALUES($1::uuid,$2,$3::jsonb) ON CONFLICT (provider_id) DO NOTHING`, providerID, kind, scope.JSON()); err != nil {
		return err
	}
	var previousKind source.Kind
	var previousScope []byte
	if err := tx.QueryRow(ctx, `SELECT source,scope FROM olp.provider_billing_contexts
		WHERE provider_id=$1::uuid FOR UPDATE`, providerID).Scan(&previousKind, &previousScope); err != nil {
		return err
	}
	if !sameScope(previousKind, previousScope, scope) {
		return resetCoverage(ctx, tx, providerID, scope)
	}
	if err := refreshCoverage(ctx, tx, providerID, kind, scope.JSON()); err != nil {
		return err
	}
	digest := scope.Digest()
	_, err := tx.Exec(ctx, `UPDATE olp.provider_billing_imports bi SET
		last_success_at=COALESCE(bi.last_success_at,(SELECT last_success_at FROM olp.provider_billing_contexts WHERE provider_id=$1::uuid)),
		source_revision=(SELECT source_revision FROM olp.provider_billing_revisions
			WHERE provider_id=$1::uuid AND source=$2 AND scope_digest=$3 AND covered_from IS NOT NULL
			ORDER BY ordering_at DESC,period_start DESC LIMIT 1)
		WHERE bi.provider_id=$1::uuid`, providerID, kind, digest[:])
	return err
}

func sameScope(previousKind source.Kind, previousScope []byte, scope source.Scope) bool {
	src, ok := source.Lookup(previousKind)
	if !ok || src.Kind() != scope.Source().Kind() {
		return false
	}
	previous, err := src.Scope(previousScope)
	return err == nil && previous.Digest() == scope.Digest()
}

func resetCoverage(ctx context.Context, tx pgx.Tx, providerID string, scope source.Scope) error {
	if _, err := tx.Exec(ctx, `UPDATE olp.provider_billing_contexts SET source=$2,scope=$3::jsonb,
		imported_from=NULL,imported_through=NULL,last_success_at=NULL WHERE provider_id=$1::uuid`,
		providerID, scope.Source().Kind(), scope.JSON()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.provider_billing_revisions SET covered_from=NULL,covered_through=NULL
		WHERE provider_id=$1::uuid`, providerID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE olp.provider_billing_imports SET
		imported_from=NULL,imported_through=NULL,source_revision=NULL,last_success_at=NULL,
		resume_period=NULL,resume_remaining=0 WHERE provider_id=$1::uuid`, providerID)
	return err
}
