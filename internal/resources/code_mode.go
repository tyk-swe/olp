package resources

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/limits"
)

type CodeStore struct{ Pool *pgxpool.Pool }

func (s *CodeStore) ObserveAllowance(ctx context.Context, account string, allowance codemode.Allowance) error {
	if err := allowance.Validate(); err != nil {
		return err
	}
	document, err := json.Marshal(allowance)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1 AND (allowance IS NULL OR (allowance->>'observed_at')::timestamptz<=$3)`, account, document, allowance.ObservedAt)
	return err
}

type CodeAdmission struct {
	Route     codemode.Route
	APIKeyID  string
	Operation codemode.Operation
	Bound     *codemode.TokenBound
}

type CodePermit struct {
	Authority access.Authority
	Binding   codemode.Binding
	Account   codemode.Account
	Attempt   codemode.Attempt
}

const CodeBindingColumns = `b.id::text,b.project_id::text,b.route_id::text,b.api_key_id::text,b.conversation,b.parent_id::text,b.root_id::text,b.account_id::text,b.principal,b.created_at,b.retired_at`

func ScanCodeBinding(row pgx.Row) (codemode.Binding, error) {
	var b codemode.Binding
	err := row.Scan(&b.ID, &b.ProjectID, &b.RouteID, &b.APIKeyID, &b.Conversation, &b.ParentID, &b.RootID, &b.AccountID, &b.Principal, &b.CreatedAt, &b.RetiredAt)
	return b, err
}

// Admit checks live authority, pins the tree and reserves every hard token
// budget atomically. A returned permit authorizes exactly one dispatch.
func (s *CodeStore) Admit(ctx context.Context, in CodeAdmission) (CodePermit, error) {
	var out CodePermit
	if err := in.Operation.Identity.Validate(); err != nil {
		return out, err
	}
	if err := access.ValidText("operation", in.Operation.Name, 100); err != nil {
		return out, codemode.Refuse(400, "code_operation_invalid")
	}
	if err := access.ValidText("model", in.Operation.Model, 200); err != nil {
		return out, codemode.Refuse(400, "code_model_invalid")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT id FROM olp.installation WHERE singleton FOR SHARE`); err != nil {
		return out, err
	}
	if err = codeAuthority(ctx, tx, in, &out.Authority); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, in.Route.ID+":"+in.APIKeyID); err != nil {
		return out, err
	}
	out.Binding, err = s.bind(ctx, tx, in)
	if err != nil {
		return out, err
	}
	out.Account, err = codeAccount(ctx, tx, in, out.Binding.AccountID)
	if err != nil {
		return out, err
	}
	if out.Account.Principal != out.Binding.Principal {
		return out, codemode.Refuse(403, "code_principal_changed")
	}
	out.Attempt = codemode.Attempt{ID: access.NewID(), ProjectID: in.Route.ProjectID, RouteID: in.Route.ID, RouteRevisionID: in.Route.RevisionID, APIKeyID: in.APIKeyID, BindingID: out.Binding.ID, AccountID: out.Account.ID, Operation: in.Operation.Name, Model: in.Operation.Model, State: "prepared"}
	if in.Bound != nil {
		if err = in.Bound.Validate(); err != nil {
			return out, err
		}
		out.Attempt.ReservedTokens = in.Bound.Tokens
		out.Attempt.BoundEvidence = &in.Bound.Evidence
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_attempts(id,project_id,route_id,api_key_id,binding_id,account_id,operation,model,state,reserved_tokens,bound_evidence,route_revision_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'prepared',$9,$10,$11) RETURNING created_at`, out.Attempt.ID, in.Route.ProjectID, in.Route.ID, in.APIKeyID, out.Binding.ID, out.Account.ID, in.Operation.Name, in.Operation.Model, out.Attempt.ReservedTokens, out.Attempt.BoundEvidence, in.Route.RevisionID).Scan(&out.Attempt.CreatedAt)
	if err != nil {
		return out, err
	}
	if err = limits.ReserveCodeTokens(ctx, tx, out.Attempt, in.Bound); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}

func codeAuthority(ctx context.Context, tx pgx.Tx, in CodeAdmission, a *access.Authority) error {
	var policy, document []byte
	err := tx.QueryRow(ctx, `SELECT k.id::text,k.lookup_id,k.created_by::text,k.project_id::text,k.policy,k.expires_at,k.revoked_at,v.document
		FROM olp.api_keys k JOIN olp.code_routes r ON r.id=$2 JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id WHERE k.id=$1`, in.APIKeyID, in.Route.ID).Scan(&a.ID, &a.LookupID, &a.Issuer, &a.ProjectID, &policy, &a.ExpiresAt, &a.RevokedAt, &document)
	if errors.Is(err, pgx.ErrNoRows) {
		return codemode.Refuse(403, "code_permission_denied")
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(policy, &a.Policy); err != nil {
		return err
	}
	var current codemode.Route
	if err = json.Unmarshal(document, &current); err != nil {
		return err
	}
	if !in.Route.Enabled || !current.Enabled || current.ID != in.Route.ID || current.Slug != in.Route.Slug || current.ProjectID != in.Route.ProjectID || current.PoolID != in.Route.PoolID ||
		!a.Allows("inference", in.Route.Slug, &in.Route.ProjectID, time.Now()) {
		return codemode.Refuse(403, "code_permission_denied")
	}
	if !slices.Contains(in.Route.Models, in.Operation.Model) || !slices.Contains(current.Models, in.Operation.Model) {
		return codemode.Refuse(403, "code_model_denied")
	}
	var assigned bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_pool_keys pk JOIN olp.code_pools p ON p.id=pk.pool_id JOIN olp.api_keys k ON k.id=pk.api_key_id
		WHERE p.id=$1 AND pk.api_key_id=$2 AND p.project_id=$3 AND (p.kind='shared' OR p.owner_user_id=k.created_by))`, in.Route.PoolID, in.APIKeyID, in.Route.ProjectID).Scan(&assigned)
	if err != nil {
		return err
	}
	if !assigned {
		return codemode.Refuse(403, "code_pool_denied")
	}
	return nil
}

func (s *CodeStore) bind(ctx context.Context, tx pgx.Tx, in CodeAdmission) (codemode.Binding, error) {
	identity := in.Operation.Identity
	b, err := ScanCodeBinding(tx.QueryRow(ctx, `SELECT `+CodeBindingColumns+` FROM olp.code_bindings b WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3`, in.Route.ID, in.APIKeyID, identity.Conversation))
	if err == nil {
		var retired bool
		if err = tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM olp.code_bindings WHERE id=$1 FOR SHARE`, b.RootID).Scan(&retired); err != nil {
			return b, err
		}
		if retired || b.RetiredAt != nil {
			return b, codemode.Refuse(410, "code_binding_retired")
		}
		if identity.Parent != "" {
			var parent string
			if err = tx.QueryRow(ctx, `SELECT conversation FROM olp.code_bindings WHERE id=$1`, b.ParentID).Scan(&parent); err != nil || parent != identity.Parent {
				return b, codemode.Refuse(409, "code_parent_conflict")
			}
		}
		return b, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return b, err
	}
	b = codemode.Binding{ID: access.NewID(), ProjectID: in.Route.ProjectID, RouteID: in.Route.ID, APIKeyID: in.APIKeyID, Conversation: identity.Conversation}
	b.RootID = b.ID
	if identity.Parent != "" {
		parent, err := ScanCodeBinding(tx.QueryRow(ctx, `SELECT `+CodeBindingColumns+` FROM olp.code_bindings b WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3`, in.Route.ID, in.APIKeyID, identity.Parent))
		if errors.Is(err, pgx.ErrNoRows) {
			return b, codemode.Refuse(409, "code_parent_unresolved")
		}
		if err != nil {
			return b, err
		}
		var retired bool
		if err = tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM olp.code_bindings WHERE id=$1 FOR SHARE`, parent.RootID).Scan(&retired); err != nil {
			return b, err
		}
		if retired || parent.RetiredAt != nil {
			return b, codemode.Refuse(410, "code_binding_retired")
		}
		b.ParentID = &parent.ID
		b.RootID = parent.RootID
		b.AccountID = parent.AccountID
		b.Principal = parent.Principal
	} else {
		account, err := codeAccount(ctx, tx, in, "")
		if err != nil {
			return b, err
		}
		b.AccountID = account.ID
		b.Principal = account.Principal
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_bindings(id,project_id,route_id,api_key_id,conversation,parent_id,root_id,account_id,principal)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`, b.ID, b.ProjectID, b.RouteID, b.APIKeyID, b.Conversation, b.ParentID, b.RootID, b.AccountID, b.Principal).Scan(&b.CreatedAt)
	return b, err
}

func codeAccount(ctx context.Context, tx pgx.Tx, in CodeAdmission, id string) (codemode.Account, error) {
	var a codemode.Account
	var models, allowance []byte
	err := tx.QueryRow(ctx, `SELECT a.id::text,a.project_id::text,a.provider_id::text,a.credential_id::text,a.principal,a.models,a.name,a.enabled,a.etag::text,a.health,a.allowance
		FROM olp.code_accounts a JOIN olp.code_pool_accounts pa ON pa.account_id=a.id
		JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=a.provider_id
		WHERE pa.pool_id=$1 AND a.project_id=$2 AND a.enabled AND p.state<>'disabled' AND p.project_id=a.project_id
		AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())
		AND a.models ? $3 AND ($4='' OR a.id::text=$4) AND a.health NOT IN ('unavailable','quota_limited') ORDER BY a.id LIMIT 1`, in.Route.PoolID, in.Route.ProjectID, in.Operation.Model, id).Scan(&a.ID, &a.ProjectID, &a.ProviderID, &a.CredentialID, &a.Principal, &models, &a.Name, &a.Enabled, &a.ETag, &a.Health, &allowance)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, codemode.Refuse(503, "code_account_unavailable")
	}
	if err != nil {
		return a, err
	}
	a.Eligible = true
	a.GrantState = "current"
	if len(allowance) > 0 {
		if err = json.Unmarshal(allowance, &a.Allowance); err != nil {
			return a, err
		}
	}
	err = json.Unmarshal(models, &a.Models)
	return a, err
}

// MarkDispatched is the irreversible pre-send transition. Network ambiguity
// leaves uncertainty; it cannot be used to authorize a second dispatch.
func (s *CodeStore) MarkDispatched(ctx context.Context, id string) error {
	r, err := s.Pool.Exec(ctx, `UPDATE olp.code_attempts SET state='uncertain' WHERE id=$1 AND state='prepared'`, id)
	if err != nil {
		return err
	}
	if r.RowsAffected() != 1 {
		return codemode.Refuse(409, "code_attempt_already_dispatched")
	}
	return nil
}

func (s *CodeStore) Settle(ctx context.Context, id string, u codemode.Usage) error {
	return s.finish(ctx, id, u, false)
}
func (s *CodeStore) Abort(ctx context.Context, id string) error {
	return s.finish(ctx, id, codemode.Usage{}, true)
}
func (s *CodeStore) finish(ctx context.Context, id string, u codemode.Usage, abort bool) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = limits.SettleCodeTokens(ctx, tx, id, u, abort); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *CodeStore) ObserveHealth(ctx context.Context, accountID, health string) error {
	if !slices.Contains([]string{"healthy", "unavailable", "quota_limited"}, health) {
		return codemode.Refuse(400, "code_health_invalid")
	}
	_, err := s.Pool.Exec(ctx, `UPDATE olp.code_accounts SET health=$2 WHERE id=$1`, accountID, health)
	return err
}

func (s *CodeStore) RecordRefusal(ctx context.Context, route codemode.Route, keyID, code string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO olp.code_refusals(id,project_id,route_id,api_key_id,code) SELECT $1,$2,$3,k.id,$5 FROM olp.api_keys k WHERE k.id=$4 AND k.project_id=$2`, access.NewID(), route.ProjectID, route.ID, keyID, code)
	return err
}
