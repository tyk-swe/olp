package resources

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
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
	merged := current.Merge(allowance)
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

type CodeRecovery struct {
	From            string
	AccountID       string
	CredentialID    string
	GrantGeneration int64
	RejectedUntil   time.Time
	Number          int
}

type CodeAdmission struct {
	InitialRecovery  bool
	Recovery         *CodeRecovery
	Attribution      map[string]string
	Route            codemode.Route
	APIKeyID         string
	EndUserDigest    string
	Operation        codemode.Operation
	ModelProviders   map[string][]string
	Providers        []string
	Bound            *codemode.TokenBound
	PreviousResponse string
}

type CodePermit struct {
	InitialRecovery bool
	Authority       access.Authority
	Binding         codemode.Binding
	Pin             codemode.Pin
	Account         codemode.Account
	Attempt         codemode.Attempt
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

func validateCodeAdmission(in CodeAdmission) error {
	if err := attribution.Validate(in.Attribution); err != nil {
		return codemode.Refuse(400, "code_invalid_attribution")
	}
	if in.EndUserDigest != "" && !access.ValidEndUserDigest(in.EndUserDigest) {
		return codemode.Refuse(400, "code_end_user_invalid")
	}
	if err := in.Operation.Identity.Validate(); err != nil {
		return err
	}
	if err := access.ValidText("operation", in.Operation.Name, 100); err != nil {
		return codemode.Refuse(400, "code_operation_invalid")
	}
	if err := access.ValidText("model", in.Operation.Model, 200); err != nil {
		return codemode.Refuse(400, "code_model_invalid")
	}
	return nil
}

func codeReference(ctx context.Context, tx pgx.Tx, in CodeAdmission) (codemode.Binding, CodeAdmission, error) {
	var reference codemode.Binding
	if in.PreviousResponse == "" {
		return reference, in, nil
	}
	reference, err := ScanCodeBinding(tx.QueryRow(ctx, `SELECT `+CodeBindingColumns+` FROM olp.code_references x JOIN olp.code_bindings b ON b.id=x.binding_id
		WHERE x.route_id=$1 AND x.api_key_id=$2 AND x.external_id=$3 AND NOT x.ambiguous AND x.account_id IS NOT NULL`, in.Route.ID, in.APIKeyID, in.PreviousResponse))
	if errors.Is(err, pgx.ErrNoRows) {
		return reference, in, codemode.Refuse(409, "code_parent_unresolved")
	}
	if err != nil {
		return reference, in, err
	}
	if in.Operation.Identity.Conversation != reference.Conversation && in.Operation.Identity.Parent == "" {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_bindings WHERE route_id=$1 AND api_key_id=$2 AND conversation=$3)`, in.Route.ID, in.APIKeyID, in.Operation.Identity.Conversation).Scan(&exists); err != nil {
			return reference, in, err
		}
		if !exists {
			updated := in
			updated.Operation.Identity.Parent = reference.Conversation
			return reference, updated, nil
		}
	}
	return reference, in, nil
}

func (in CodeAdmission) withOperation(operation codemode.Operation) CodeAdmission {
	providers := func() []string {
		if in.ModelProviders != nil {
			return in.ModelProviders[operation.Model]
		}
		return in.Providers
	}()
	return CodeAdmission{ModelProviders: in.ModelProviders, InitialRecovery: in.InitialRecovery, Recovery: in.Recovery, Attribution: in.Attribution, Route: in.Route, APIKeyID: in.APIKeyID, EndUserDigest: in.EndUserDigest, Operation: operation, Providers: providers, Bound: in.Bound, PreviousResponse: in.PreviousResponse}
}

func (s *CodeStore) admit(ctx context.Context, admission CodeAdmission, connection bool) (CodePermit, error) {
	var out CodePermit
	resolved, err := admission.Route.ResolveModel(admission.Operation)
	if err != nil {
		return out, err
	}
	input := admission.withOperation(resolved)
	if err := validateCodeAdmission(input); err != nil {
		return out, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT id FROM olp.installation WHERE singleton FOR SHARE`); err != nil {
		return out, err
	}
	if err = codeAuthority(ctx, tx, input, &out.Authority); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, input.Route.ID+":"+input.APIKeyID); err != nil {
		return out, err
	}
	reference, referenced, err := codeReference(ctx, tx, input)
	if err != nil {
		return out, err
	}
	if referenced.Recovery != nil {
		if err = recoverCodeBinding(ctx, tx, referenced); err != nil {
			return out, err
		}
	}
	out.Binding, err = s.bind(ctx, tx, referenced)
	if err != nil {
		return out, err
	}
	if referenced.PreviousResponse != "" && reference.RootID != out.Binding.RootID {
		return out, codemode.Refuse(409, "code_parent_conflict")
	}
	pins, err := codePins(ctx, tx, out.Binding.RootID)
	if err != nil {
		return out, err
	}
	selected, err := referenced.Route.ResolvePinnedModel(referenced.Operation, pins)
	if err != nil {
		return out, err
	}
	in := referenced.withOperation(selected)
	if selected.Model != referenced.Operation.Model {
		if err := codeAuthority(ctx, tx, in, &out.Authority); err != nil {
			return out, err
		}
	}
	switch pinned := slices.IndexFunc(pins, func(p codemode.Pin) bool { return p.Model == in.Operation.PublicModel() }); {
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
	if in.PreviousResponse != "" {
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_references WHERE route_id=$1 AND api_key_id=$2 AND external_id=$3 AND account_id=$4 AND NOT ambiguous)`, in.Route.ID, in.APIKeyID, in.PreviousResponse, out.Account.ID).Scan(&owned); err != nil {
			return out, err
		}
		if !owned {
			return out, codemode.Refuse(409, "code_response_account_conflict")
		}
	}
	if !connection && !out.Authority.AllowsAttribution(in.Attribution) {
		return out, codemode.Refuse(409, "code_attribution_changed")
	}
	if connection {
		if _, err := tx.Exec(ctx, `UPDATE olp.code_bindings SET socket_bound=true WHERE id=$1 AND NOT socket_bound`, out.Binding.RootID); err != nil {
			return out, err
		}
		return out, tx.Commit(ctx)
	}
	out.InitialRecovery = in.InitialRecovery && out.Binding.ParentID == nil && in.Operation.Identity.Parent == "" && in.PreviousResponse == "" && in.Route.InitialRecoveryAttempts > 0
	out.Attempt = codemode.Attempt{ID: access.NewID(), ProjectID: in.Route.ProjectID, RouteID: in.Route.ID, RouteRevisionID: in.Route.RevisionID, APIKeyID: in.APIKeyID, Attribution: in.Attribution, EndUserDigest: in.EndUserDigest, BindingID: out.Binding.ID, AccountID: out.Account.ID, Operation: in.Operation.Name, Model: in.Operation.Model, State: "prepared"}
	if in.Bound != nil {
		if err = in.Bound.Validate(); err != nil {
			return out, err
		}
		out.Attempt.ReservedTokens = in.Bound.Tokens
		out.Attempt.BoundEvidence = &in.Bound.Evidence
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_attempts(id,project_id,route_id,api_key_id,binding_id,account_id,operation,model,state,reserved_tokens,bound_evidence,route_revision_id,end_user_digest,attribution,initial_recovery,recovery_from)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'prepared',$9,$10,$11,$12,$13,$14,$15) RETURNING created_at`, out.Attempt.ID, in.Route.ProjectID, in.Route.ID, in.APIKeyID, out.Binding.ID, out.Account.ID, in.Operation.Name, in.Operation.Model, out.Attempt.ReservedTokens, out.Attempt.BoundEvidence, in.Route.RevisionID, in.EndUserDigest, attribution.JSON(in.Attribution), out.InitialRecovery, codeRecoveryFrom(in.Recovery)).Scan(&out.Attempt.CreatedAt)
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
	return codeAuthorityFor(ctx, tx, in, a, codeInference)
}

type codeAuthorityOperation uint8

const (
	codeInference codeAuthorityOperation = iota
	codeResearch
	codeModels
)

func codeAuthorityFor(ctx context.Context, tx pgx.Tx, in CodeAdmission, a *access.Authority, operation codeAuthorityOperation) error {
	var policy, document []byte
	var groups access.RouteGroups
	var templates access.LimitTemplates
	err := tx.QueryRow(ctx, `SELECT k.id::text,k.lookup_id,k.created_by::text,k.project_id::text,k.policy,k.expires_at,k.revoked_at,v.document,COALESCE(p.attribution_policy,'{}'::jsonb),COALESCE(p.route_groups,'{}'::jsonb),COALESCE(k.limit_lookup_id,k.lookup_id),COALESCE(p.limit_templates,'{}'::jsonb),p.end_user_policy,k.budget_group_id::text,g.limit_template,g.requests_per_minute,g.tokens_per_minute,g.max_concurrency,g.daily_cost_limit::text,g.monthly_cost_limit::text,g.weekly_cost_limit::text,k.workload_issuer_id::text,COALESCE(k.workload_digest,''),COALESCE(k.workload_mapping,''),COALESCE(wi.etag::text,'')
		FROM olp.api_keys k LEFT JOIN olp.workload_issuers wi ON wi.id=k.workload_issuer_id LEFT JOIN olp.budget_groups g ON g.id=k.budget_group_id LEFT JOIN olp.projects p ON p.id=k.project_id JOIN olp.users u ON u.id=k.created_by JOIN olp.code_routes r ON r.id=$2 JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id
		WHERE k.id=$1 AND (k.workload_issuer_id IS NULL OR wi.document->>'enabled'='true') AND u.active AND u.oidc_authorized AND
		(u.access_scope='global' OR EXISTS(SELECT 1 FROM olp.effective_project_members m WHERE m.user_id=u.id AND m.project_id=r.project_id))`, in.APIKeyID, in.Route.ID).Scan(&a.ID, &a.LookupID, &a.Issuer, &a.ProjectID, &policy, &a.ExpiresAt, &a.RevokedAt, &document, &a.ProjectAttributionPolicy, &groups, &a.LimitLookupID, &templates, &a.ProjectEndUserPolicy, &a.BudgetGroupID, &a.BudgetGroupTemplate, &a.BudgetGroupRPM, &a.BudgetGroupTPM, &a.BudgetGroupConcurrency, &a.BudgetGroupDailyCostLimit, &a.BudgetGroupMonthlyCostLimit, &a.BudgetGroupWeeklyCostLimit, &a.WorkloadIssuerID, &a.WorkloadDigest, &a.WorkloadMapping, &a.WorkloadRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return codemode.Refuse(403, "code_permission_denied")
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(policy, &a.Policy); err != nil {
		return err
	}
	if err = a.BindLimitTemplates(templates); err != nil {
		return err
	}
	if err = a.BindRouteGroups(groups); err != nil {
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
	if operation == codeInference {
		if _, err := current.ResolveModel(codemode.Operation{Model: in.Operation.PublicModel()}); err != nil {
			return err
		}
	}
	if operation == codeInference && (!slices.Contains(in.Route.Models, in.Operation.Model) || !slices.Contains(current.Models, in.Operation.Model)) {
		return codemode.Refuse(403, "code_model_denied")
	}
	if in.Recovery != nil && (current.RevisionID != in.Route.RevisionID || current.InitialRecoveryAttempts < in.Recovery.Number) {
		return codemode.Refuse(409, "code_recovery_policy_changed")
	}
	if operation == codeModels && current.RevisionID != in.Route.RevisionID {
		return codemode.Refuse(409, "code_route_revision_changed")
	}
	if operation == codeResearch && (current.RevisionID != in.Route.RevisionID || current.Tools == nil || in.Route.Tools == nil ||
		current.Tools.AccountID != in.Route.Tools.AccountID || !slices.Equal(current.Tools.Services, in.Route.Tools.Services)) {
		return codemode.Refuse(409, "code_research_revision_changed")
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
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_bindings(id,project_id,route_id,api_key_id,conversation,parent_id,root_id,account_id,principal,socket_bound)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at`, b.ID, b.ProjectID, b.RouteID, b.APIKeyID, b.Conversation, b.ParentID, b.RootID, b.AccountID, b.Principal, in.Operation.Name == "connect" || in.Operation.Name == "connect_model").Scan(&b.CreatedAt)
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
	rows, err := tx.Query(ctx, `SELECT model,coalesce(target_model,model),account_id::text,principal,created_at FROM olp.code_pins WHERE root_id=$1 ORDER BY seq`, root)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (codemode.Pin, error) {
		var p codemode.Pin
		err := row.Scan(&p.Model, &p.TargetModel, &p.AccountID, &p.Principal, &p.CreatedAt)
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
	pin := codemode.Pin{Model: in.Operation.PublicModel(), TargetModel: in.Operation.Model, AccountID: account.ID, Principal: account.Principal}
	if principal, ok := principals[account.ID]; ok {
		pin.Principal = principal
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_pins(root_id,model,account_id,principal,adapter,target_model)
		SELECT $1,$2,$3,$4,`+codeadapter.SQL("v.connections->($5::text)")+`,$7 FROM olp.code_route_revisions v WHERE v.id=$6 RETURNING created_at`,
		b.RootID, pin.Model, pin.AccountID, pin.Principal, account.ProviderID, in.Route.RevisionID, pin.TargetModel).Scan(&pin.CreatedAt)
	return pin, account, err
}

type codeCandidate struct {
	account   codemode.Account
	available bool
	position  int
	score     float64
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
	routeID, err := uuid.Parse(in.Route.ID)
	if err != nil {
		return a, err
	}
	rows, err := tx.Query(ctx, `SELECT a.id::text,a.project_id::text,a.provider_id::text,a.credential_id::text,a.principal,a.models,a.name,a.enabled,a.etag::text,a.health,a.allowance,olp.code_account_available(a),coalesce(array_position($6::text[],a.id::text),2147483647),pa.weight
		FROM olp.code_accounts a JOIN olp.code_pool_accounts pa ON pa.account_id=a.id
		JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=a.provider_id
		JOIN olp.code_route_revisions v ON v.id=$7 CROSS JOIN LATERAL (SELECT `+codeadapter.SQL("v.connections->(a.provider_id::text)")+` adapter) f
		WHERE pa.pool_id=$1 AND a.project_id=$2 AND a.enabled AND p.state<>'disabled' AND p.project_id=a.project_id
		AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())
		AND ($9::uuid IS NULL OR NOT EXISTS(SELECT 1 FROM olp.code_recovery_chain($9) rejected WHERE rejected.account_id=a.id AND rejected.nonexecution))
		AND a.models @> $3::jsonb AND ($4='' OR a.id::text=$4) AND a.provider_id::text=ANY($5::text[])
		AND NOT EXISTS(SELECT 1 FROM olp.code_accounts t WHERE t.id::text=ANY($6::text[])
			AND coalesce(array_position($6::text[],t.id::text)<array_position($6::text[],a.id::text),true)
			AND (`+codeadapter.SQL("v.connections->(t.provider_id::text)")+` IS NOT DISTINCT FROM f.adapter
				OR EXISTS(SELECT 1 FROM olp.code_pins p WHERE p.root_id=nullif($8,'')::uuid AND p.account_id=t.id AND p.adapter IS NOT DISTINCT FROM f.adapter)))`,
		in.Route.PoolID, in.Route.ProjectID, requiredJSON, id, in.Providers, tree, in.Route.RevisionID, root, codeRecoveryExclusion(in.Recovery))
	if err != nil {
		return a, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (codeCandidate, error) {
		var candidate codeCandidate
		var weight int64
		if err := row.Scan(&candidate.account.ID, &candidate.account.ProjectID, &candidate.account.ProviderID, &candidate.account.CredentialID, &candidate.account.Principal, &candidate.account.Models, &candidate.account.Name, &candidate.account.Enabled, &candidate.account.ETag, &candidate.account.Health, &candidate.account.Allowance, &candidate.available, &candidate.position, &weight); err != nil {
			return candidate, err
		}
		accountID, err := uuid.Parse(candidate.account.ID)
		if err != nil {
			return candidate, err
		}
		return codeCandidate{account: candidate.account, available: candidate.available, position: candidate.position, score: routing.Score(routeID, accountID, weight, in.Operation.Name, "code", in.Operation.Model, []byte(in.APIKeyID+":"+in.Operation.Identity.Conversation))}, nil
	})
	if err != nil {
		return a, err
	}
	if len(candidates) == 0 {
		return a, codemode.Refuse(503, "code_account_unavailable")
	}
	chosen := slices.MaxFunc(candidates, func(left, right codeCandidate) int {
		if left.position != right.position {
			return cmp.Compare(right.position, left.position)
		}
		if left.available != right.available {
			if left.available {
				return 1
			}
			return -1
		}
		if left.score != right.score {
			return cmp.Compare(left.score, right.score)
		}
		return cmp.Compare(right.account.ID, left.account.ID)
	})
	if !chosen.available {
		return a, codemode.Refuse(503, "code_account_unavailable")
	}
	return chosen.account, nil
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

func (s *CodeStore) ObserveHealth(ctx context.Context, accountID string, observation codemode.HealthObservation) error {
	if err := observation.Validate(); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE olp.code_accounts SET health=$2,health_observed_at=$3,
		unavailable_until=CASE WHEN $2='healthy' THEN NULL
			ELSE greatest(unavailable_until,coalesce($4::timestamptz,$3::timestamptz+interval '1 minute')) END
		WHERE id=$1 AND (health_observed_at IS NULL OR health_observed_at<$3)
		AND ($2<>'healthy' OR unavailable_until IS NULL OR unavailable_until<=now())`, accountID, observation.Status, observation.ObservedAt, observation.RetryAt)
	return err
}

func (s *CodeStore) ObserveReference(ctx context.Context, attemptID, externalID string) error {
	if err := (codemode.Identity{Conversation: externalID}).Validate(); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO olp.code_references(route_id,api_key_id,external_id,binding_id,account_id)
		SELECT a.route_id,a.api_key_id,$2,a.binding_id,a.account_id FROM olp.code_attempts a WHERE a.id=$1 AND a.state<>'prepared'
		ON CONFLICT(route_id,api_key_id,external_id) DO UPDATE SET ambiguous=olp.code_references.ambiguous OR
		olp.code_references.account_id IS DISTINCT FROM EXCLUDED.account_id OR
		(SELECT root_id FROM olp.code_bindings WHERE id=olp.code_references.binding_id)<>(SELECT root_id FROM olp.code_bindings WHERE id=EXCLUDED.binding_id)`, attemptID, externalID)
	return err
}

func (s *CodeStore) RecordRefusal(ctx context.Context, route codemode.Route, keyID, digest, code string) error {
	if digest != "" && !access.ValidEndUserDigest(digest) {
		return codemode.Refuse(400, "code_end_user_invalid")
	}
	if !codeRefusalReason.MatchString(code) {
		return codemode.Refuse(400, "code_refusal_invalid")
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO olp.code_refusals(id,project_id,route_id,api_key_id,code,end_user_digest) SELECT $1,$2,$3,k.id,$5,$6 FROM olp.api_keys k WHERE k.id=$4 AND k.project_id=$2`, access.NewID(), route.ProjectID, route.ID, keyID, code, digest)
	return err
}
