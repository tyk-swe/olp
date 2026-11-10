package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/routing"
)

type bindMode uint8

const (
	modeInference bindMode = iota
	modeConnect
	modeConnectModel
)

func (m bindMode) isConnection() bool {
	return m == modeConnect || m == modeConnectModel
}

const bindingColumns = `b.id::text,b.project_id::text,b.route_id::text,b.api_key_id::text,b.conversation,b.parent_id::text,b.root_id::text,b.account_id::text,b.principal,b.created_at,b.retired_at`

func scanBinding(row pgx.Row) (code.Binding, error) {
	var b code.Binding
	err := row.Scan(&b.ID, &b.ProjectID, &b.RouteID, &b.APIKeyID, &b.Conversation, &b.ParentID, &b.RootID, &b.AccountID, &b.Principal, &b.CreatedAt, &b.RetiredAt)
	return b, err
}

func (s *Store) Admit(ctx context.Context, in Admission) (Permit, error) {
	return s.admit(ctx, in, modeInference)
}

func (s *Store) BindConnection(ctx context.Context, route code.Route, key string, identity code.Identity, model string, providers []string) (Permit, error) {
	if len(route.Models) == 0 {
		return Permit{}, code.Refuse(403, "code_model_denied")
	}
	mode := modeConnectModel
	operation := "connect_model"
	if model == "" {
		mode = modeConnect
		model = route.Models[0]
		operation = "connect"
	}
	return s.admit(ctx, Admission{
		Route:        route,
		APIKeyID:     key,
		Operation:    code.Operation{Name: operation, Model: model, Identity: identity},
		ProvidersFor: StaticProviders(providers...),
	}, mode)
}

func resolveReference(ctx context.Context, tx pgx.Tx, in Admission) (code.Binding, Admission, error) {
	var reference code.Binding
	if in.PreviousResponse == "" {
		return reference, in, nil
	}
	reference, err := scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM olp.code_references x JOIN olp.code_bindings b ON b.id=x.binding_id
		WHERE x.route_id=$1 AND x.api_key_id=$2 AND x.external_id=$3 AND NOT x.ambiguous AND x.account_id IS NOT NULL`,
		in.Route.ID, in.APIKeyID, in.PreviousResponse))
	if errors.Is(err, pgx.ErrNoRows) {
		return reference, in, code.Refuse(409, "code_parent_unresolved")
	}
	if err != nil {
		return reference, in, err
	}
	identity := in.Operation.Identity
	if (identity.Generic() || (code.Identity{Conversation: reference.Conversation}).Generic()) && identity.Conversation != reference.Conversation {
		return reference, in, code.Refuse(409, "code_parent_conflict")
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

func (s *Store) admit(ctx context.Context, admission Admission, mode bindMode) (Permit, error) {
	var out Permit
	resolved, err := admission.Route.ResolveModel(admission.Operation)
	if err != nil {
		return out, err
	}
	input := admission.withOperation(resolved)
	if err := validateAdmission(input); err != nil {
		return out, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT id FROM olp.installation WHERE singleton FOR SHARE`); err != nil {
		return out, err
	}
	if _, err = loadAuthority(ctx, tx, input, &out.Authority, scopeInference); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, input.Route.ID+":"+input.APIKeyID); err != nil {
		return out, err
	}
	reference, referenced, err := resolveReference(ctx, tx, input)
	if err != nil {
		return out, err
	}
	var recoverySelection *selectionResult
	if referenced.Recovery != nil {
		if recoverySelection, err = recoverBinding(ctx, tx, referenced); err != nil {
			return out, err
		}
	}
	var rootSelection *selectionResult
	out.Binding, rootSelection, err = s.bind(ctx, tx, referenced, mode)
	if err != nil {
		return out, err
	}
	if referenced.PreviousResponse != "" && reference.RootID != out.Binding.RootID {
		return out, code.Refuse(409, "code_parent_conflict")
	}
	pins, err := loadPins(ctx, tx, out.Binding.RootID)
	if err != nil {
		return out, err
	}
	selected, err := referenced.Route.ResolvePinnedModel(referenced.Operation, pins)
	if err != nil {
		return out, err
	}
	in := referenced.withOperation(selected)
	if selected.Model != referenced.Operation.Model {
		if _, err := loadAuthority(ctx, tx, in, &out.Authority, scopeInference); err != nil {
			return out, err
		}
	}
	reason := code.DecisionChosen
	var selection selectionResult
	switch pinned := slices.IndexFunc(pins, func(p code.Pin) bool { return p.Model == in.Operation.PublicModel() }); {
	case pinned >= 0:
		reason = code.DecisionPin
		out.Pin = pins[pinned]
		tree, _ := treeAccounts(out.Binding, pins)
		selection, err = selectAccount(ctx, tx, in, mode, out.Pin.AccountID, out.Binding.RootID, tree, false)
		out.Account = selection.account
	case mode.isConnection():
		reason = code.DecisionConnection
		out.Pin = code.Pin{AccountID: out.Binding.AccountID, Principal: out.Binding.Principal}
		selection, err = selectAccount(ctx, tx, in, mode, out.Pin.AccountID, "", nil, false)
		out.Account = selection.account
	default:
		out.Pin, selection, err = pinModel(ctx, tx, in, out.Binding, pins, in.Route.AllowanceOrdering && rootSelection != nil)
		out.Account = selection.account
	}
	if err != nil {
		return out, err
	}
	if out.Account.Principal != out.Pin.Principal {
		return out, code.Refuse(403, "code_principal_changed")
	}
	if in.PreviousResponse != "" {
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_references WHERE route_id=$1 AND api_key_id=$2 AND external_id=$3 AND account_id=$4 AND NOT ambiguous)`, in.Route.ID, in.APIKeyID, in.PreviousResponse, out.Account.ID).Scan(&owned); err != nil {
			return out, err
		}
		if !owned {
			return out, code.Refuse(409, "code_response_account_conflict")
		}
	}
	if !mode.isConnection() && !out.Authority.AllowsAttribution(in.Attribution) {
		return out, code.Refuse(409, "code_attribution_changed")
	}
	if mode.isConnection() {
		if _, err := tx.Exec(ctx, `UPDATE olp.code_bindings SET socket_bound=true WHERE id=$1 AND NOT socket_bound`, out.Binding.RootID); err != nil {
			return out, err
		}
		decisionSelection := selection
		if rootSelection != nil && rootSelection.account.ID == selection.account.ID {
			decisionSelection = *rootSelection
		}
		if err = recordDecision(ctx, tx, in, out.Binding, nil, decisionSelection, code.DecisionConnection); err != nil {
			return out, err
		}
		return out, tx.Commit(ctx)
	}
	out.InitialRecovery = in.InitialRecovery && out.Binding.ParentID == nil && in.Operation.Identity.Parent == "" && in.PreviousResponse == "" && in.Route.InitialRecoveryAttempts > 0
	out.Attempt = code.Attempt{
		ID: access.NewID(), ProjectID: in.Route.ProjectID, RouteID: in.Route.ID, RouteRevisionID: in.Route.RevisionID,
		APIKeyID: in.APIKeyID, Attribution: in.Attribution, EndUserDigest: in.EndUserDigest, BindingID: out.Binding.ID,
		AccountID: out.Account.ID, Operation: in.Operation.Name, Model: in.Operation.Model,
		RequestedModel: new(in.Operation.PublicModel()), State: code.StatePrepared,
	}
	if in.Bound != nil {
		if err = in.Bound.Validate(); err != nil {
			return out, err
		}
		out.Attempt.ReservedTokens = in.Bound.Tokens
		out.Attempt.BoundEvidence = &in.Bound.Evidence
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_attempts(id,project_id,route_id,api_key_id,binding_id,account_id,operation,model,state,reserved_tokens,bound_evidence,route_revision_id,end_user_digest,attribution,initial_recovery,recovery_from,requested_model)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'prepared',$9,$10,$11,$12,$13,$14,$15,$16) RETURNING created_at`,
		out.Attempt.ID, in.Route.ProjectID, in.Route.ID, in.APIKeyID, out.Binding.ID, out.Account.ID, in.Operation.Name, in.Operation.Model,
		out.Attempt.ReservedTokens, out.Attempt.BoundEvidence, in.Route.RevisionID, in.EndUserDigest, attribution.JSON(in.Attribution),
		out.InitialRecovery, recoveryFrom(in.Recovery), in.Operation.PublicModel()).Scan(&out.Attempt.CreatedAt)
	if err != nil {
		return out, err
	}
	decisionSelection, decisionReason := selection, reason
	if recoverySelection != nil && recoverySelection.account.ID == selection.account.ID {
		decisionSelection, decisionReason = *recoverySelection, code.DecisionChosen
	} else if rootSelection != nil && reason == code.DecisionChosen && rootSelection.account.ID == selection.account.ID {
		decisionSelection = *rootSelection
	}
	if err = recordDecision(ctx, tx, in, out.Binding, &out.Attempt.ID, decisionSelection, decisionReason); err != nil {
		return out, err
	}
	if err = reserveTokens(ctx, tx, out.Attempt, in.Bound); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) bind(ctx context.Context, tx pgx.Tx, in Admission, mode bindMode) (code.Binding, *selectionResult, error) {
	identity := in.Operation.Identity
	b, err := loadConversation(ctx, tx, in, identity.Conversation)
	if err == nil {
		if err = requireLiveTree(ctx, tx, b); err != nil {
			return b, nil, err
		}
		if identity.Parent != "" {
			var parent string
			if err = tx.QueryRow(ctx, `SELECT conversation FROM olp.code_bindings WHERE id=$1`, b.ParentID).Scan(&parent); err != nil || parent != identity.Parent {
				return b, nil, code.Refuse(409, "code_parent_conflict")
			}
		}
		return b, nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return b, nil, err
	}
	b = code.Binding{ID: access.NewID(), ProjectID: in.Route.ProjectID, RouteID: in.Route.ID, APIKeyID: in.APIKeyID, Conversation: identity.Conversation}
	b.RootID = b.ID
	var root *selectionResult
	if identity.Parent != "" {
		parent, err := loadConversation(ctx, tx, in, identity.Parent)
		if errors.Is(err, pgx.ErrNoRows) {
			return b, nil, code.Refuse(409, "code_parent_unresolved")
		}
		if err != nil {
			return b, nil, err
		}
		if err = requireLiveTree(ctx, tx, parent); err != nil {
			return b, nil, err
		}
		b.ParentID = &parent.ID
		b.RootID = parent.RootID
		b.AccountID = parent.AccountID
		b.Principal = parent.Principal
	} else {
		selection, err := selectAccount(ctx, tx, in, mode, "", "", nil, in.Route.AllowanceOrdering)
		if err != nil {
			if refusal, ok := errors.AsType[*code.Refusal](err); ok && refusal.Code == "code_account_unavailable" && mode == modeConnect && len(in.Route.Models) > 1 {
				return b, nil, modelHintRefusal(ctx, tx, in, selection, err)
			}
			return b, nil, err
		}
		root = &selection
		b.AccountID = selection.account.ID
		b.Principal = selection.account.Principal
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_bindings(id,project_id,route_id,api_key_id,conversation,parent_id,root_id,account_id,principal,socket_bound)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at`,
		b.ID, b.ProjectID, b.RouteID, b.APIKeyID, b.Conversation, b.ParentID, b.RootID, b.AccountID, b.Principal, mode.isConnection()).Scan(&b.CreatedAt)
	return b, root, err
}

func modelHintRefusal(ctx context.Context, tx pgx.Tx, in Admission, base selectionResult, baseErr error) error {
	type hintResult struct {
		selection selectionResult
		err       error
	}
	hints := slices.Collect(func(yield func(hintResult) bool) {
		for _, model := range in.Route.Models {
			hinted := in.withOperation(code.Operation{Name: "connect_model", Model: model, Identity: in.Operation.Identity})
			candidate, hintErr := selectAccount(ctx, tx, hinted, modeConnectModel, "", "", nil, in.Route.AllowanceOrdering)
			if !yield(hintResult{selection: candidate, err: hintErr}) || hintErr == nil {
				return
			}
			if _, ok := errors.AsType[*code.Refusal](hintErr); !ok {
				return
			}
		}
	})
	for _, hint := range hints {
		if hint.err == nil {
			return code.Refuse(400, "code_connection_model_required")
		}
		if _, ok := errors.AsType[*code.Refusal](hint.err); !ok {
			return hint.err
		}
	}
	if len(base.candidates) == 0 {
		deadlines := slices.Collect(func(yield func(*time.Time) bool) {
			for _, hint := range hints {
				if len(hint.selection.candidates) == 0 {
					continue
				}
				refusal, _ := errors.AsType[*code.Refusal](hint.err)
				if !yield(refusal.RetryAt) {
					return
				}
			}
		})
		if len(deadlines) > 0 {
			return &code.Refusal{Status: 400, Code: "code_connection_model_required", RetryAt: code.EarliestRecovery(time.Now(), deadlines...)}
		}
	}
	return baseErr
}

func loadConversation(ctx context.Context, tx pgx.Tx, in Admission, conversation string) (code.Binding, error) {
	return scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM olp.code_bindings b WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3`, in.Route.ID, in.APIKeyID, conversation))
}

func requireLiveTree(ctx context.Context, tx pgx.Tx, b code.Binding) error {
	var retired bool
	if err := tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM olp.code_bindings WHERE id=$1 FOR SHARE`, b.RootID).Scan(&retired); err != nil {
		return err
	}
	if retired || b.RetiredAt != nil {
		return code.Refuse(410, "code_binding_retired")
	}
	return nil
}

func loadPins(ctx context.Context, tx pgx.Tx, root string) ([]code.Pin, error) {
	rows, err := tx.Query(ctx, `SELECT model,coalesce(target_model,model),account_id::text,principal,created_at FROM olp.code_pins WHERE root_id=$1 ORDER BY seq`, root)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (code.Pin, error) {
		var p code.Pin
		err := row.Scan(&p.Model, &p.TargetModel, &p.AccountID, &p.Principal, &p.CreatedAt)
		return p, err
	})
}

func treeAccounts(b code.Binding, pins []code.Pin) ([]string, map[string]string) {
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

func revisionAdapter(ctx context.Context, tx pgx.Tx, revisionID, providerID string) (*string, error) {
	var configJSON []byte
	if err := tx.QueryRow(ctx, `SELECT connections->($2::text) FROM olp.code_route_revisions WHERE id=$1`, revisionID, providerID).Scan(&configJSON); err != nil {
		return nil, err
	}
	if len(configJSON) == 0 {
		return nil, nil
	}
	var cfg vendor.ConnectionIdentity
	if err := json.Unmarshal(configJSON, &cfg); err != nil {
		return nil, err
	}
	v, ok := vendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID)
	if !ok {
		return nil, nil
	}
	adapter := string(v.Adapter)
	return &adapter, nil
}

func pinModel(ctx context.Context, tx pgx.Tx, in Admission, b code.Binding, pins []code.Pin, bandOrdered bool) (code.Pin, selectionResult, error) {
	tree, principals := treeAccounts(b, pins)
	selection, err := selectAccount(ctx, tx, in, modeInference, "", b.RootID, tree, bandOrdered)
	if err != nil {
		return code.Pin{}, selection, err
	}
	account := selection.account
	pin := code.Pin{Model: in.Operation.PublicModel(), TargetModel: in.Operation.Model, AccountID: account.ID, Principal: account.Principal}
	if principal, ok := principals[account.ID]; ok {
		pin.Principal = principal
	}
	adapter, err := revisionAdapter(ctx, tx, in.Route.RevisionID, account.ProviderID)
	if err != nil {
		return code.Pin{}, selection, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_pins(root_id,model,account_id,principal,adapter,target_model)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING created_at`,
		b.RootID, pin.Model, pin.AccountID, pin.Principal, adapter, pin.TargetModel).Scan(&pin.CreatedAt)
	return pin, selection, err
}

type accountCandidate struct {
	account       code.Account
	outcome       code.Candidate
	cooldownRetry *time.Time
	assessment    *code.BandAssessment
}

type selectionResult struct {
	account    code.Account
	candidates []code.Candidate
	assessment *code.BandAssessment
}

func (s selectionResult) chosen() (code.Candidate, bool) {
	index := slices.IndexFunc(s.candidates, func(candidate code.Candidate) bool {
		return candidate.AccountID == s.account.ID
	})
	if index < 0 {
		return code.Candidate{}, false
	}
	return s.candidates[index], true
}

func treeUsedAdapters(ctx context.Context, tx pgx.Tx, revisionID, root string, tree []string) (map[string][]string, error) {
	if len(tree) == 0 {
		return nil, nil
	}
	var connectionsJSON []byte
	if err := tx.QueryRow(ctx, `SELECT connections FROM olp.code_route_revisions WHERE id=$1`, revisionID).Scan(&connectionsJSON); err != nil {
		return nil, err
	}
	var connections map[string]vendor.ConnectionIdentity
	if len(connectionsJSON) > 0 {
		if err := json.Unmarshal(connectionsJSON, &connections); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT a.id::text, a.provider_id::text,
		ARRAY(SELECT DISTINCT p.adapter FROM olp.code_pins p WHERE p.root_id=nullif($2,'')::uuid AND p.account_id=a.id AND p.adapter IS NOT NULL)
		FROM olp.code_accounts a WHERE a.id::text=ANY($1::text[])`, tree, root)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[string][]string{}
	for rows.Next() {
		var accountID, providerID string
		var pinned []string
		if err := rows.Scan(&accountID, &providerID, &pinned); err != nil {
			return nil, err
		}
		adapters := slices.Clone(pinned)
		if cfg, ok := connections[providerID]; ok {
			if v, ok := vendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID); ok && !slices.Contains(adapters, string(v.Adapter)) {
				adapters = append(adapters, string(v.Adapter))
			}
		}
		used[accountID] = adapters
	}
	return used, rows.Err()
}

func selectAccount(ctx context.Context, tx pgx.Tx, in Admission, mode bindMode, id, root string, tree []string, bandOrdered bool) (selectionResult, error) {
	family := ""
	effectiveOrdering := bandOrdered && family != ""
	var none selectionResult
	if _, paid := in.Route.PaidTargets[in.Operation.Model]; paid {
		return none, code.Refuse(503, "code_account_unavailable")
	}
	required := []string{in.Operation.Model}
	if mode == modeConnect {
		required = in.Route.Models
	}
	if id != "" && mode.isConnection() {
		required = []string{}
	}
	cooldownModels := []string{in.Operation.Model}
	if mode == modeConnect {
		if id != "" {
			cooldownModels = []string{}
		} else {
			cooldownModels = in.Route.Models
		}
	}
	requiredJSON, err := json.Marshal(required)
	if err != nil {
		return none, err
	}
	routeID, err := uuid.Parse(in.Route.ID)
	if err != nil {
		return none, err
	}
	usedByTree, err := treeUsedAdapters(ctx, tx, in.Route.RevisionID, root, tree)
	if err != nil {
		return none, err
	}
	rows, err := tx.Query(ctx, `SELECT a.id::text,a.project_id::text,a.provider_id::text,a.credential_id::text,a.principal,a.models,a.name,a.enabled,a.etag::text,a.health,a.allowance,
		olp.code_account_available(a,$8,$9),
		(a.health IN ('unavailable','quota_limited') AND NOT coalesce(a.unavailable_until<=now(),false)),
		coalesce(array_position($6::text[],a.id::text),2147483647),
		pa.weight,a.unavailable_until,
		EXISTS(SELECT 1 FROM olp.code_account_cooldowns cd WHERE cd.account_id=a.id AND cd.retry_at>now() AND (cd.model IS NULL OR cd.model=ANY($7::text[]))),
		(SELECT max(cd.retry_at) FROM olp.code_account_cooldowns cd WHERE cd.account_id=a.id AND cd.retry_at>now() AND (cd.model IS NULL OR cd.model=ANY($7::text[]))),
		a.extra_usage_enabled,a.cycle_anchor::text,v.connections->(a.provider_id::text)
		FROM olp.code_accounts a JOIN olp.code_pool_accounts pa ON pa.account_id=a.id
		JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=a.provider_id
		JOIN olp.code_route_revisions v ON v.id=$10
		WHERE pa.pool_id=$1 AND a.project_id=$2 AND a.enabled AND p.state<>'disabled' AND p.project_id=a.project_id
		AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())
		AND ($11::uuid IS NULL OR NOT EXISTS(SELECT 1 FROM olp.code_recovery_chain($11) rejected WHERE rejected.account_id=a.id AND rejected.nonexecution))
		AND a.models @> $3::jsonb AND ($4='' OR a.id::text=$4) AND a.provider_id::text=ANY($5::text[])`,
		in.Route.PoolID, in.Route.ProjectID, requiredJSON, id, in.providers(), tree, cooldownModels, effectiveOrdering, family, in.Route.RevisionID, recoveryExclusion(in.Recovery))
	if err != nil {
		return none, err
	}
	now := time.Now().UTC()
	var candidates []accountCandidate
	for rows.Next() {
		var candidate accountCandidate
		var weight int64
		var cooling bool
		var connJSON []byte
		if err := rows.Scan(
			&candidate.account.ID, &candidate.account.ProjectID, &candidate.account.ProviderID, &candidate.account.CredentialID,
			&candidate.account.Principal, &candidate.account.Models, &candidate.account.Name, &candidate.account.Enabled,
			&candidate.account.ETag, &candidate.account.Health, &candidate.account.Allowance,
			&candidate.outcome.Available, &candidate.outcome.Cooldown, &candidate.outcome.Position,
			&weight, &candidate.account.UnavailableUntil, &cooling, &candidate.cooldownRetry,
			&candidate.account.ExtraUsageEnabled, &candidate.account.CycleAnchor, &connJSON,
		); err != nil {
			rows.Close()
			return none, err
		}
		var candidateAdapter string
		if len(connJSON) > 0 {
			var cfg vendor.ConnectionIdentity
			if err := json.Unmarshal(connJSON, &cfg); err != nil {
				rows.Close()
				return none, err
			}
			if v, ok := vendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID); ok {
				candidateAdapter = string(v.Adapter)
			}
		}
		if excludedByTreeAdapter(tree, usedByTree, candidate.account.ID, candidateAdapter) {
			continue
		}
		accountID, err := uuid.Parse(candidate.account.ID)
		if err != nil {
			rows.Close()
			return none, err
		}
		var assessment *code.BandAssessment
		var band code.Band
		var resetAt *time.Time
		if effectiveOrdering {
			assessed := code.AssessBand(candidate.account.Allowance, family, now)
			assessment, band, resetAt = &assessed, assessed.Band, assessed.SoonestReset
		}
		outcome := code.Candidate{
			AccountID: candidate.account.ID,
			Available: candidate.outcome.Available && !cooling,
			Cooldown:  candidate.outcome.Cooldown || cooling,
			Position:  candidate.outcome.Position,
			Score:     routing.Score(routeID, accountID, weight, in.Operation.Name, "code", in.Operation.Model, []byte(in.APIKeyID+":"+in.Operation.Identity.Conversation)),
			Band:      band,
			ResetAt:   resetAt,
		}
		candidates = append(candidates, accountCandidate{account: candidate.account, outcome: outcome, cooldownRetry: candidate.cooldownRetry, assessment: assessment})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return none, err
	}
	outcomes := slices.Collect(func(yield func(code.Candidate) bool) {
		for _, candidate := range candidates {
			if !yield(candidate.outcome) {
				return
			}
		}
	})
	chosen, ok := code.ChooseCandidate(outcomes)
	if !ok || !chosen.Eligible() {
		if len(candidates) == 0 {
			return none, code.Refuse(503, "code_account_unavailable")
		}
		deadlines := slices.Collect(func(yield func(*time.Time) bool) {
			for _, candidate := range candidates {
				if candidate.outcome.Position != chosen.Position {
					continue
				}
				if (candidate.account.Health == code.HealthUnavailable || candidate.account.Health == code.HealthQuotaLimited) && candidate.account.UnavailableUntil == nil {
					if !yield(nil) {
						return
					}
					continue
				}
				var unavailableUntil *time.Time
				if candidate.account.Health == code.HealthUnavailable || candidate.account.Health == code.HealthQuotaLimited {
					unavailableUntil = candidate.account.UnavailableUntil
				}
				if !yield(code.BlockingDeadline(now, unavailableUntil, candidate.cooldownRetry, candidate.account.Allowance, effectiveOrdering, family)) {
					return
				}
			}
		})
		return selectionResult{candidates: outcomes}, &code.Refusal{Status: 503, Code: "code_account_unavailable", RetryAt: code.EarliestRecovery(now, deadlines...)}
	}
	index := slices.IndexFunc(candidates, func(candidate accountCandidate) bool {
		return candidate.account.ID == chosen.AccountID
	})
	return selectionResult{account: candidates[index].account, candidates: outcomes, assessment: candidates[index].assessment}, nil
}

func excludedByTreeAdapter(tree []string, used map[string][]string, candidateID, candidateAdapter string) bool {
	pos := slices.Index(tree, candidateID)
	limit := len(tree)
	if pos >= 0 {
		limit = pos
	}
	for _, earlier := range tree[:limit] {
		adapters, present := used[earlier]
		if !present {
			continue
		}
		if candidateAdapter == "" && len(adapters) == 0 || candidateAdapter != "" && slices.Contains(adapters, candidateAdapter) {
			return true
		}
	}
	return false
}

func recordDecision(ctx context.Context, tx pgx.Tx, in Admission, binding code.Binding, attemptID *string, selection selectionResult, reason code.DecisionReason) error {
	chosen, ok := selection.chosen()
	if !ok {
		return fmt.Errorf("code decision chosen account missing from candidates")
	}
	scope := code.DecisionScope{
		ID:              access.NewID(),
		ProjectID:       in.Route.ProjectID,
		RouteID:         in.Route.ID,
		RouteRevisionID: in.Route.RevisionID,
		APIKeyID:        in.APIKeyID,
		BindingID:       binding.ID,
		RootID:          binding.RootID,
		ChosenAccountID: selection.account.ID,
		Skipped:         code.SkippedAccounts(selection.candidates, chosen),
	}
	var decision code.Decision
	var err error
	if reason == code.DecisionConnection {
		decision, err = code.NewConnectionDecision(scope)
	} else if attemptID != nil {
		decision, err = code.NewAttemptDecision(scope, *attemptID, reason, selection.assessment, in.Recovery)
	} else {
		return fmt.Errorf("missing attempt ID for code decision")
	}
	if err != nil {
		return err
	}
	skipped, err := json.Marshal(decision.Skipped)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.code_decisions(id,attempt_id,project_id,route_id,route_revision_id,api_key_id,binding_id,root_id,chosen_account_id,reason,band,evidence,evidence_observed_at,skipped,recovery_envelope,recovery_ordinal)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		decision.ID, decision.AttemptID, decision.ProjectID, decision.RouteID, decision.RouteRevisionID, decision.APIKeyID, decision.BindingID, decision.RootID, decision.ChosenAccountID, decision.Reason, decision.Band, decision.Evidence, decision.EvidenceObservedAt, skipped, decision.RecoveryEnvelope, decision.RecoveryOrdinal)
	return err
}
