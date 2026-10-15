package guardrails

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
)

// Save appends an immutable definition under the caller's serialized mutation
// transaction. HTTP and configuration promotion share this storage path.
func Save(ctx context.Context, tx pgx.Tx, actor string, current, next *Definition) error {
	legacy := len(next.Configuration) == 0
	if next.TimeoutMS == 0 {
		next.TimeoutMS = 1000
	}
	if next.OnError == "" {
		next.OnError = FailClosed
	}
	if legacy {
		if err := contentpolicy.Validate(next.Policy); err != nil || next.Policy == nil {
			return access.Invalid("policy", "Supply a valid bounded content policy.")
		}
		if next.Policy.Rules == nil {
			next.Policy.Rules = []contentpolicy.Rule{}
		}
		patterns := []map[string]any{}
		for _, r := range next.Policy.Rules {
			patterns = append(patterns, map[string]any{"id": r.ID, "pattern": r.Pattern, "replacement": r.Replacement})
		}
		next.Configuration, _ = json.Marshal(map[string]any{"rules": patterns})
	}
	if err := ValidateDefinition(Revision{Type: next.Type, Configuration: next.Configuration, TimeoutMS: next.TimeoutMS, OnError: next.OnError}); err != nil {
		return access.Invalid("configuration", "Supply valid typed guardrail configuration, timeout and failure policy.")
	}

	next.ETag, next.RevisionID, next.Revision = access.NewID(), access.NewID(), 1
	var err error
	if current == nil {
		next.ID = access.NewID()
		_, err = tx.Exec(ctx, "INSERT INTO olp.guardrails(id,project_id,name,type,latest_revision_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)", next.ID, projectPointer(next.ProjectID), next.Name, next.Type, next.RevisionID, next.ETag, actor)
	} else {
		if current.ProjectID != next.ProjectID || current.Type != next.Type || current.RetiredAt != nil {
			return access.Invalid("guardrail", "The owning boundary and type are immutable.")
		}
		next.ID, next.Revision = current.ID, current.Revision+1
		_, err = tx.Exec(ctx, "UPDATE olp.guardrails SET name=$2,latest_revision_id=$3,etag=$4 WHERE id=$1", next.ID, next.Name, next.RevisionID, next.ETag)
	}
	if err != nil {
		return err
	}
	policy := next.Policy
	if policy == nil {
		policy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{}}
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_revisions(id,guardrail_id,revision,policy,created_by,configuration,timeout_ms,on_error) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", next.RevisionID, next.ID, next.Revision, encoded, actor, next.Configuration, next.TimeoutMS, next.OnError)
	if err == nil && legacy {
		_, err = tx.Exec(ctx, "UPDATE olp.guardrails SET active_revision_id=$2 WHERE id=$1", next.ID, next.RevisionID)
		if err == nil {
			_, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_revision_activations(revision_id,activated_by) VALUES($1,$2) ON CONFLICT DO NOTHING", next.RevisionID, actor)
		}
	}
	return err
}

// FindActive resolves the case-insensitive portable name in one project.
func FindActive(ctx context.Context, q access.Queryer, projectID, name string) (*Definition, error) {
	return scan(q.QueryRow(ctx, "SELECT "+columns+joined+" WHERE g.project_id IS NOT DISTINCT FROM $1::uuid AND lower(g.name)=lower($2) AND g.retired_at IS NULL", projectPointer(projectID), name))
}
