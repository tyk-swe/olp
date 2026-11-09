package policy

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type Policy struct {
	ID               string             `json:"id"`
	RevisionID       string             `json:"revision_id"`
	ProjectID        string             `json:"project_id"`
	Name             string             `json:"name"`
	Revision         int64              `json:"revision"`
	Entries          []guardrails.Entry `json:"entries"`
	ETag             string             `json:"etag"`
	ActiveRevisionID *string            `json:"active_revision_id"`
	RetiredAt        *time.Time         `json:"retired_at"`
}

func (p Policy) MarshalJSON() ([]byte, error) {
	type plain Policy
	return json.Marshal(struct {
		plain
		PolicyID string `json:"policy_id"`
	}{plain(p), p.ID})
}

type Saved struct{ ID, RevisionID string }

const selected = `SELECT p.id::text,v.id::text,COALESCE(p.project_id::text,'installation'),p.name,v.revision,v.entries,p.etag::text,p.active_revision_id::text,p.retired_at
 FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.id=p.latest_revision_id`

func scan(row pgx.Row) (Policy, error) {
	var p Policy
	err := row.Scan(&p.ID, &p.RevisionID, &p.ProjectID, &p.Name, &p.Revision, &p.Entries, &p.ETag, &p.ActiveRevisionID, &p.RetiredAt)
	return p, err
}

func load(ctx context.Context, q access.Queryer, id string) (Policy, error) {
	return scan(q.QueryRow(ctx, selected+" WHERE p.id=$1", id))
}

func lock(ctx context.Context, tx pgx.Tx, id string) (Policy, error) {
	return scan(tx.QueryRow(ctx, selected+" WHERE p.id=$1 FOR UPDATE OF p", id))
}

func FindLatest(ctx context.Context, q access.Queryer, projectID *string, name string) (*Policy, error) {
	p, err := scan(q.QueryRow(ctx, selected+" WHERE p.project_id IS NOT DISTINCT FROM $1::uuid AND lower(p.name)=lower($2) AND p.retired_at IS NULL", projectID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func Save(ctx context.Context, tx pgx.Tx, actor string, current *Policy, d Draft) (Saved, error) {
	entries, err := json.Marshal(d.entries)
	if err != nil {
		return Saved{}, err
	}
	saved, revision, err := writeHead(ctx, tx, actor, current, d)
	if err != nil {
		return Saved{}, err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_policy_revisions(id,policy_id,revision,entries,created_by) VALUES($1,$2,$3,$4,$5)", saved.RevisionID, saved.ID, revision, entries, actor)
	return saved, err
}

func writeHead(ctx context.Context, tx pgx.Tx, actor string, current *Policy, d Draft) (Saved, int64, error) {
	if current == nil {
		saved := Saved{ID: access.NewID(), RevisionID: access.NewID()}
		_, err := tx.Exec(ctx, "INSERT INTO olp.guardrail_policies(id,project_id,name,latest_revision_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)", saved.ID, guardrails.ProjectPointer(d.project), d.name, saved.RevisionID, access.NewID(), actor)
		return saved, 1, err
	}
	saved := Saved{ID: current.ID, RevisionID: access.NewID()}
	_, err := tx.Exec(ctx, "UPDATE olp.guardrail_policies SET name=$2,latest_revision_id=$3,etag=$4 WHERE id=$1", saved.ID, d.name, saved.RevisionID, access.NewID())
	return saved, current.Revision + 1, err
}

func Activate(ctx context.Context, tx pgx.Tx, actor, policyID, revisionID string) error {
	tag, err := tx.Exec(ctx, "UPDATE olp.guardrail_policies SET active_revision_id=$2,etag=$3 WHERE id=$1 AND EXISTS(SELECT 1 FROM olp.guardrail_policy_revisions WHERE id=$2 AND policy_id=$1)", policyID, revisionID, access.NewID())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return guardrails.ActivationRefused()
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_policy_revision_activations(revision_id,activated_by) VALUES($1,$2) ON CONFLICT DO NOTHING", revisionID, actor)
	return err
}

func retire(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.guardrail_policies SET retired_at=now(),etag=$2 WHERE id=$1", id, access.NewID())
	return err
}

func revisionOf(ctx context.Context, q access.Queryer, revisionID string) (guardrails.PolicyRevision, error) {
	var r guardrails.PolicyRevision
	err := q.QueryRow(ctx, "SELECT v.id::text,p.id::text,COALESCE(p.project_id::text,'installation'),p.name,v.revision,v.entries FROM olp.guardrail_policy_revisions v JOIN olp.guardrail_policies p ON p.id=v.policy_id WHERE v.id=$1", revisionID).
		Scan(&r.ID, &r.PolicyID, &r.ProjectID, &r.Name, &r.Revision, &r.Entries)
	return r, err
}

func anyRetired(ctx context.Context, q access.Queryer, revisionIDs []string) (bool, error) {
	var retired bool
	err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.guardrail_revisions v JOIN olp.guardrails g ON g.id=v.guardrail_id WHERE v.id=ANY($1::uuid[]) AND g.retired_at IS NOT NULL)", revisionIDs).Scan(&retired)
	return retired, err
}

type lineage struct{}

func (lineage) Lock(ctx context.Context, tx pgx.Tx, id string) (guardrails.Head, error) {
	p, err := lock(ctx, tx, id)
	return guardrails.Head{Project: p.ProjectID, ETag: p.ETag, Retired: p.RetiredAt != nil}, err
}

func (lineage) Activate(ctx context.Context, tx pgx.Tx, actor, id, revisionID string) error {
	return Activate(ctx, tx, actor, id, revisionID)
}
