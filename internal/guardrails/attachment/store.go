package attachment

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

const (
	columns = "a.id::text,a.scope,COALESCE(a.scope_id::text,'installation'),a.policy_revision_id::text,a.position"
	owner   = ` LEFT JOIN LATERAL (
 SELECT project_id,true AS found FROM olp.routes WHERE a.scope='route' AND id=a.scope_id
 UNION ALL SELECT project_id,true FROM olp.api_keys WHERE a.scope='api-key' AND id=a.scope_id
 UNION ALL SELECT project_id,true FROM olp.agent_publications WHERE a.scope='toolset' AND id=a.scope_id AND kind='toolset') o ON true`
	ownerColumns = "o.project_id::text,o.found IS NOT NULL"
)

func Authorize(ctx context.Context, q access.Queryer, p access.Principal, a guardrails.Attachment, need access.Need) error {
	t, err := locate(ctx, q, a)
	if err != nil {
		return err
	}
	return authorize(p, t, need)
}

func locate(ctx context.Context, q access.Queryer, a guardrails.Attachment) (target, error) {
	t := target{Attachment: a}
	if !t.owned() {
		return t, nil
	}
	err := q.QueryRow(ctx, "SELECT "+ownerColumns+" FROM (VALUES($1::text,$2::uuid)) a(scope,scope_id)"+owner, a.Scope, guardrails.ProjectPointer(a.ScopeID)).Scan(&t.owner, &t.found)
	return t, err
}

func lock(ctx context.Context, tx pgx.Tx, id string) (target, string, error) {
	var t target
	var etag string
	err := tx.QueryRow(ctx, "SELECT "+columns+",a.etag::text,"+ownerColumns+" FROM olp.guardrail_attachments a"+owner+" WHERE a.id=$1 FOR UPDATE OF a", id).
		Scan(&t.ID, &t.Scope, &t.ScopeID, &t.PolicyRevisionID, &t.Order, &etag, &t.owner, &t.found)
	return t, etag, err
}

type listed struct {
	guardrails.Attachment
	ETag             string  `json:"etag"`
	PolicyID         string  `json:"policy_id"`
	ActiveRevisionID *string `json:"active_revision_id"`
	owner            *string
	found            bool
}

func list(ctx context.Context, q access.Queryer) ([]listed, error) {
	rows, err := q.Query(ctx, "SELECT "+columns+",a.etag::text,v.policy_id::text,p.active_revision_id::text,"+ownerColumns+` FROM olp.guardrail_attachments a
 JOIN olp.guardrail_policy_revisions v ON v.id=a.policy_revision_id JOIN olp.guardrail_policies p ON p.id=v.policy_id`+owner+" ORDER BY a.position,a.id LIMIT 10001")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (listed, error) {
		var l listed
		err := row.Scan(&l.ID, &l.Scope, &l.ScopeID, &l.PolicyRevisionID, &l.Order, &l.ETag, &l.PolicyID, &l.ActiveRevisionID, &l.owner, &l.found)
		return l, err
	})
}

func (l listed) target() target {
	return target{Attachment: l.Attachment, owner: l.owner, found: l.found}
}

func requirePolicy(ctx context.Context, tx pgx.Tx, p access.Principal, t target) error {
	var project *string
	if err := tx.QueryRow(ctx, "SELECT p.project_id::text FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.policy_id=p.id WHERE v.id=$1 AND p.retired_at IS NULL", t.PolicyRevisionID).Scan(&project); err != nil {
		return access.Invalid("policy_revision_id", "Choose an existing policy revision.")
	}
	if err := p.Project(project, access.Change); err != nil {
		return err
	}
	ownedBy := func(id *string) bool { return project != nil && id != nil && *project == *id }
	switch {
	case t.Scope == guardrails.ProjectScope && !ownedBy(&t.ScopeID), t.owned() && !ownedBy(t.owner):
		return access.Invalid("scope_id", "The policy must belong to this project.")
	case t.Scope == guardrails.RouteScope && t.Order == 0:
		return refuseMigratedPosition(ctx, tx, t.ScopeID)
	}
	return nil
}

func refuseMigratedPosition(ctx context.Context, tx pgx.Tx, route string) error {
	var migrated bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.guardrail_policies p JOIN olp.routes r ON r.latest_revision_id=p.managed_route_revision_id WHERE r.id=$1)", route).Scan(&migrated); err != nil {
		return err
	}
	if migrated {
		return access.Invalid("order", "Position zero preserves this route's migrated content policy; choose a later position.")
	}
	return nil
}

func create(ctx context.Context, tx pgx.Tx, actor string, a guardrails.Attachment) (guardrails.Attachment, error) {
	created := guardrails.Attachment{ID: access.NewID(), Scope: a.Scope, ScopeID: a.ScopeID, PolicyRevisionID: a.PolicyRevisionID, Order: a.Order}
	_, err := tx.Exec(ctx, "INSERT INTO olp.guardrail_attachments(id,scope,scope_id,policy_revision_id,position,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)", created.ID, created.Scope, guardrails.ProjectPointer(created.ScopeID), created.PolicyRevisionID, created.Order, access.NewID(), actor)
	return created, err
}

func repoint(ctx context.Context, tx pgx.Tx, id, policyRevisionID string) (string, error) {
	etag := access.NewID()
	_, err := tx.Exec(ctx, "UPDATE olp.guardrail_attachments SET policy_revision_id=$2,etag=$3 WHERE id=$1", id, policyRevisionID, etag)
	return etag, err
}

func remove(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "DELETE FROM olp.guardrail_attachments WHERE id=$1", id)
	return err
}

func Place(ctx context.Context, tx pgx.Tx, actor string, a guardrails.Attachment) error {
	var id, current string
	err := tx.QueryRow(ctx, "SELECT id::text,policy_revision_id::text FROM olp.guardrail_attachments WHERE scope=$1 AND scope_id IS NOT DISTINCT FROM $2::uuid AND position=$3", a.Scope, guardrails.ProjectPointer(a.ScopeID), a.Order).Scan(&id, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err := create(ctx, tx, actor, a)
		return err
	}
	if err != nil || current == a.PolicyRevisionID {
		return err
	}
	if err := requireSamePolicy(ctx, tx, current, a.PolicyRevisionID, "An additive attachment cannot replace a different policy at this position."); err != nil {
		return err
	}
	_, err = repoint(ctx, tx, id, a.PolicyRevisionID)
	return err
}

func requireSamePolicy(ctx context.Context, tx pgx.Tx, current, next, refusal string) error {
	var same bool
	if err := tx.QueryRow(ctx, "SELECT a.policy_id=b.policy_id FROM olp.guardrail_policy_revisions a,olp.guardrail_policy_revisions b WHERE a.id=$1 AND b.id=$2", current, next).Scan(&same); err != nil {
		return err
	}
	if !same {
		return access.Invalid("guardrail_attachments", refusal)
	}
	return nil
}
