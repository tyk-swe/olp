package database

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func CompleteInstallationSetup(ctx context.Context, tx pgx.Tx, name string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.installation SET name=$1,setup_complete=true WHERE singleton", name)
	return err
}

func BindInstallationKeys(ctx context.Context, tx pgx.Tx, authFingerprint []byte, activeKeyVersion int) error {
	_, err := tx.Exec(ctx, "UPDATE olp.installation SET auth_fingerprint=$1,active_key_version=$2 WHERE singleton", authFingerprint, activeKeyVersion)
	return err
}

func SetInstallationBranding(ctx context.Context, tx pgx.Tx, name, logo, accent, etag string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.installation SET name=$1,logo=$2,accent=$3,branding_etag=$4 WHERE singleton", name, logo, accent, etag)
	return err
}

func SetInstallationBudgetPolicy(ctx context.Context, tx pgx.Tx, id string, encodedPolicy []byte, etag string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.installation SET budget_policy=NULLIF($2::jsonb,'null'::jsonb),budget_etag=$3 WHERE id=$1", id, encodedPolicy, etag)
	return err
}

func AdvanceInstallationAuthority(ctx context.Context, tx pgx.Tx, authorityID string) (string, int64, error) {
	var id string
	var sequence int64
	err := tx.QueryRow(ctx, "UPDATE olp.installation SET authority_id=$1,authority_sequence=authority_sequence+1 WHERE singleton RETURNING authority_id::text,authority_sequence", authorityID).Scan(&id, &sequence)
	return id, sequence, err
}

func NextReleaseSequence(ctx context.Context, tx pgx.Tx) (int64, error) {
	var sequence int64
	err := tx.QueryRow(ctx, "UPDATE olp.installation SET release_sequence=release_sequence+1 WHERE singleton RETURNING release_sequence").Scan(&sequence)
	return sequence, err
}
