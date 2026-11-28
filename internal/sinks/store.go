package sinks

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type Definition struct {
	ID            string        `json:"id"`
	ProjectID     *string       `json:"project_id"`
	Name          string        `json:"name"`
	Type          string        `json:"type"`
	Destination   string        `json:"destination"`
	Streams       []string      `json:"streams"`
	Enabled       bool          `json:"enabled"`
	HasCredential bool          `json:"has_credential"`
	ETag          string        `json:"etag"`
	RetiredAt     *time.Time    `json:"retired_at"`
	Delivery      DeliveryState `json:"delivery"`
}

type DeliveryState struct {
	Delivered       int64      `json:"delivered"`
	Failed          int64      `json:"failed"`
	Expired         int64      `json:"expired"`
	LastDeliveredAt *time.Time `json:"last_delivered_at"`
}

const columns = "id::text,project_id::text,name,type,destination,streams,enabled,credential_id IS NOT NULL,etag::text,retired_at,delivered_total,failed_total,expired_total,last_delivered_at"

func scan(row pgx.Row) (*Definition, error) {
	var d Definition
	err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Type, &d.Destination, &d.Streams, &d.Enabled, &d.HasCredential, &d.ETag, &d.RetiredAt, &d.Delivery.Delivered, &d.Delivery.Failed, &d.Delivery.Expired, &d.Delivery.LastDeliveredAt)
	return &d, err
}

func load(ctx context.Context, q database.Queryer, id string, lock bool) (*Definition, error) {
	query := "SELECT " + columns + " FROM olp.managed_export_sinks WHERE id=$1"
	if lock {
		query += " FOR NO KEY UPDATE"
	}
	return scan(q.QueryRow(ctx, query, id))
}

func listActive(ctx context.Context, q database.Queryer, page database.Pagination, allProjects bool, projects []string) ([]*Definition, error) {
	rows, err := q.Query(ctx, "SELECT "+columns+" FROM olp.managed_export_sinks WHERE retired_at IS NULL AND id<$1 AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4", page.Before, allProjects, projects, page.Limit+1)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Definition, error) { return scan(row) })
}

func countActive(ctx context.Context, q database.Queryer) (int, error) {
	var count int
	err := q.QueryRow(ctx, "SELECT count(*) FROM olp.managed_export_sinks WHERE retired_at IS NULL").Scan(&count)
	return count, err
}

type Spec struct {
	ProjectID   *string
	Name        string
	Type        string
	Destination string
	Streams     []string
	Enabled     bool
}

func Insert(ctx context.Context, tx pgx.Tx, id string, spec Spec, etag, actor string) error {
	_, err := tx.Exec(ctx, "INSERT INTO olp.managed_export_sinks(id,project_id,name,type,destination,streams,enabled,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", id, spec.ProjectID, spec.Name, spec.Type, spec.Destination, spec.Streams, spec.Enabled, etag, actor)
	return nameTaken(err)
}

func Update(ctx context.Context, tx pgx.Tx, id string, spec Spec, etag string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.managed_export_sinks SET name=$2,destination=$3,streams=$4,enabled=$5,etag=$6 WHERE id=$1", id, spec.Name, spec.Destination, spec.Streams, spec.Enabled, etag)
	return nameTaken(err)
}

func markRetired(ctx context.Context, tx pgx.Tx, id, etag string) error {
	if _, err := tx.Exec(ctx, "UPDATE olp.managed_export_sinks SET enabled=false,retired_at=now(),etag=$2 WHERE id=$1", id, etag); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE olp.managed_export_deliveries SET status='cancelled',lease=NULL WHERE sink_id=$1 AND status='pending'", id)
	return err
}

func nameTaken(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return problem.Fail(409, "sink_name_exists", "Use a unique active sink name inside its scope.")
	}
	return err
}

type ActiveSink struct {
	ID           string
	CredentialID *string
	Name         string
	Project      *string
	Destination  string
	Streams      []string
	Enabled      bool
}

func ReadActiveSinks(ctx context.Context, q database.Queryer) ([]ActiveSink, error) {
	rows, err := q.Query(ctx, "SELECT s.id::text,s.credential_id::text,s.name,p.name,s.destination,s.streams,s.enabled FROM olp.managed_export_sinks s LEFT JOIN olp.projects p ON p.id=s.project_id WHERE s.retired_at IS NULL")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ActiveSink])
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
	return secrets.Delete(ctx, tx, secrets.SinkCredential, *previous)
}

func (c Credentials) store(ctx context.Context, tx pgx.Tx, secret *string) (*string, error) {
	if secret == nil {
		return nil, nil
	}
	id := ids.New()
	material := []byte(*secret)
	defer clear(material)
	if err := c.Keys.Store(ctx, tx, c.Installation, id, secrets.SinkCredential, material, nil); err != nil {
		return nil, err
	}
	return &id, nil
}
