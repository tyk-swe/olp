package resources

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
)

type PaidCodeAdmission struct {
	Fallback bool
	CodeAdmission
	Authority access.Authority
	Provider  routing.Provider
	Slot      routing.Slot
}

type PaidCodePermit struct {
	Authority access.Authority
	Attempt   codemode.Attempt
	BindingID string
}

type paidCodeBinding struct {
	ID                 string
	ParentID           *string
	RootID             string
	Model              string
	ProviderID         string
	SlotID             string
	CredentialID       string
	SubscriptionRootID *string
}

func (s *CodeStore) AdmitPaid(ctx context.Context, in PaidCodeAdmission) (PaidCodePermit, error) {
	if err := validateCodeAdmission(in.CodeAdmission); err != nil {
		return PaidCodePermit{}, err
	}
	if !in.Route.PaidParticipation {
		return PaidCodePermit{}, codemode.Refuse(403, "code_paid_participation_disabled")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return PaidCodePermit{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM olp.installation FOR SHARE`); err != nil {
		return PaidCodePermit{}, err
	}
	authority := in.Authority
	current, authorityErr := codeAuthorityDocument(ctx, tx, in.CodeAdmission, &authority, codePaidInference)
	if authorityErr != nil {
		return PaidCodePermit{}, authorityErr
	}
	if !authority.AllowsAttribution(in.Attribution) {
		return PaidCodePermit{}, codemode.Refuse(409, "code_attribution_changed")
	}
	target := current.PaidTarget(in.Operation.Model, in.Fallback)
	frozen := in.Route.PaidTarget(in.Operation.Model, in.Fallback)
	if in.Fallback && current.InitialRecoveryAttempts < 1 {
		return PaidCodePermit{}, codemode.Refuse(409, "code_recovery_policy_changed")
	}
	if !current.PaidParticipation || target.ProviderID == "" || frozen.ProviderID == "" || target != frozen || target.ProviderID != in.Provider.ID || target.SlotID != in.Slot.ID || in.Slot.CredentialID == nil || in.Slot.CredentialVersion == nil {
		return PaidCodePermit{}, codemode.Refuse(403, "code_paid_target_denied")
	}
	if err := paidCodeTarget(ctx, tx, in, true); err != nil {
		return PaidCodePermit{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, in.Route.ID+":"+in.APIKeyID); err != nil {
		return PaidCodePermit{}, err
	}
	binding, err := paidCodeBind(ctx, tx, in, current)
	if err != nil {
		return PaidCodePermit{}, err
	}
	attempt := codemode.Attempt{ID: access.NewID(), ProjectID: in.Route.ProjectID, RouteID: in.Route.ID, RouteRevisionID: in.Route.RevisionID, APIKeyID: in.APIKeyID, Attribution: in.Attribution, EndUserDigest: in.EndUserDigest, Operation: in.Operation.Name, Model: in.Operation.Model, State: "prepared"}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.code_attempts(id,project_id,route_id,route_revision_id,api_key_id,paid_binding_id,paid_provider_id,paid_slot_id,requested_model,operation,model,state,reserved_tokens,end_user_digest,attribution,paid_cost_owners,recovery_from)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'prepared',0,$12,$13,$14,$15)`, attempt.ID, in.Route.ProjectID, in.Route.ID, in.Route.RevisionID, in.APIKeyID, binding.ID, target.ProviderID, target.SlotID, in.Operation.PublicModel(), in.Operation.Name, in.Operation.Model, in.EndUserDigest, attribution.JSON(in.Attribution), paidCodeOwners(authority, in), codeRecoveryFrom(in.Recovery)); err != nil {
		return PaidCodePermit{}, err
	}
	if err := limits.ReserveCodeTokens(ctx, tx, attempt, in.Bound); err != nil {
		return PaidCodePermit{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PaidCodePermit{}, err
	}
	return PaidCodePermit{Authority: authority, Attempt: attempt, BindingID: binding.ID}, nil
}

func paidCodeOwners(authority access.Authority, in PaidCodeAdmission) []string {
	return slices.Collect(func(yield func(string) bool) {
		for _, id := range []string{authority.ID, in.Route.ID, in.Provider.ID, in.Slot.ID, limits.KeyRouteBudgetID(authority.ID, in.Route.Slug), limits.AggregateBudgetID("project", in.Route.ProjectID)} {
			if !yield(id) {
				return
			}
		}
		if authority.BudgetGroupID != nil {
			if !yield(*authority.BudgetGroupID) {
				return
			}
		}
		if authority.InstallationID != "" {
			if !yield(limits.AggregateBudgetID("installation", authority.InstallationID)) {
				return
			}
		}
		if authority.OrganizationID != nil {
			if !yield(limits.AggregateBudgetID("organization", *authority.OrganizationID)) {
				return
			}
		}
		for key, value := range in.Attribution {
			if !yield(limits.AttributionBudgetID(in.Route.ProjectID, key, value)) {
				return
			}
		}
		if in.EndUserDigest != "" {
			for _, id := range []string{limits.EndUserKeyID(authority.ID, in.EndUserDigest), limits.EndUserProjectID(in.Route.ProjectID, in.EndUserDigest)} {
				if !yield(id) {
					return
				}
			}
		}
	})
}

func paidCodeBind(ctx context.Context, tx pgx.Tx, in PaidCodeAdmission, current codemode.Route) (paidCodeBinding, error) {
	requested := in.Operation.Identity
	binding, bindingErr := paidCodeConversation(ctx, tx, in, requested.Conversation)
	if bindingErr != nil && !errors.Is(bindingErr, pgx.ErrNoRows) {
		return binding, bindingErr
	}
	var referenceConversation, referenceRoot string
	if in.PreviousResponse != "" {
		err := tx.QueryRow(ctx, `SELECT b.conversation,b.root_id::text FROM olp.code_paid_references r JOIN olp.code_paid_bindings b ON b.id=r.binding_id JOIN olp.code_paid_bindings root ON root.id=b.root_id WHERE r.route_id=$1 AND r.api_key_id=$2 AND r.external_id=$3 AND NOT r.ambiguous AND b.model=$4 AND b.provider_id=$5 AND b.slot_id=$6 AND b.credential_id=$7 AND root.retired_at IS NULL`, in.Route.ID, in.APIKeyID, in.PreviousResponse, in.Operation.PublicModel(), in.Provider.ID, in.Slot.ID, *in.Slot.CredentialID).Scan(&referenceConversation, &referenceRoot)
		if errors.Is(err, pgx.ErrNoRows) {
			return binding, codemode.Refuse(409, "code_response_account_conflict")
		}
		if err != nil {
			return binding, err
		}
		if referenceConversation != requested.Conversation && (requested.Generic() || (codemode.Identity{Conversation: referenceConversation}).Generic()) {
			return binding, codemode.Refuse(409, "code_response_account_conflict")
		}
		if bindingErr == nil && binding.RootID != referenceRoot {
			return binding, codemode.Refuse(409, "code_response_account_conflict")
		}
	}
	identity := func() codemode.Identity {
		if bindingErr == nil || requested.Parent != "" || referenceConversation == "" || referenceConversation == requested.Conversation {
			return requested
		}
		return codemode.Identity{Conversation: requested.Conversation, Parent: referenceConversation}
	}()
	var subscription bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_bindings b JOIN olp.code_pins p ON p.root_id=b.root_id WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3 AND p.model=$4)`, in.Route.ID, in.APIKeyID, identity.Conversation, in.Operation.PublicModel()).Scan(&subscription); err != nil {
		return paidCodeBinding{}, err
	}
	source, sourceErr := func() (*string, error) {
		if !subscription {
			if in.Fallback && bindingErr != nil && identity.Parent == "" {
				return nil, codePaidInitialFallback(ctx, tx, in, current)
			}
			return nil, nil
		}
		if !in.Fallback {
			return nil, codemode.Refuse(409, "code_paid_target_conflict")
		}
		if bindingErr == nil && binding.SubscriptionRootID != nil {
			return binding.SubscriptionRootID, nil
		}
		owner, err := codeRecoverySource(ctx, tx, in.CodeAdmission)
		if err != nil {
			return nil, err
		}
		pins, err := codePins(ctx, tx, owner.root)
		if err != nil {
			return nil, err
		}
		operations, err := in.Route.RecoveryModels(in.Operation, pins)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(operations, func(operation codemode.Operation) bool { return operation.Model == in.Operation.Model }) {
			return nil, codemode.Refuse(409, "code_recovery_unsafe")
		}
		return &owner.root, liveCodeTree(ctx, tx, codemode.Binding{RootID: owner.root})
	}()
	if sourceErr != nil {
		return paidCodeBinding{}, sourceErr
	}
	if in.Recovery != nil && source == nil {
		return paidCodeBinding{}, codemode.Refuse(409, "code_recovery_unsafe")
	}
	if bindingErr == nil {
		if binding.Model != in.Operation.Model || binding.ProviderID != in.Provider.ID || binding.SlotID != in.Slot.ID || binding.CredentialID != *in.Slot.CredentialID {
			return binding, codemode.Refuse(409, "code_paid_target_conflict")
		}
		if identity.Parent != "" {
			var parent string
			if err := tx.QueryRow(ctx, `SELECT conversation FROM olp.code_paid_bindings WHERE id=$1`, binding.ParentID).Scan(&parent); err != nil || parent != identity.Parent {
				return binding, codemode.Refuse(409, "code_parent_conflict")
			}
		}
		return binding, nil
	}
	parent, parentErr := func() (paidCodeBinding, error) {
		if identity.Parent == "" {
			return paidCodeBinding{}, nil
		}
		return paidCodeConversation(ctx, tx, in, identity.Parent)
	}()
	if errors.Is(parentErr, pgx.ErrNoRows) {
		return binding, codemode.Refuse(409, "code_parent_unresolved")
	}
	if parentErr != nil {
		return binding, parentErr
	}
	if referenceRoot != "" && parent.RootID != referenceRoot {
		return binding, codemode.Refuse(409, "code_response_account_conflict")
	}
	if parent.ID != "" && (parent.ProviderID != in.Provider.ID || parent.SlotID != in.Slot.ID || parent.CredentialID != *in.Slot.CredentialID) {
		return binding, codemode.Refuse(409, "code_paid_target_conflict")
	}
	id := access.NewID()
	root := func() string {
		if parent.ID != "" {
			return parent.RootID
		}
		return id
	}()
	parentID := func() *string {
		if parent.ID != "" {
			return &parent.ID
		}
		return nil
	}()
	if _, err := tx.Exec(ctx, `INSERT INTO olp.code_paid_bindings(id,project_id,route_id,api_key_id,conversation,parent_id,root_id,model,selected_model,provider_id,slot_id,credential_id,subscription_root_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, id, in.Route.ProjectID, in.Route.ID, in.APIKeyID, identity.Conversation, parentID, root, in.Operation.PublicModel(), in.Operation.Model, in.Provider.ID, in.Slot.ID, *in.Slot.CredentialID, source); err != nil {
		return binding, err
	}
	return paidCodeBinding{ID: id, ParentID: parentID, RootID: root, Model: in.Operation.Model, ProviderID: in.Provider.ID, SlotID: in.Slot.ID, CredentialID: *in.Slot.CredentialID}, nil
}

func paidCodeConversation(ctx context.Context, tx pgx.Tx, in PaidCodeAdmission, conversation string) (paidCodeBinding, error) {
	var binding paidCodeBinding
	var retired bool
	err := tx.QueryRow(ctx, `SELECT b.id::text,b.parent_id::text,b.root_id::text,b.selected_model,b.provider_id::text,b.slot_id::text,b.credential_id::text,root.subscription_root_id::text,root.retired_at IS NOT NULL FROM olp.code_paid_bindings b JOIN olp.code_paid_bindings root ON root.id=b.root_id WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3 AND b.model=$4 FOR SHARE OF root`, in.Route.ID, in.APIKeyID, conversation, in.Operation.PublicModel()).Scan(&binding.ID, &binding.ParentID, &binding.RootID, &binding.Model, &binding.ProviderID, &binding.SlotID, &binding.CredentialID, &binding.SubscriptionRootID, &retired)
	if err == nil && retired {
		return binding, codemode.Refuse(410, "code_binding_retired")
	}
	if err == nil && binding.SubscriptionRootID != nil {
		return binding, liveCodeTree(ctx, tx, codemode.Binding{RootID: *binding.SubscriptionRootID})
	}
	return binding, err
}

func paidCodeTarget(ctx context.Context, db access.Queryer, in PaidCodeAdmission, lock bool) error {
	locking := func() string {
		if lock {
			return " FOR SHARE OF p,s,c"
		}
		return ""
	}()
	var liveRestrictions, liveConfiguration, liveLimits []byte
	err := db.QueryRow(ctx, `SELECT s.restrictions,p.configuration,s.limits FROM olp.providers p
 JOIN olp.provider_slots s ON s.provider_id=p.id JOIN olp.provider_credentials c ON c.id=s.credential_id AND c.provider_id=p.id
 LEFT JOIN olp.provider_grants g ON g.credential_id=c.id
 WHERE p.id=$1 AND p.project_id=$2 AND p.state='active' AND p.active_revision_id=$3
 AND s.id=$4 AND s.enabled AND s.credential_id=$5 AND c.revoked_at IS NULL AND c.version=$6
 AND (g.credential_id IS NULL OR (g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())))
 AND EXISTS(SELECT 1 FROM olp.provider_models m WHERE m.provider_id=p.id AND m.upstream_model=$7 AND m.enabled)
 `+locking, in.Provider.ID, in.Route.ProjectID, in.Provider.RevisionID, in.Slot.ID, *in.Slot.CredentialID, *in.Slot.CredentialVersion, in.Operation.Model).Scan(&liveRestrictions, &liveConfiguration, &liveLimits)
	if errors.Is(err, pgx.ErrNoRows) {
		return codemode.Refuse(403, "code_paid_target_denied")
	}
	if err != nil {
		return err
	}
	var liveSlot routing.Slot
	var slotLimits routing.Slot
	if err := json.Unmarshal(liveLimits, &slotLimits); err != nil {
		return err
	}
	var configuration routing.Configuration
	if err := json.Unmarshal(liveRestrictions, &liveSlot); err != nil {
		return err
	}
	if err := json.Unmarshal(liveConfiguration, &configuration); err != nil {
		return err
	}
	liveSlot.Enabled = true
	if !liveSlot.Allows(in.Operation.Model, in.Route.Slug, in.APIKeyID) || !in.Slot.Allows(in.Operation.Model, in.Route.Slug, in.APIKeyID) || !routing.CodePaidLimitsMatch(configuration.Options.Limits, in.Provider.Limits, slotLimits, in.Slot) {
		return codemode.Refuse(403, "code_paid_target_denied")
	}

	return nil
}

func (s *CodeStore) ResolvePaidModel(ctx context.Context, in CodeAdmission, verified access.Authority) (codemode.Operation, error) {
	if in.Operation.PublicModel() == in.Operation.Model {
		return in.Operation, nil
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return codemode.Operation{}, err
	}
	defer tx.Rollback(ctx)
	var selected *string
	err = tx.QueryRow(ctx, `SELECT (SELECT b.selected_model FROM olp.code_paid_bindings b JOIN olp.code_paid_bindings root ON root.id=b.root_id WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.model=$3 AND root.retired_at IS NULL AND (b.conversation=$4 OR b.conversation=$5 OR b.id=(SELECT r.binding_id FROM olp.code_paid_references r WHERE r.route_id=$1 AND r.api_key_id=$2 AND r.external_id=$6 AND NOT r.ambiguous)) ORDER BY CASE WHEN b.conversation=$4 THEN 0 WHEN b.conversation=$5 THEN 1 ELSE 2 END LIMIT 1)`, in.Route.ID, in.APIKeyID, in.Operation.PublicModel(), in.Operation.Identity.Conversation, in.Operation.Identity.Parent, in.PreviousResponse).Scan(&selected)
	if err != nil {
		return codemode.Operation{}, err
	}
	pins := func() []codemode.Pin {
		if selected == nil {
			return nil
		}
		return []codemode.Pin{{Model: in.Operation.PublicModel(), TargetModel: *selected}}
	}()
	operation, err := in.Route.ResolvePinnedModel(in.Operation, pins)
	if err != nil {
		return codemode.Operation{}, err
	}
	fresh := verified
	if err := codeAuthorityFor(ctx, tx, in.withOperation(operation), &fresh, codePaidInference); err != nil {
		return codemode.Operation{}, err
	}
	return operation, nil
}

func (s *CodeStore) ResolvePaidFallback(ctx context.Context, in CodeAdmission, verified access.Authority) (*codemode.Operation, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if in.Recovery == nil {
		var selected *string
		err := tx.QueryRow(ctx, `SELECT (SELECT b.selected_model FROM olp.code_paid_bindings b WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.model=$3 AND (b.conversation=$4 OR b.conversation=$5 OR b.id=(SELECT r.binding_id FROM olp.code_paid_references r WHERE r.route_id=$1 AND r.api_key_id=$2 AND r.external_id=$6 AND NOT r.ambiguous)) ORDER BY CASE WHEN b.conversation=$4 THEN 0 WHEN b.conversation=$5 THEN 1 ELSE 2 END LIMIT 1)`, in.Route.ID, in.APIKeyID, in.Operation.PublicModel(), in.Operation.Identity.Conversation, in.Operation.Identity.Parent, in.PreviousResponse).Scan(&selected)
		if err != nil {
			return nil, err
		}
		if selected == nil {
			if !in.InitialRecovery || in.Operation.Identity.Parent != "" || in.PreviousResponse != "" || in.Route.InitialRecoveryAttempts < 1 {
				return nil, nil
			}
			return in.Route.PaidFallbackModel(in.Operation, nil)
		}
		operation, err := in.Route.ResolvePinnedModel(in.Operation, []codemode.Pin{{Model: in.Operation.PublicModel(), TargetModel: *selected}})
		if err != nil {
			return nil, err
		}
		if _, allowed := in.Route.PaidFallbackTargets[operation.Model]; !allowed {
			return nil, codemode.Refuse(403, "code_paid_target_denied")
		}
		return &operation, nil
	}
	owner, err := codeRecoverySource(ctx, tx, in)
	if err != nil {
		return nil, err
	}
	pins, err := codePins(ctx, tx, owner.root)
	if err != nil {
		return nil, err
	}
	return in.Route.PaidFallbackModel(in.Operation, pins)
}

func codePaidInitialFallback(ctx context.Context, tx pgx.Tx, in PaidCodeAdmission, current codemode.Route) error {
	if !in.InitialRecovery || in.Route.InitialRecoveryAttempts < 1 || in.Operation.Identity.Parent != "" || in.PreviousResponse != "" || in.Recovery != nil {
		return codemode.Refuse(409, "code_recovery_unsafe")
	}
	var occupied bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_bindings WHERE route_id=$1 AND api_key_id=$2 AND conversation=$3) OR EXISTS(SELECT 1 FROM olp.code_paid_bindings WHERE route_id=$1 AND api_key_id=$2 AND conversation=$3)`, in.Route.ID, in.APIKeyID, in.Operation.Identity.Conversation).Scan(&occupied)
	if err != nil {
		return err
	}
	if occupied {
		return codemode.Refuse(409, "code_recovery_unsafe")
	}
	public := codemode.Operation{Name: in.Operation.Name, Model: in.Operation.PublicModel(), Identity: in.Operation.Identity}
	operations, err := in.Route.RecoveryModels(public, nil)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(operations, func(operation codemode.Operation) bool { return operation.Model == in.Operation.Model }) {
		return codemode.Refuse(409, "code_recovery_unsafe")
	}
	currentOperations, err := current.RecoveryModels(public, nil)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(currentOperations, func(operation codemode.Operation) bool { return operation.Model == in.Operation.Model }) {
		return codemode.Refuse(409, "code_recovery_policy_changed")
	}
	_, _, availabilityErr := codeRecoveryDestination(ctx, tx, in.CodeAdmission, operations)
	if availabilityErr == nil {
		return codemode.Refuse(409, "code_paid_fallback_not_needed")
	}
	refusal, ok := errors.AsType[*codemode.Refusal](availabilityErr)
	if !ok || refusal.Code != "code_account_unavailable" {
		return availabilityErr
	}
	return nil
}
