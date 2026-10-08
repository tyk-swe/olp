package resources

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/limits"
)

type CodeStore struct{ Pool *pgxpool.Pool }

var codeRefusalReason = regexp.MustCompile(`^code_[a-z_]{1,80}$`)

func (s *CodeStore) ObserveAllowance(ctx context.Context, account string, allowance codemode.Allowance) error {
	if err := allowance.Validate(); err != nil {
		return err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var document []byte
	if err = tx.QueryRow(ctx, `SELECT allowance FROM olp.code_accounts WHERE id=$1 FOR UPDATE`, account).Scan(&document); err != nil {
		return err
	}
	var current codemode.Allowance
	if len(document) > 0 {
		if err = json.Unmarshal(document, &current); err != nil {
			return err
		}
	}
	merged := mergeCodeAllowance(current, allowance)
	if err = merged.Validate(); err != nil {
		return err
	}
	document, err = json.Marshal(merged)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, account, document); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type CodeAdmission struct {
	Route     codemode.Route
	APIKeyID  string
	Operation codemode.Operation
	// Providers are the route revision's provider connections whose adapter
	// serves the request's path; only their accounts serve it.
	Providers        []string
	Bound            *codemode.TokenBound
	PreviousResponse string
}

type CodePermit struct {
	Authority access.Authority
	Binding   codemode.Binding
	Pin       codemode.Pin
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
	return s.admit(ctx, in, false)
}

// BindConnection pins a WebSocket connection to an account of providers
// without authorizing inference.
func (s *CodeStore) BindConnection(ctx context.Context, route codemode.Route, key string, identity codemode.Identity, model string, providers []string) (CodePermit, error) {
	if len(route.Models) == 0 {
		return CodePermit{}, codemode.Refuse(403, "code_model_denied")
	}
	operation := "connect_model"
	if model == "" {
		model = route.Models[0]
		operation = "connect"
	}
	return s.admit(ctx, CodeAdmission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: operation, Model: model, Identity: identity}, Providers: providers}, true)
}

func (s *CodeStore) admit(ctx context.Context, in CodeAdmission, connection bool) (CodePermit, error) {
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
	var reference codemode.Binding
	if in.PreviousResponse != "" {
		reference, err = ScanCodeBinding(tx.QueryRow(ctx, `SELECT `+CodeBindingColumns+` FROM olp.code_references x JOIN olp.code_bindings b ON b.id=x.binding_id
			WHERE x.route_id=$1 AND x.api_key_id=$2 AND x.external_id=$3 AND NOT x.ambiguous`, in.Route.ID, in.APIKeyID, in.PreviousResponse))
		if errors.Is(err, pgx.ErrNoRows) {
			return out, codemode.Refuse(409, "code_parent_unresolved")
		}
		if err != nil {
			return out, err
		}
		if in.Operation.Identity.Conversation != reference.Conversation && in.Operation.Identity.Parent == "" {
			var exists bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_bindings WHERE route_id=$1 AND api_key_id=$2 AND conversation=$3)`, in.Route.ID, in.APIKeyID, in.Operation.Identity.Conversation).Scan(&exists); err != nil {
				return out, err
			}
			if !exists {
				in.Operation.Identity.Parent = reference.Conversation
			}
		}
	}
	out.Binding, err = s.bind(ctx, tx, in)
	if err != nil {
		return out, err
	}
	if in.PreviousResponse != "" && reference.RootID != out.Binding.RootID {
		return out, codemode.Refuse(409, "code_parent_conflict")
	}
	pins, err := codePins(ctx, tx, out.Binding.RootID)
	if err != nil {
		return out, err
	}
	switch pinned := slices.IndexFunc(pins, func(p codemode.Pin) bool { return p.Model == in.Operation.Model }); {
	case pinned >= 0:
		// A republish can give two of the tree's accounts one adapter, so
		// the pinned account is checked against the earlier ones.
		out.Pin = pins[pinned]
		tree, _ := codeTree(out.Binding, pins)
		out.Account, err = codeAccount(ctx, tx, in, out.Pin.AccountID, out.Binding.RootID, tree)
	case connection:
		// A connection serves on the tree's own account until its model is
		// pinned; each generation it carries admits its own model.
		out.Pin = codemode.Pin{AccountID: out.Binding.AccountID, Principal: out.Binding.Principal}
		out.Account, err = codeAccount(ctx, tx, in, out.Pin.AccountID, "", nil)
	default:
		out.Pin, out.Account, err = codePin(ctx, tx, in, out.Binding, pins)
	}
	if err != nil {
		return out, err
	}
	if out.Account.Principal != out.Pin.Principal {
		return out, codemode.Refuse(403, "code_principal_changed")
	}
	if connection {
		return out, tx.Commit(ctx)
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
		FROM olp.api_keys k JOIN olp.users u ON u.id=k.created_by JOIN olp.code_routes r ON r.id=$2 JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id
		WHERE k.id=$1 AND u.active AND u.oidc_authorized AND
		(u.access_scope='global' OR EXISTS(SELECT 1 FROM olp.project_members m WHERE m.user_id=u.id AND m.project_id=r.project_id))`, in.APIKeyID, in.Route.ID).Scan(&a.ID, &a.LookupID, &a.Issuer, &a.ProjectID, &policy, &a.ExpiresAt, &a.RevokedAt, &document)
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
	b, err := codeConversation(ctx, tx, in, identity.Conversation)
	if err == nil {
		if err = liveCodeTree(ctx, tx, b); err != nil {
			return b, err
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
		parent, err := codeConversation(ctx, tx, in, identity.Parent)
		if errors.Is(err, pgx.ErrNoRows) {
			return b, codemode.Refuse(409, "code_parent_unresolved")
		}
		if err != nil {
			return b, err
		}
		if err = liveCodeTree(ctx, tx, parent); err != nil {
			return b, err
		}
		b.ParentID = &parent.ID
		b.RootID = parent.RootID
		b.AccountID = parent.AccountID
		b.Principal = parent.Principal
	} else {
		account, err := codeAccount(ctx, tx, in, "", "", nil)
		if err != nil {
			if refusal, ok := errors.AsType[*codemode.Refusal](err); ok && refusal.Code == "code_account_unavailable" && in.Operation.Name == "connect" && len(in.Route.Models) > 1 {
				return b, codemode.Refuse(400, "code_connection_model_required")
			}
			return b, err
		}
		b.AccountID = account.ID
		b.Principal = account.Principal
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_bindings(id,project_id,route_id,api_key_id,conversation,parent_id,root_id,account_id,principal)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at`, b.ID, b.ProjectID, b.RouteID, b.APIKeyID, b.Conversation, b.ParentID, b.RootID, b.AccountID, b.Principal).Scan(&b.CreatedAt)
	return b, err
}

func codeConversation(ctx context.Context, tx pgx.Tx, in CodeAdmission, conversation string) (codemode.Binding, error) {
	return ScanCodeBinding(tx.QueryRow(ctx, `SELECT `+CodeBindingColumns+` FROM olp.code_bindings b WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3`, in.Route.ID, in.APIKeyID, conversation))
}

// liveCodeTree refuses a binding whose tree is retired, and holds its root
// against retirement until the transaction ends.
func liveCodeTree(ctx context.Context, tx pgx.Tx, b codemode.Binding) error {
	var retired bool
	if err := tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM olp.code_bindings WHERE id=$1 FOR SHARE`, b.RootID).Scan(&retired); err != nil {
		return err
	}
	if retired || b.RetiredAt != nil {
		return codemode.Refuse(410, "code_binding_retired")
	}
	return nil
}

// codePins returns the pins of a conversation tree in the order it made them.
func codePins(ctx context.Context, tx pgx.Tx, root string) ([]codemode.Pin, error) {
	rows, err := tx.Query(ctx, `SELECT model,account_id::text,principal,created_at FROM olp.code_pins WHERE root_id=$1 ORDER BY seq`, root)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (codemode.Pin, error) {
		var p codemode.Pin
		err := row.Scan(&p.Model, &p.AccountID, &p.Principal, &p.CreatedAt)
		return p, err
	})
}

// codeTree lists a conversation tree's accounts, its own first and then its
// pins' in the order it made them, with the principal each serves it as.
func codeTree(b codemode.Binding, pins []codemode.Pin) ([]string, map[string]string) {
	tree := []string{b.AccountID}
	principals := map[string]string{b.AccountID: b.Principal}
	for _, p := range pins {
		if _, ok := principals[p.AccountID]; !ok {
			tree = append(tree, p.AccountID)
			principals[p.AccountID] = p.Principal
		}
	}
	return tree, principals
}

// codePin pins an admission's model, which the binding's tree has not served
// yet, to the account that serves it. A tree keeps one account of each
// adapter, so the model goes to the tree's own account when that serves it,
// else to another account the tree uses, else to the first available account
// of an adapter the tree does not use yet. Nothing fails over: a tree account
// that cools refuses a new model it serves as it refuses its pinned ones.
func codePin(ctx context.Context, tx pgx.Tx, in CodeAdmission, b codemode.Binding, pins []codemode.Pin) (codemode.Pin, codemode.Account, error) {
	tree, principals := codeTree(b, pins)
	account, err := codeAccount(ctx, tx, in, "", b.RootID, tree)
	if err != nil {
		return codemode.Pin{}, account, err
	}
	pin := codemode.Pin{Model: in.Operation.Model, AccountID: account.ID, Principal: account.Principal}
	if principal, ok := principals[account.ID]; ok {
		pin.Principal = principal
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_pins(root_id,model,account_id,principal,adapter)
		SELECT $1,$2,$3,$4,`+codeadapter.SQL("v.connections->($5::text)")+` FROM olp.code_route_revisions v WHERE v.id=$6 RETURNING created_at`,
		b.RootID, pin.Model, pin.AccountID, pin.Principal, account.ProviderID, in.Route.RevisionID).Scan(&pin.CreatedAt)
	return pin, account, err
}

// codeAccount selects the pool account that serves an admission: an eligible
// one of the admission's providers, so its adapter serves the request's path
// and the gateway can reach it. It is the account id when given, else the
// first of tree that serves the model, else the first available account of an
// adapter no account of tree has. An account of tree serves only while no
// earlier one has its adapter, now or as the root's pins recorded it, since a
// republish can change or drop an account's connection; a selected account
// that is unavailable refuses the admission.
func codeAccount(ctx context.Context, tx pgx.Tx, in CodeAdmission, id, root string, tree []string) (codemode.Account, error) {
	var a codemode.Account
	var models, allowance []byte
	var available bool
	required := []string{in.Operation.Model}
	if in.Operation.Name == "connect" {
		required = in.Route.Models
	}
	if id != "" && (in.Operation.Name == "connect" || in.Operation.Name == "connect_model") {
		required = []string{}
	}
	requiredJSON, err := json.Marshal(required)
	if err != nil {
		return a, err
	}
	err = tx.QueryRow(ctx, `SELECT a.id::text,a.project_id::text,a.provider_id::text,a.credential_id::text,a.principal,a.models,a.name,a.enabled,a.etag::text,a.health,a.allowance,olp.code_account_available(a)
		FROM olp.code_accounts a JOIN olp.code_pool_accounts pa ON pa.account_id=a.id
		JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=a.provider_id
		JOIN olp.code_route_revisions v ON v.id=$7 CROSS JOIN LATERAL (SELECT `+codeadapter.SQL("v.connections->(a.provider_id::text)")+` adapter) f
		WHERE pa.pool_id=$1 AND a.project_id=$2 AND a.enabled AND p.state<>'disabled' AND p.project_id=a.project_id
		AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())
		AND a.models @> $3::jsonb AND ($4='' OR a.id::text=$4) AND a.provider_id::text=ANY($5::text[])
		AND NOT EXISTS(SELECT 1 FROM olp.code_accounts t WHERE t.id::text=ANY($6::text[])
			AND coalesce(array_position($6::text[],t.id::text)<array_position($6::text[],a.id::text),true)
			AND (`+codeadapter.SQL("v.connections->(t.provider_id::text)")+` IS NOT DISTINCT FROM f.adapter
				OR EXISTS(SELECT 1 FROM olp.code_pins p WHERE p.root_id=nullif($8,'')::uuid AND p.account_id=t.id AND p.adapter IS NOT DISTINCT FROM f.adapter)))
		ORDER BY array_position($6::text[],a.id::text) NULLS LAST,olp.code_account_available(a) DESC,a.id LIMIT 1`,
		in.Route.PoolID, in.Route.ProjectID, requiredJSON, id, in.Providers, tree, in.Route.RevisionID, root).Scan(&a.ID, &a.ProjectID, &a.ProviderID, &a.CredentialID, &a.Principal, &models, &a.Name, &a.Enabled, &a.ETag, &a.Health, &allowance, &available)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !available {
		return a, codemode.Refuse(503, "code_account_unavailable")
	}
	if err != nil {
		return a, err
	}
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
	_, err := s.Pool.Exec(ctx, `UPDATE olp.code_accounts SET health=$2,
		unavailable_until=CASE WHEN $2 IN ('unavailable','quota_limited') THEN now()+interval '1 minute' END
		WHERE id=$1 AND ($2<>'healthy' OR unavailable_until IS NULL OR unavailable_until<=now())`, accountID, health)
	return err
}

func (s *CodeStore) ObserveReference(ctx context.Context, attemptID, externalID string) error {
	if err := (codemode.Identity{Conversation: externalID}).Validate(); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO olp.code_references(route_id,api_key_id,external_id,binding_id)
		SELECT a.route_id,a.api_key_id,$2,a.binding_id FROM olp.code_attempts a WHERE a.id=$1 AND a.state<>'prepared'
		ON CONFLICT(route_id,api_key_id,external_id) DO UPDATE SET ambiguous=olp.code_references.ambiguous OR
		(SELECT root_id FROM olp.code_bindings WHERE id=olp.code_references.binding_id)<>(SELECT root_id FROM olp.code_bindings WHERE id=EXCLUDED.binding_id)`, attemptID, externalID)
	return err
}

func (s *CodeStore) RecordRefusal(ctx context.Context, route codemode.Route, keyID, code string) error {
	if !codeRefusalReason.MatchString(code) {
		return codemode.Refuse(400, "code_refusal_invalid")
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO olp.code_refusals(id,project_id,route_id,api_key_id,code) SELECT $1,$2,$3,k.id,$5 FROM olp.api_keys k WHERE k.id=$4 AND k.project_id=$2`, access.NewID(), route.ProjectID, route.ID, keyID, code)
	return err
}
