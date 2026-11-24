package codesessions

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/routing"
)

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var codeRefusalReason = regexp.MustCompile(`^code_[a-z_]{1,80}$`)

func (s *Store) ObserveAllowance(ctx context.Context, account string, allowance codemode.Allowance) error {
	if err := allowance.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
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

type Recovery struct {
	From            string
	Envelope        string
	AccountID       string
	CredentialID    string
	GrantGeneration int64
	RejectedUntil   time.Time
	Number          int
}

type Admission struct {
	InitialRecovery  bool
	Recovery         *Recovery
	Attribution      map[string]string
	Route            codemode.Route
	APIKeyID         string
	EndUserDigest    string
	Operation        codemode.Operation
	ModelProviders   map[string][]string
	ModelSource      *routing.View
	ModelPath        string
	ModelTransport   codeadapter.Transport
	Providers        []string
	Bound            *codemode.TokenBound
	PreviousResponse string
}

type Permit struct {
	InitialRecovery bool
	Authority       keyauthority.Authority
	Binding         codemode.Binding
	Pin             codemode.Pin
	Account         codemode.Account
	Attempt         codemode.Attempt
}

const bindingColumns = `b.id::text,b.project_id::text,b.route_id::text,b.api_key_id::text,b.conversation,b.parent_id::text,b.root_id::text,b.account_id::text,b.principal,b.created_at,b.retired_at`

func scanBinding(row pgx.Row) (codemode.Binding, error) {
	var b codemode.Binding
	err := row.Scan(&b.ID, &b.ProjectID, &b.RouteID, &b.APIKeyID, &b.Conversation, &b.ParentID, &b.RootID, &b.AccountID, &b.Principal, &b.CreatedAt, &b.RetiredAt)
	return b, err
}

// Admit checks live authority, pins the tree and reserves every hard token
// budget atomically. A returned permit authorizes exactly one dispatch.
func (s *Store) Admit(ctx context.Context, in Admission) (Permit, error) {
	return s.admit(ctx, in, false)
}

// BindConnection pins a WebSocket connection to an account of providers
// without authorizing inference.
func (s *Store) BindConnection(ctx context.Context, route codemode.Route, key string, identity codemode.Identity, model string, providers []string) (Permit, error) {
	if len(route.Models) == 0 {
		return Permit{}, codemode.Refuse(403, "code_model_denied")
	}
	operation := "connect_model"
	if model == "" {
		model = route.Models[0]
		operation = "connect"
	}
	return s.admit(ctx, Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: operation, Model: model, Identity: identity}, Providers: providers}, true)
}

func validateCodeAdmission(in Admission) error {
	if err := attribution.Validate(in.Attribution); err != nil {
		return codemode.Refuse(400, "code_invalid_attribution")
	}
	if in.EndUserDigest != "" && !keyauthority.ValidEndUserDigest(in.EndUserDigest) {
		return codemode.Refuse(400, "code_end_user_invalid")
	}
	if err := in.Operation.Identity.Validate(); err != nil {
		return err
	}
	if err := httpkit.ValidText("operation", in.Operation.Name, 100); err != nil {
		return codemode.Refuse(400, "code_operation_invalid")
	}
	if err := httpkit.ValidText("model", in.Operation.Model, 200); err != nil {
		return codemode.Refuse(400, "code_model_invalid")
	}
	return nil
}

func codeReference(ctx context.Context, tx pgx.Tx, in Admission) (codemode.Binding, Admission, error) {
	var reference codemode.Binding
	if in.PreviousResponse == "" {
		return reference, in, nil
	}
	reference, err := scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM olp.code_references x JOIN olp.code_bindings b ON b.id=x.binding_id
		WHERE x.route_id=$1 AND x.api_key_id=$2 AND x.external_id=$3 AND NOT x.ambiguous AND x.account_id IS NOT NULL`, in.Route.ID, in.APIKeyID, in.PreviousResponse))
	if errors.Is(err, pgx.ErrNoRows) {
		return reference, in, codemode.Refuse(409, "code_parent_unresolved")
	}
	if err != nil {
		return reference, in, err
	}
	identity := in.Operation.Identity
	if (identity.Generic() || (codemode.Identity{Conversation: reference.Conversation}).Generic()) && identity.Conversation != reference.Conversation {
		return reference, in, codemode.Refuse(409, "code_parent_conflict")
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

func (in Admission) withOperation(operation codemode.Operation) Admission {
	providers := func() []string {
		if in.ModelProviders != nil {
			return in.ModelProviders[operation.Model]
		}
		if in.ModelSource != nil && (operation.Model != in.Operation.Model || in.Providers == nil) {
			return in.ModelSource.CodeModelProviders(in.Route, in.ModelPath, operation.Model, in.ModelTransport)
		}
		return in.Providers
	}()
	return Admission{ModelProviders: in.ModelProviders, ModelSource: in.ModelSource, ModelPath: in.ModelPath, ModelTransport: in.ModelTransport, InitialRecovery: in.InitialRecovery, Recovery: in.Recovery, Attribution: in.Attribution, Route: in.Route, APIKeyID: in.APIKeyID, EndUserDigest: in.EndUserDigest, Operation: operation, Providers: providers, Bound: in.Bound, PreviousResponse: in.PreviousResponse}
}

func (s *Store) admit(ctx context.Context, admission Admission, connection bool) (Permit, error) {
	var out Permit
	resolved, err := admission.Route.ResolveModel(admission.Operation)
	if err != nil {
		return out, err
	}
	input := admission.withOperation(resolved)
	if err := validateCodeAdmission(input); err != nil {
		return out, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
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
	var recoverySelection *codeSelection
	if referenced.Recovery != nil {
		if recoverySelection, err = recoverCodeBinding(ctx, tx, referenced); err != nil {
			return out, err
		}
	}
	var rootSelection *codeSelection
	out.Binding, rootSelection, err = s.bind(ctx, tx, referenced)
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
	reason := codemode.DecisionChosen
	var selection codeSelection
	switch pinned := slices.IndexFunc(pins, func(p codemode.Pin) bool { return p.Model == in.Operation.PublicModel() }); {
	case pinned >= 0:
		// A republish can give two of the tree's accounts one adapter, so
		// the pinned account is checked against the earlier ones.
		reason = codemode.DecisionPin
		out.Pin = pins[pinned]
		tree, _ := codeTree(out.Binding, pins)
		selection, err = codeAccount(ctx, tx, in, out.Pin.AccountID, out.Binding.RootID, tree)
		out.Account = selection.account
	case connection:
		// A connection serves on the tree's own account until its model is
		// pinned; each generation it carries admits its own model.
		reason = codemode.DecisionConnection
		out.Pin = codemode.Pin{AccountID: out.Binding.AccountID, Principal: out.Binding.Principal}
		selection, err = codeAccount(ctx, tx, in, out.Pin.AccountID, "", nil)
		out.Account = selection.account
	default:
		out.Pin, selection, err = codePin(ctx, tx, in, out.Binding, pins)
		out.Account = selection.account
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
	return commitCodeAdmission(ctx, tx, in, out, connection, selection, reason, rootSelection, recoverySelection)
}

// MarkDispatched is the irreversible pre-send transition. Network ambiguity
// leaves uncertainty; it cannot be used to authorize a second dispatch.
func (s *Store) MarkDispatched(ctx context.Context, id string) error {
	r, err := s.pool.Exec(ctx, `UPDATE olp.code_attempts SET state='uncertain' WHERE id=$1 AND state='prepared'
		AND (account_id IS NULL OR operation='prewarm' OR created_at>statement_timestamp()-make_interval(secs=>$2))`, id, float64(generationLeaseSeconds))
	if err != nil {
		return err
	}
	if r.RowsAffected() != 1 {
		return codemode.Refuse(409, "code_attempt_already_dispatched")
	}
	return nil
}

func (s *Store) Settle(ctx context.Context, id string, u codemode.Usage) error {
	return s.finish(ctx, id, u, false)
}
func (s *Store) Abort(ctx context.Context, id string) error {
	return s.finish(ctx, id, codemode.Usage{}, true)
}
func (s *Store) finish(ctx context.Context, id string, u codemode.Usage, abort bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = settleCodeTokens(ctx, tx, id, u, abort); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ObserveHealth(ctx context.Context, accountID string, observation codemode.HealthObservation) error {
	if err := observation.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE olp.code_accounts SET health=$2,health_observed_at=$3,
		unavailable_until=CASE WHEN $2='healthy' THEN NULL
			ELSE greatest(unavailable_until,coalesce($4::timestamptz,$3::timestamptz+interval '1 minute')) END
		WHERE id=$1 AND (health_observed_at IS NULL OR health_observed_at<$3)
		AND ($2<>'healthy' OR unavailable_until IS NULL OR unavailable_until<=now())`, accountID, observation.Status, observation.ObservedAt, observation.RetryAt)
	return err
}

func (s *Store) ObserveReference(ctx context.Context, attemptID, externalID string) error {
	if (codemode.Identity{Conversation: externalID}).Validate() != nil {
		return codemode.Refuse(400, "code_response_reference_invalid")
	}
	_, err := s.pool.Exec(ctx, `SELECT olp.observe_code_reference($1,$2)`, attemptID, externalID)
	return err
}

func (s *Store) RecordRefusal(ctx context.Context, route codemode.Route, keyID, digest, code string) error {
	if digest != "" && !keyauthority.ValidEndUserDigest(digest) {
		return codemode.Refuse(400, "code_end_user_invalid")
	}
	if !codeRefusalReason.MatchString(code) {
		return codemode.Refuse(400, "code_refusal_invalid")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO olp.code_refusals(id,project_id,route_id,api_key_id,code,end_user_digest) SELECT $1,$2,$3,k.id,$5,$6 FROM olp.api_keys k WHERE k.id=$4 AND k.project_id=$2`, httpkit.NewID(), route.ProjectID, route.ID, keyID, code, digest)
	return err
}

func commitCodeAdmission(ctx context.Context, tx pgx.Tx, in Admission, out Permit, connection bool, selection codeSelection, reason codemode.DecisionReason, rootSelection, recoverySelection *codeSelection) (Permit, error) {
	var err error
	if connection {
		if _, err := tx.Exec(ctx, `UPDATE olp.code_bindings SET socket_bound=true WHERE id=$1 AND NOT socket_bound`, out.Binding.RootID); err != nil {
			return out, err
		}
		decisionSelection := func() codeSelection {
			if rootSelection != nil && rootSelection.account.ID == selection.account.ID {
				return *rootSelection
			}
			return selection
		}()
		if err = recordCodeDecision(ctx, tx, in, out.Binding, nil, decisionSelection, codemode.DecisionConnection); err != nil {
			return out, err
		}
		return out, tx.Commit(ctx)
	}
	out.InitialRecovery = in.InitialRecovery && out.Binding.ParentID == nil && in.Operation.Identity.Parent == "" && in.PreviousResponse == "" && in.Route.InitialRecoveryAttempts > 0
	out.Attempt = codemode.Attempt{ID: httpkit.NewID(), ProjectID: in.Route.ProjectID, RouteID: in.Route.ID, RouteRevisionID: in.Route.RevisionID, APIKeyID: in.APIKeyID, Attribution: in.Attribution, EndUserDigest: in.EndUserDigest, BindingID: out.Binding.ID, AccountID: out.Account.ID, Operation: in.Operation.Name, Model: in.Operation.Model, RequestedModel: new(in.Operation.PublicModel()), State: "prepared"}
	if in.Bound != nil {
		if err = in.Bound.Validate(); err != nil {
			return out, err
		}
		out.Attempt.ReservedTokens = in.Bound.Tokens
		out.Attempt.BoundEvidence = &in.Bound.Evidence
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_attempts(id,project_id,route_id,api_key_id,binding_id,account_id,operation,model,state,reserved_tokens,bound_evidence,route_revision_id,end_user_digest,attribution,initial_recovery,recovery_from,requested_model,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'prepared',$9,$10,$11,$12,$13,$14,$15,$16,statement_timestamp()) RETURNING created_at`, out.Attempt.ID, in.Route.ProjectID, in.Route.ID, in.APIKeyID, out.Binding.ID, out.Account.ID, in.Operation.Name, in.Operation.Model, out.Attempt.ReservedTokens, out.Attempt.BoundEvidence, in.Route.RevisionID, in.EndUserDigest, attribution.JSON(in.Attribution), out.InitialRecovery, codeRecoveryFrom(in.Recovery), in.Operation.PublicModel()).Scan(&out.Attempt.CreatedAt)
	if err != nil {
		return out, err
	}
	decisionSelection, decisionReason := func() (codeSelection, codemode.DecisionReason) {
		if recoverySelection != nil && recoverySelection.account.ID == selection.account.ID {
			return *recoverySelection, codemode.DecisionChosen
		}
		if rootSelection != nil && reason == codemode.DecisionChosen && rootSelection.account.ID == selection.account.ID {
			return *rootSelection, reason
		}
		return selection, reason
	}()
	if err = recordCodeDecision(ctx, tx, in, out.Binding, &out.Attempt.ID, decisionSelection, decisionReason); err != nil {
		return out, err
	}
	if err = reserveCodeTokens(ctx, tx, out.Attempt, in.Bound); err != nil {
		return out, err
	}
	if err = tx.Commit(ctx); err != nil {
		return out, err
	}
	return out, nil
}
