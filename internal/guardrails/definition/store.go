package definition

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type Definition struct {
	Configuration    json.RawMessage       `json:"configuration"`
	TimeoutMS        int64                 `json:"timeout_ms"`
	OnError          guardrails.OnError    `json:"on_error"`
	ActiveRevisionID *string               `json:"active_revision_id"`
	ID               string                `json:"id"`
	ProjectID        string                `json:"project_id"`
	Name             string                `json:"name"`
	Type             guardrails.Kind       `json:"type"`
	ETag             string                `json:"etag"`
	RevisionID       string                `json:"revision_id"`
	Revision         int64                 `json:"revision"`
	Policy           *contentpolicy.Policy `json:"policy"`
	RetiredAt        *time.Time            `json:"retired_at"`
}

type Saved struct{ ID, RevisionID string }

const selected = "SELECT g.id::text,COALESCE(g.project_id::text,'installation'),g.name,g.type,g.etag::text,v.id::text,v.revision,v.policy,g.retired_at,v.configuration,v.timeout_ms,v.on_error,g.active_revision_id::text FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.id=g.latest_revision_id"

func scan(row pgx.Row) (Definition, error) {
	var value Definition
	var policy []byte
	if err := row.Scan(&value.ID, &value.ProjectID, &value.Name, &value.Type, &value.ETag, &value.RevisionID, &value.Revision, &policy, &value.RetiredAt, &value.Configuration, &value.TimeoutMS, &value.OnError, &value.ActiveRevisionID); err != nil {
		return Definition{}, err
	}
	return value, json.Unmarshal(policy, &value.Policy)
}

func load(ctx context.Context, q access.Queryer, id string) (Definition, error) {
	return scan(q.QueryRow(ctx, selected+" WHERE g.id=$1", id))
}

func lock(ctx context.Context, tx pgx.Tx, id string) (Definition, error) {
	return scan(tx.QueryRow(ctx, selected+" WHERE g.id=$1 FOR UPDATE OF g", id))
}

func FindActive(ctx context.Context, q access.Queryer, projectID, name string) (*Definition, error) {
	value, err := scan(q.QueryRow(ctx, selected+" WHERE g.project_id IS NOT DISTINCT FROM $1::uuid AND lower(g.name)=lower($2) AND g.retired_at IS NULL", guardrails.ProjectPointer(projectID), name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func Save(ctx context.Context, tx pgx.Tx, actor string, current *Definition, d Draft) (Saved, error) {
	saved, revision, err := writeHead(ctx, tx, actor, current, d)
	if err != nil {
		return Saved{}, err
	}
	policy, err := json.Marshal(storedPolicy(d.legacy))
	if err != nil {
		return Saved{}, err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO olp.guardrail_revisions(id,guardrail_id,revision,policy,created_by,configuration,timeout_ms,on_error) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", saved.RevisionID, saved.ID, revision, policy, actor, d.configuration, d.timeout, d.onError); err != nil {
		return Saved{}, err
	}
	if d.legacy == nil {
		return saved, nil
	}
	return saved, Activate(ctx, tx, actor, saved.ID, saved.RevisionID)
}

func storedPolicy(legacy *contentpolicy.Policy) *contentpolicy.Policy {
	if legacy == nil {
		return &contentpolicy.Policy{Rules: []contentpolicy.Rule{}}
	}
	return legacy
}

func writeHead(ctx context.Context, tx pgx.Tx, actor string, current *Definition, d Draft) (Saved, int64, error) {
	saved := Saved{ID: access.NewID(), RevisionID: access.NewID()}
	if current == nil {
		_, err := tx.Exec(ctx, "INSERT INTO olp.guardrails(id,project_id,name,type,latest_revision_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)", saved.ID, guardrails.ProjectPointer(d.project), d.name, d.kind, saved.RevisionID, access.NewID(), actor)
		return saved, 1, err
	}
	if current.ProjectID != d.project || current.Type != d.kind {
		return Saved{}, 0, access.Invalid("guardrail", "The owning boundary and type are immutable.")
	}
	saved.ID = current.ID
	_, err := tx.Exec(ctx, "UPDATE olp.guardrails SET name=$2,latest_revision_id=$3,etag=$4 WHERE id=$1", saved.ID, d.name, saved.RevisionID, access.NewID())
	return saved, current.Revision + 1, err
}

func retire(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.guardrails SET retired_at=now(),etag=$2 WHERE id=$1", id, access.NewID())
	return err
}

func Activate(ctx context.Context, tx pgx.Tx, actor, id, revisionID string) error {
	tag, err := tx.Exec(ctx, "UPDATE olp.guardrails SET active_revision_id=$2,etag=$3 WHERE id=$1 AND EXISTS(SELECT 1 FROM olp.guardrail_revisions WHERE id=$2 AND guardrail_id=$1)", id, revisionID, access.NewID())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return guardrails.ActivationRefused()
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_revision_activations(revision_id,activated_by) VALUES($1,$2) ON CONFLICT DO NOTHING", revisionID, actor)
	return err
}

type lineage struct{}

func (lineage) Lock(ctx context.Context, tx pgx.Tx, id string) (guardrails.Head, error) {
	var head guardrails.Head
	err := tx.QueryRow(ctx, "SELECT COALESCE(project_id::text,'installation'),etag::text,retired_at IS NOT NULL FROM olp.guardrails WHERE id=$1 FOR UPDATE", id).Scan(&head.Project, &head.ETag, &head.Retired)
	return head, err
}

func (lineage) Activate(ctx context.Context, tx pgx.Tx, actor, id, revisionID string) error {
	return Activate(ctx, tx, actor, id, revisionID)
}
