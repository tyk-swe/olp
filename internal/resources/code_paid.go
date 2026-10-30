package resources

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
)

type PaidCodeAdmission struct {
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
	ID           string
	ParentID     *string
	RootID       string
	Model        string
	ProviderID   string
	SlotID       string
	CredentialID string
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
	if err := codeAuthority(ctx, tx, in.CodeAdmission, &authority); err != nil {
		return PaidCodePermit{}, err
	}
	if !authority.AllowsAttribution(in.Attribution) {
		return PaidCodePermit{}, codemode.Refuse(409, "code_attribution_changed")
	}
	var current codemode.Route
	var document []byte
	if err := tx.QueryRow(ctx, `SELECT v.document FROM olp.code_routes r JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id WHERE r.id=$1`, in.Route.ID).Scan(&document); err != nil {
		return PaidCodePermit{}, err
	}
	if err := json.Unmarshal(document, &current); err != nil {
		return PaidCodePermit{}, err
	}
	target, exists := current.PaidTargets[in.Operation.Model]
	frozen, selected := in.Route.PaidTargets[in.Operation.Model]
	if !current.PaidParticipation || !exists || !selected || target != frozen || target.ProviderID != in.Provider.ID || target.SlotID != in.Slot.ID || in.Slot.CredentialID == nil || in.Slot.CredentialVersion == nil {
		return PaidCodePermit{}, codemode.Refuse(403, "code_paid_target_denied")
	}
	if err := paidCodeTarget(ctx, tx, in, true); err != nil {
		return PaidCodePermit{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, in.Route.ID+":"+in.APIKeyID); err != nil {
		return PaidCodePermit{}, err
	}
	binding, err := paidCodeBind(ctx, tx, in)
	if err != nil {
		return PaidCodePermit{}, err
	}
	attempt := codemode.Attempt{ID: access.NewID(), ProjectID: in.Route.ProjectID, RouteID: in.Route.ID, RouteRevisionID: in.Route.RevisionID, APIKeyID: in.APIKeyID, Attribution: in.Attribution, EndUserDigest: in.EndUserDigest, Operation: in.Operation.Name, Model: in.Operation.Model, State: "prepared"}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.code_attempts(id,project_id,route_id,route_revision_id,api_key_id,paid_binding_id,paid_provider_id,paid_slot_id,requested_model,operation,model,state,reserved_tokens,end_user_digest,attribution,paid_cost_owners)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'prepared',0,$12,$13,$14)`, attempt.ID, in.Route.ProjectID, in.Route.ID, in.Route.RevisionID, in.APIKeyID, binding.ID, target.ProviderID, target.SlotID, in.Operation.PublicModel(), in.Operation.Name, in.Operation.Model, in.EndUserDigest, attribution.JSON(in.Attribution), paidCodeOwners(authority, in)); err != nil {
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

func paidCodeBind(ctx context.Context, tx pgx.Tx, in PaidCodeAdmission) (paidCodeBinding, error) {
	identity := in.Operation.Identity
	if in.PreviousResponse != "" {
		var parent string
		err := tx.QueryRow(ctx, `SELECT b.conversation FROM olp.code_paid_references r JOIN olp.code_paid_bindings b ON b.id=r.binding_id JOIN olp.code_paid_bindings root ON root.id=b.root_id WHERE r.route_id=$1 AND r.api_key_id=$2 AND r.external_id=$3 AND NOT r.ambiguous AND b.model=$4 AND b.provider_id=$5 AND b.slot_id=$6 AND b.credential_id=$7 AND root.retired_at IS NULL`, in.Route.ID, in.APIKeyID, in.PreviousResponse, in.Operation.PublicModel(), in.Provider.ID, in.Slot.ID, *in.Slot.CredentialID).Scan(&parent)
		if errors.Is(err, pgx.ErrNoRows) {
			return paidCodeBinding{}, codemode.Refuse(409, "code_response_account_conflict")
		}
		if err != nil {
			return paidCodeBinding{}, err
		}
		if identity.Parent != "" && identity.Parent != parent {
			return paidCodeBinding{}, codemode.Refuse(409, "code_parent_conflict")
		}
		resolved := in
		resolved.Operation.Identity = codemode.Identity{Conversation: identity.Conversation, Parent: parent}
		resolved.PreviousResponse = ""
		return paidCodeBind(ctx, tx, resolved)
	}
	var subscription bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_bindings b JOIN olp.code_pins p ON p.root_id=b.root_id WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3 AND p.model=$4)`, in.Route.ID, in.APIKeyID, identity.Conversation, in.Operation.PublicModel()).Scan(&subscription); err != nil {
		return paidCodeBinding{}, err
	}
	if subscription {
		return paidCodeBinding{}, codemode.Refuse(409, "code_paid_target_conflict")
	}
	binding, err := paidCodeConversation(ctx, tx, in, identity.Conversation)
	if err == nil {
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
	if !errors.Is(err, pgx.ErrNoRows) {
		return binding, err
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
	if _, err := tx.Exec(ctx, `INSERT INTO olp.code_paid_bindings(id,project_id,route_id,api_key_id,conversation,parent_id,root_id,model,selected_model,provider_id,slot_id,credential_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, in.Route.ProjectID, in.Route.ID, in.APIKeyID, identity.Conversation, parentID, root, in.Operation.PublicModel(), in.Operation.Model, in.Provider.ID, in.Slot.ID, *in.Slot.CredentialID); err != nil {
		return binding, err
	}
	return paidCodeBinding{ID: id, ParentID: parentID, RootID: root, Model: in.Operation.Model, ProviderID: in.Provider.ID, SlotID: in.Slot.ID, CredentialID: *in.Slot.CredentialID}, nil
}

func paidCodeConversation(ctx context.Context, tx pgx.Tx, in PaidCodeAdmission, conversation string) (paidCodeBinding, error) {
	var binding paidCodeBinding
	var retired bool
	err := tx.QueryRow(ctx, `SELECT b.id::text,b.parent_id::text,b.root_id::text,b.selected_model,b.provider_id::text,b.slot_id::text,b.credential_id::text,root.retired_at IS NOT NULL FROM olp.code_paid_bindings b JOIN olp.code_paid_bindings root ON root.id=b.root_id WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3 AND b.model=$4 FOR SHARE OF root`, in.Route.ID, in.APIKeyID, conversation, in.Operation.PublicModel()).Scan(&binding.ID, &binding.ParentID, &binding.RootID, &binding.Model, &binding.ProviderID, &binding.SlotID, &binding.CredentialID, &retired)
	if err == nil && retired {
		return binding, codemode.Refuse(410, "code_binding_retired")
	}
	return binding, err
}

func samePaidSlotLimits(live, frozen routing.Slot) bool {
	return reflect.DeepEqual(live.RequestsPerMinute, frozen.RequestsPerMinute) && reflect.DeepEqual(live.TokensPerMinute, frozen.TokensPerMinute) && reflect.DeepEqual(live.MaxConcurrency, frozen.MaxConcurrency) && reflect.DeepEqual(live.Supply, frozen.Supply)
}

func paidCodeTarget(ctx context.Context, db access.Queryer, in PaidCodeAdmission, lock bool) error {
	locking := ""
	if lock {
		locking = " FOR SHARE OF p,s,c"
	}
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
	if !liveSlot.Allows(in.Operation.Model, in.Route.Slug, in.APIKeyID) || !in.Slot.Allows(in.Operation.Model, in.Route.Slug, in.APIKeyID) || !reflect.DeepEqual(configuration.Options.Limits, in.Provider.Limits) || !samePaidSlotLimits(slotLimits, in.Slot) {
		return codemode.Refuse(403, "code_paid_target_denied")
	}

	return nil
}
