package sinks

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Spec struct {
	ProjectID   *string
	Name        string
	Type        string
	Destination string
	Streams     []string
	Enabled     bool
}

func NormalizeSpec(spec Spec, policy *egress.Policy) (Spec, error) {
	if spec.Type != "https" {
		return Spec{}, access.Invalid("type", "Use the HTTPS JSON sink type.")
	}
	in := input{Name: spec.Name, Destination: spec.Destination, Streams: spec.Streams, Enabled: &spec.Enabled}
	if err := validateAndNormalize(&in, policy, spec.ProjectID); err != nil {
		return Spec{}, err
	}
	return Spec{ProjectID: spec.ProjectID, Name: in.Name, Type: spec.Type, Destination: in.Destination, Streams: in.Streams, Enabled: spec.Enabled}, nil
}

func Insert(ctx context.Context, tx pgx.Tx, id string, spec Spec, etag, actor string) error {
	_, err := tx.Exec(ctx, "INSERT INTO olp.managed_export_sinks(id,project_id,name,type,destination,streams,enabled,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", id, spec.ProjectID, spec.Name, spec.Type, spec.Destination, spec.Streams, spec.Enabled, etag, actor)
	return nameTaken(err)
}

func Update(ctx context.Context, tx pgx.Tx, id string, spec Spec, etag string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.managed_export_sinks SET name=$2,destination=$3,streams=$4,enabled=$5,etag=$6 WHERE id=$1", id, spec.Name, spec.Destination, spec.Streams, spec.Enabled, etag)
	return nameTaken(err)
}

func nameTaken(err error) error {
	var problem *pgconn.PgError
	if errors.As(err, &problem) && problem.Code == "23505" {
		return access.Fail(409, "sink_name_exists", "Use a unique active sink name inside its scope.")
	}
	return err
}

type Credentials struct {
	Keys         *secrets.KeyRing
	Installation string
}

func (c Credentials) Matches(ctx context.Context, q secrets.RowQuerier, id, secret string) (bool, error) {
	value, err := c.Keys.Read(ctx, q, c.Installation, id, secrets.SinkCredential)
	if err != nil {
		return false, err
	}
	defer clear(value)
	return subtle.ConstantTimeCompare(value, []byte(secret)) == 1, nil
}

func (c Credentials) Replace(ctx context.Context, tx pgx.Tx, sinkID string, secret *string) error {
	var previous *string
	if err := tx.QueryRow(ctx, "SELECT credential_id::text FROM olp.managed_export_sinks WHERE id=$1", sinkID).Scan(&previous); err != nil {
		return err
	}
	next, err := c.store(ctx, tx, secret)
	if err != nil {
		return err
	}
	if _, updateErr := tx.Exec(ctx, "UPDATE olp.managed_export_sinks SET credential_id=$2 WHERE id=$1", sinkID, next); updateErr != nil {
		return updateErr
	}
	if previous == nil {
		return nil
	}
	_, deleteErr := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *previous, secrets.SinkCredential)
	return deleteErr
}

func (c Credentials) store(ctx context.Context, tx pgx.Tx, secret *string) (*string, error) {
	if secret == nil {
		return nil, nil
	}
	id := access.NewID()
	material := []byte(*secret)
	defer clear(material)
	if err := c.Keys.Store(ctx, tx, c.Installation, id, secrets.SinkCredential, material, nil); err != nil {
		return nil, err
	}
	return &id, nil
}
