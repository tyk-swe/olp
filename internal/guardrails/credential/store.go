package credential

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

type New struct {
	ProjectID *string
	Name      string
	Secret    []byte
	CreatedBy string
}

func Create(ctx context.Context, tx pgx.Tx, keys *secrets.KeyRing, installation string, in New) (string, error) {
	id := access.NewID()
	if err := keys.Store(ctx, tx, installation, id, secrets.GuardrailCredential, in.Secret, nil); err != nil {
		return "", err
	}
	_, err := tx.Exec(ctx, "INSERT INTO olp.guardrail_credentials(id,project_id,name,created_by) VALUES($1,$2,$3,$4)", id, in.ProjectID, in.Name, in.CreatedBy)
	return id, err
}

func FindID(ctx context.Context, q access.Queryer, projectID *string, name string) (string, bool, error) {
	var id string
	err := q.QueryRow(ctx, "SELECT id::text FROM olp.guardrail_credentials WHERE project_id IS NOT DISTINCT FROM $1::uuid AND name=$2", projectID, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func Name(ctx context.Context, q access.Queryer, id string) (string, error) {
	var name string
	err := q.QueryRow(ctx, "SELECT name FROM olp.guardrail_credentials WHERE id=$1", id).Scan(&name)
	return name, err
}

func Load(ctx context.Context, q access.Queryer, keys *secrets.KeyRing, installation, project, id string) ([]byte, error) {
	var owner string
	if err := q.QueryRow(ctx, "SELECT project_id::text FROM olp.guardrail_credentials WHERE id=$1", id).Scan(&owner); err != nil || owner != project {
		return nil, errors.New("guardrail credential boundary mismatch")
	}
	secret, err := keys.Read(ctx, q, installation, id, secrets.GuardrailCredential)
	if err != nil {
		return nil, errors.New("guardrail credential unavailable")
	}
	return secret, nil
}

func lock(ctx context.Context, tx pgx.Tx, id string) (string, error) {
	var project string
	err := tx.QueryRow(ctx, "SELECT project_id::text FROM olp.guardrail_credentials WHERE id=$1 FOR UPDATE", id).Scan(&project)
	return project, err
}

func referenced(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.guardrail_revisions WHERE CASE WHEN pg_input_is_valid(configuration->>'credential_ref','uuid') THEN (configuration->>'credential_ref')::uuid END=$1::uuid OR CASE WHEN pg_input_is_valid(configuration->'network'->>'credential_id','uuid') THEN (configuration->'network'->>'credential_id')::uuid END=$1::uuid)", id).Scan(&found)
	return found, err
}

func remove(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx, "DELETE FROM olp.guardrail_credentials WHERE id=$1", id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", id, secrets.GuardrailCredential)
	return err
}
