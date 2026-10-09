package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

type resolvedSlot struct {
	entry        *SlotEntry
	id           string
	credentialID *string
	// allowedAPIKeys is the destination slot's own key restriction. API keys
	// are not portable, so apply keeps it rather than widening the slot.
	allowedAPIKeys []string
}

func (s *Server) applyDocument(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document, bindings bindingSet) error {
	if err := applyMFAPolicy(ctx, tx, p, doc.RequireLocalMFA); err != nil {
		return err
	}

	state, err := loadState(ctx, tx)
	if err != nil {
		return err
	}
	// Plan resolves a project reference against both the document and the
	// destination, so apply must resolve undeclared destination projects too.
	projectIDs := maps.Clone(state.projects)
	if err := applyOrganizations(ctx, tx, p, doc); err != nil {
		return err
	}
	if err := applyBudgetTimeZone(ctx, tx, p, doc.BudgetTimeZone); err != nil {
		return err
	}
	projectPoliciesChanged, err := applyInstallationBudget(ctx, tx, p, doc.InstallationBudget)
	if err != nil {
		return err
	}
	for _, project := range doc.Projects {
		key := strings.ToLower(project.Name)
		id, exists := projectIDs[key]
		if !exists {
			var err error
			id, err = createProject(ctx, tx, p, project.Name, project.Organization)
			if err != nil {
				return err
			}
			projectIDs[key] = id
		}
		if err := s.applyProjectOrganization(ctx, tx, p, id, project.Organization); err != nil {
			return err
		}
		var current *access.EndUserPolicy
		var currentAttribution *attribution.Policy
		var currentGroups access.RouteGroups
		var currentBudget *access.BudgetPolicy
		var currentTemplates access.LimitTemplates
		var currentCaps access.AttributionBudgets
		if err := tx.QueryRow(ctx, `SELECT end_user_policy,attribution_policy,route_groups,budget_policy,limit_templates,attribution_budgets FROM olp.projects WHERE id=$1 FOR UPDATE`, id).Scan(&current, &currentAttribution, &currentGroups, &currentBudget, &currentTemplates, &currentCaps); err != nil {
			return err
		}
		policy := projectEndUserPolicy(current, project.EndUserDefaults, project.EndUserLimitTemplate)
		templates := currentTemplates
		if project.LimitTemplates != nil {
			templates = *project.LimitTemplates
		}
		if currentCaps.Equal(project.AttributionBudgets) && reflect.DeepEqual(templates, currentTemplates) && reflect.DeepEqual(current, policy) && reflect.DeepEqual(currentAttribution, project.AttributionPolicy) && currentGroups.Equal(project.RouteGroups) && reflect.DeepEqual(currentBudget, project.Budget) {
			continue
		}
		// Promoting admission policy needs the same permission as editing it
		// directly. A configure-only token may still apply unchanged defaults.
		if err := p.Authorize(access.Keys); err != nil {
			return err
		}
		encoded, err := json.Marshal(policy)
		if err != nil {
			return err
		}
		labels, err := json.Marshal(project.AttributionPolicy)
		if err != nil {
			return err
		}
		if project.Budget.Limited() {
			if err := limits.EnsureAggregateBudget(ctx, tx, "project", id); err != nil {
				return err
			}
		}
		if err := project.AttributionBudgets.EnsureAccounts(ctx, tx, id); err != nil {
			return err
		}
		budget, err := json.Marshal(project.Budget)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE olp.projects SET end_user_policy=NULLIF($2::jsonb,'null'::jsonb),etag=$3,updated_at=now(),attribution_policy=NULLIF($4::jsonb,'null'::jsonb),route_groups=$5::jsonb,budget_policy=NULLIF($6::jsonb,'null'::jsonb),limit_templates=$7::jsonb,attribution_budgets=$8::jsonb
   WHERE id=$1`, id, encoded, access.NewID(), labels, project.RouteGroups.JSON(), budget, templates.JSON(), project.AttributionBudgets.JSON())
		if err != nil {
			return err
		}
		projectPoliciesChanged = projectPoliciesChanged || tag.RowsAffected() > 0
	}
	if err := s.applySCIMMappings(ctx, tx, p, doc, projectIDs); err != nil {
		return err
	}
	if err := s.applyWorkloadIssuers(ctx, tx, p, doc, projectIDs); err != nil {
		return err
	}
	for _, project := range doc.Projects {
		var templates access.LimitTemplates
		id := projectIDs[strings.ToLower(project.Name)]
		if err := tx.QueryRow(ctx, "SELECT limit_templates FROM olp.projects WHERE id=$1", id).Scan(&templates); err != nil {
			return err
		}
		if err := access.ValidateTemplateReferences(ctx, tx, id, templates); err != nil {
			return err
		}
	}
	if projectPoliciesChanged {
		if _, err := access.AdvanceAuthority(ctx, tx); err != nil {
			return err
		}
	}

	providerIDs := map[string]string{}
	for name, existing := range state.providers {
		providerIDs[name] = existing.ID
	}
	for i := range doc.Providers {
		entry := &doc.Providers[i]
		var projectID *string
		if entry.Project != nil {
			id := projectIDs[strings.ToLower(*entry.Project)]
			projectID = &id
		}
		configuration, err := json.Marshal(entry.Configuration)
		if err != nil {
			return err
		}
		existing, ok := state.providers[strings.ToLower(entry.Name)]
		var providerID string
		switch {
		case !ok:
			providerID = access.NewID()
			if _, err = tx.Exec(ctx, "INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,$2,$3,'draft',$4,$5,$6,$7,$8)", providerID, entry.Name, entry.Configuration.Kind, configuration, access.NewID(), access.NewID(), p.UserID(), projectID); err != nil {
				return err
			}
		default:
			providerID = existing.ID
			current, err := s.currentProviderEntry(ctx, tx, existing, state.projectOf(existing.ProjectID))
			if err != nil {
				return err
			}
			if !canonicalEqualProvider(entry, current) {
				if _, err = tx.Exec(ctx, "UPDATE olp.providers SET name=$2,configuration=$3,etag=$4,draft_dirty=true,updated_at=now() WHERE id=$1", providerID, entry.Name, configuration, access.NewID()); err != nil {
					return err
				}
			} else {
				if err = s.bindChangedCredentials(ctx, tx, providerID, entry, existing, bindings); err != nil {
					return err
				}
				if err := s.bindNetwork(ctx, tx, providerID, entry, existing, bindings); err != nil {
					return err
				}
				providerIDs[strings.ToLower(entry.Name)] = providerID
				continue
			}
		}
		if err := s.bindNetwork(ctx, tx, providerID, entry, existing, bindings); err != nil {
			return err
		}
		resolved, err := s.resolveSlots(ctx, tx, providerID, entry, existing, ok, bindings)
		if err != nil {
			return err
		}
		if err = s.replaceDraftContents(ctx, tx, providerID, entry, resolved); err != nil {
			return err
		}
		providerIDs[strings.ToLower(entry.Name)] = providerID
	}
	for i := range doc.Routes {
		desired := doc.Routes[i]
		route := &desired
		var projectID *string
		if route.Project != nil {
			id := projectIDs[strings.ToLower(*route.Project)]
			projectID = &id
		}
		draft, staged := state.drafts[routeKey(route.Slug, route.Project)]
		if staged {
			current, err := s.currentRouteEntry(ctx, tx, draft.ID, route, state)
			if err != nil {
				return err
			}
			if canonicalEqualRoute(route, current) {
				continue
			}
		}
		if err := routes.ValidateFidelityPolicy(route.Fidelity, route.ContentPolicy); err != nil {
			return err
		}
		input := routes.DraftInput{Slug: route.Slug, Operations: route.Operations, OverallTimeoutMS: route.OverallTimeoutMS, MaxAttempts: route.MaxAttempts, ContentPolicy: route.ContentPolicy, Fidelity: route.Fidelity,
			CallerCostExempt: route.CallerCostExempt, MaxBodyBytes: route.MaxBodyBytes, Fallbacks: route.Fallbacks, Selectors: route.Selectors, Retry: route.Retry, Affinity: route.Affinity, Budget: route.Budget}
		for _, t := range route.Targets {
			providerID := providerIDs[strings.ToLower(t.Provider)]
			input.Targets = append(input.Targets, routes.TargetInput{ProviderID: &providerID, ProviderModel: &t.ProviderModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: t.TimeoutMS, Tags: t.Tags, Shadow: t.Shadow})
		}
		targets, err := routes.ValidateDraftInput(ctx, tx, &input, projectID, nil)
		if err != nil {
			return err
		}
		operations, _ := json.Marshal(input.Operations)
		encoded, _ := json.Marshal(targets)
		var draftID string
		if staged {
			draftID = draft.ID
			if _, err = tx.Exec(ctx, "UPDATE olp.route_drafts SET state='draft',operations=$3,overall_timeout_ms=$4,max_attempts=$5,targets=$6,content_policy=$7,etag=$8,fidelity=$9,behavior=$10,catalog_expose_upstream_models=$11,updated_at=now() WHERE id=$1 AND slug=$2", draftID, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, input.ContentPolicy, access.NewID(), input.Fidelity, input.Behavior, route.ExposeUpstreamModels); err != nil {
				return err
			}
		} else {
			draftID = access.NewID()
			if _, err = tx.Exec(ctx, "INSERT INTO olp.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,content_policy,etag,created_by,project_id,fidelity,behavior,catalog_expose_upstream_models) VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)", draftID, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, input.ContentPolicy, access.NewID(), p.UserID(), projectID, input.Fidelity, input.Behavior, route.ExposeUpstreamModels); err != nil {
				return err
			}
		}
		if route.RoutingPolicy == nil {
			if _, err = tx.Exec(ctx, "DELETE FROM olp.routing_policies WHERE scope='route-draft' AND scope_id=$1", draftID); err != nil {
				return err
			}
		} else {
			policy, _ := json.Marshal(route.RoutingPolicy)
			if _, err = tx.Exec(ctx, "INSERT INTO olp.routing_policies(scope,scope_id,policy,etag,updated_by) VALUES('route-draft',$1,$2,$3,$4) ON CONFLICT(scope,scope_id) DO UPDATE SET policy=excluded.policy,etag=excluded.etag,updated_by=excluded.updated_by,updated_at=now()", draftID, policy, access.NewID(), p.UserID()); err != nil {
				return err
			}
		}
	}
	if err := applyTemplates(ctx, tx, p, doc, projectIDs); err != nil {
		return err
	}
	if doc.Pricing != nil {
		changed, err := s.pricingChanged(ctx, tx, doc)
		if err != nil {
			return err
		}
		if changed {
			effective, _ := time.Parse(time.RFC3339, doc.Pricing.EffectiveAt)
			if !effective.After(time.Now()) {
				effective = time.Now().UTC()
			}
			prices := make([]usage.Price, 0, len(doc.Pricing.Prices))
			for i, entry := range doc.Pricing.Prices {
				price := usage.Price{VendorID: entry.VendorID, ProviderKind: entry.ProviderKind, Model: entry.Model, Operation: entry.Operation,
					InputPerMillion: entry.InputPerMillion, CachedInputPerMillion: entry.CachedInputPerMillion,
					OutputPerMillion: entry.OutputPerMillion, CacheWriteInputPerMillion: entry.CacheWriteInputPerMillion,
					CacheWrite5MInputPerMillion: entry.CacheWrite5MInputPerMillion,
					CacheWrite1HInputPerMillion: entry.CacheWrite1HInputPerMillion,
					UnitPrice:                   entry.UnitPrice, Currency: entry.Currency}
				if entry.Provider != nil {
					id, ok := providerIDs[strings.ToLower(*entry.Provider)]
					if !ok {
						return access.Invalid("pricing.prices."+strconv.Itoa(i)+".provider", "Price "+strconv.Itoa(i)+" names a provider neither the document nor this installation declares.")
					}
					price.ProviderID = &id
				}
				prices = append(prices, price)
			}
			if _, err = usage.CreateRevision(ctx, tx, p.UserID(), effective, prices, s.VendorKind); err != nil {
				return err
			}
			if _, err = runtime.Publish(ctx, tx, p.UserID()); err != nil {
				return err
			}
		}
	}
	for _, project := range doc.Projects {
		id := projectIDs[strings.ToLower(project.Name)]
		for _, members := range project.RouteGroups {
			var foreign bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r WHERE slug=ANY($1::text[]) AND project_id IS DISTINCT FROM $2::uuid)`, members, id).Scan(&foreign); err != nil {
				return err
			}
			if foreign {
				return access.Invalid("route_groups", "Group routes must belong to their project.")
			}
		}
	}

	return applyCatalogPolicies(ctx, tx, p, doc)
}

func (s *Server) resolveSlots(ctx context.Context, tx pgx.Tx, providerID string, entry *ProviderEntry, existing *existingProvider, ok bool, bindings bindingSet) ([]resolvedSlot, error) {
	resolved := make([]resolvedSlot, 0, len(entry.Slots))
	for j := range entry.Slots {
		slot := &entry.Slots[j]
		slotID := access.NewID()
		var credentialID *string
		allowedAPIKeys := slot.Restrictions.AllowedAPIKeys
		if ok {
			if current, present := existing.Slots[slot.Name]; present {
				slotID = current.ID
				// A slot the artifact declares without a credential holds none.
				if slot.CredentialRef != nil {
					credentialID = current.CredentialID
				}
				var stored []byte
				if err := tx.QueryRow(ctx, "SELECT coalesce(restrictions->'allowed_api_keys','[]'::jsonb) FROM olp.provider_slots WHERE id=$1", current.ID).Scan(&stored); err != nil {
					return nil, err
				}
				var keys []string
				if err := json.Unmarshal(stored, &keys); err != nil {
					return nil, err
				}
				allowedAPIKeys = keys
			}
		}
		if slot.CredentialRef != nil {
			if secret, supplied := bindings[*slot.CredentialRef]; supplied {
				same, err := s.bindingMatches(ctx, tx, credentialID, secret)
				if err != nil {
					return nil, err
				}
				if !same {
					stored, err := s.storeBinding(ctx, tx, providerID, secret)
					if err != nil {
						return nil, err
					}
					credentialID = &stored
				}
			}
		}
		resolved = append(resolved, resolvedSlot{entry: slot, id: slotID, credentialID: credentialID, allowedAPIKeys: allowedAPIKeys})
	}
	return resolved, nil
}

func (s *Server) bindChangedCredentials(ctx context.Context, tx pgx.Tx, providerID string, entry *ProviderEntry, existing *existingProvider, bindings bindingSet) error {
	changed := false
	for j := range entry.Slots {
		slot := &entry.Slots[j]
		if slot.CredentialRef == nil {
			continue
		}
		secret, supplied := bindings[*slot.CredentialRef]
		if !supplied {
			continue
		}
		current, present := existing.Slots[slot.Name]
		if !present {
			continue
		}
		same, err := s.bindingMatches(ctx, tx, current.CredentialID, secret)
		if err != nil {
			return err
		}
		if same {
			continue
		}
		stored, err := s.storeBinding(ctx, tx, providerID, secret)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE olp.provider_slots SET credential_id=$3 WHERE provider_id=$1 AND id=$2", providerID, current.ID, stored); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		if _, err := tx.Exec(ctx, "UPDATE olp.providers SET slots_etag=$2,draft_dirty=true,updated_at=now() WHERE id=$1", providerID, access.NewID()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) replaceDraftContents(ctx context.Context, tx pgx.Tx, providerID string, entry *ProviderEntry, resolved []resolvedSlot) error {
	desired := make([]string, 0, len(entry.Models))
	for _, m := range entry.Models {
		desired = append(desired, m.UpstreamModel)
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.provider_models SET enabled=false,capabilities='[]'::jsonb WHERE provider_id=$1 AND NOT (upstream_model = ANY($2::text[]))", providerID, desired); err != nil {
		return err
	}
	for _, m := range entry.Models {
		display := m.DisplayName
		if display == "" {
			display = m.UpstreamModel
		}
		type capability struct {
			Operation   string  `json:"operation"`
			Surface     string  `json:"surface"`
			Mode        string  `json:"mode"`
			Source      string  `json:"source"`
			CertifiedAt *string `json:"certified_at"`
		}
		declared := make([]capability, 0, len(m.Capabilities))
		for _, c := range m.Capabilities {
			declared = append(declared, capability{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode, Source: "declared"})
		}
		capabilities, _ := json.Marshal(declared)
		if _, err := tx.Exec(ctx, `INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities) VALUES($1,$2,$3,$4,$5,$6)
            ON CONFLICT(provider_id,upstream_model) DO UPDATE SET display_name=excluded.display_name,enabled=excluded.enabled,capabilities=excluded.capabilities`, access.NewID(), providerID, m.UpstreamModel, display, m.Enabled, capabilities); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM olp.provider_slots WHERE provider_id=$1", providerID); err != nil {
		return err
	}
	for _, slot := range resolved {
		restrictions, _ := json.Marshal(struct {
			AllowedAPIKeys []string `json:"allowed_api_keys"`
			AllowedModels  []string `json:"allowed_models"`
			AllowedRoutes  []string `json:"allowed_routes"`
		}{orEmpty(slot.allowedAPIKeys), orEmpty(slot.entry.Restrictions.AllowedModels), orEmpty(slot.entry.Restrictions.AllowedRoutes)})
		limits, _ := json.Marshal(slot.entry.Limits)
		if _, err := tx.Exec(ctx, "INSERT INTO olp.provider_slots(id,provider_id,is_default,position,name,enabled,priority,weight,credential_id,restrictions,limits) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)", slot.id, providerID, slot.entry.IsDefault, slot.entry.Position, slot.entry.Name, slot.entry.Enabled, slot.entry.Priority, slot.entry.Weight, slot.credentialID, restrictions, limits); err != nil {
			return err
		}
	}
	return nil
}

// applyTemplates creates or replaces the document's route templates. Applying
// a template never generates routes itself; provider activation and the
// template's apply operation do.
func applyTemplates(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document, projectIDs map[string]string) error {
	for i := range doc.Templates {
		entry := &doc.Templates[i]
		var projectID *string
		if entry.Project != nil {
			id := projectIDs[strings.ToLower(*entry.Project)]
			projectID = &id
		}
		var id string
		var existingProject *string
		err := tx.QueryRow(ctx, "SELECT id::text,project_id::text FROM olp.route_templates WHERE name=$1 FOR UPDATE", entry.Name).Scan(&id, &existingProject)
		replace := err == nil
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			id = access.NewID()
		case err != nil:
			return err
		case !sameProjectID(existingProject, projectID):
			return access.Fail(409, "route_template_project_mismatch", "Route template "+entry.Name+" belongs to a different project.")
		}
		input := entry.input(projectID)
		if err = routes.ValidateTemplateInput(&input); err != nil {
			return withFieldPrefix(err, "templates."+strconv.Itoa(i))
		}
		if err = routes.WriteTemplate(ctx, tx, id, access.NewID(), p.UserID(), &input, replace); err != nil {
			return err
		}
	}
	return nil
}

func sameProjectID(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// Digest-specific controls belong to the destination installation. Promotion
// changes portable defaults while preserving its local overrides and blocks.
func projectEndUserPolicy(existing *access.EndUserPolicy, defaults *access.AdmissionLimits, template *string) *access.EndUserPolicy {
	if defaults == nil && template == nil && (existing == nil || len(existing.Overrides)+len(existing.Blocked) == 0) {
		return nil
	}
	policy := &access.EndUserPolicy{}
	if existing != nil {
		*policy = *existing
	}
	policy.LimitTemplate = template
	policy.Defaults = access.AdmissionLimits{}
	if defaults != nil {
		policy.Defaults = *defaults
	}
	return policy
}
