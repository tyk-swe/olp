package serve

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/modelcatalog"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/charge"
	"github.com/tyk-swe/olp/internal/promotion/guardrail"
	"github.com/tyk-swe/olp/internal/promotion/organizations"
	"github.com/tyk-swe/olp/internal/promotion/providerpromotion"
	"github.com/tyk-swe/olp/internal/promotion/scimmappings"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/settings"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage"
)

type resolvedSlot struct {
	entry        *promotion.SlotEntry
	id           string
	credentialID *string
	// allowedAPIKeys is the destination slot's own key restriction. API keys
	// are not portable, so apply keeps it rather than widening the slot.
	allowedAPIKeys []string
}

// Check references against the final project and workload state in this apply.
func validateAppliedTemplateReferences(ctx context.Context, q access.Queryer, doc *promotion.Document) error {
	for _, project := range doc.Projects {
		var id string
		var templates keyauthority.LimitTemplates
		if err := q.QueryRow(ctx, "SELECT id::text,limit_templates FROM olp.projects WHERE lower(name)=lower($1)", project.Name).Scan(&id, &templates); err != nil {
			return err
		}
		if err := projects.ValidateTemplateReferences(ctx, q, id, templates); err != nil {
			return err
		}
	}
	return nil
}

func applyDocument(s *promotion.Server, ctx context.Context, tx pgx.Tx, p access.Principal, doc *promotion.Document, bindings promotion.BindingSet) error {
	if err := access.ApplyMFAPolicy(ctx, tx, p, doc.RequireLocalMFA); err != nil {
		return err
	}

	state, err := loadState(ctx, tx)
	if err != nil {
		return err
	}
	// Plan resolves a project reference against both the document and the
	// destination, so apply must resolve undeclared destination projects too.
	projectIDs := maps.Clone(state.Projects)
	if err := organizations.ApplyOrganizations(ctx, tx, p, doc); err != nil {
		return err
	}
	if err := settings.ApplyBudgetTimeZone(ctx, tx, p, doc.BudgetTimeZone); err != nil {
		return err
	}
	installationBudgetChanged, err := settings.ApplyInstallationBudget(ctx, tx, p, doc.InstallationBudget)
	if err != nil {
		return err
	}
	for _, project := range doc.Projects {
		key := strings.ToLower(project.Name)
		id, exists := projectIDs[key]
		if !exists {
			var err error
			id, err = organizations.CreateProject(ctx, tx, p, project.Name, project.Organization)
			if err != nil {
				return err
			}
			projectIDs[key] = id
		}
		if err := organizations.ApplyProjectOrganization(s, ctx, tx, p, id, project.Organization); err != nil {
			return err
		}
	}
	policyChanges := slices.Collect(func(yield func(projects.PolicyImport) bool) {
		for _, project := range doc.Projects {
			if !yield(projects.PolicyImport{ProjectID: projectIDs[strings.ToLower(project.Name)], EndUserDefaults: project.EndUserDefaults, EndUserTemplate: project.EndUserLimitTemplate, Attribution: project.AttributionPolicy, RouteGroups: project.RouteGroups, Budget: project.Budget, Templates: project.LimitTemplates, AttributionBudgets: project.AttributionBudgets}) {
				return
			}
		}
	})
	projectPoliciesChanged, err := projects.ApplyPolicies(ctx, tx, p, policyChanges)
	if err != nil {
		return err
	}

	if err := scimmappings.ApplySCIMMappings(s, ctx, tx, p, doc, projectIDs); err != nil {
		return err
	}
	if installationBudgetChanged || projectPoliciesChanged {
		if _, err := access.AdvanceAuthority(ctx, tx); err != nil {
			return err
		}
	}

	providerIDs := map[string]string{}
	for name, existing := range state.Providers {
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
		existing, ok := state.Providers[strings.ToLower(entry.Name)]
		var providerID string
		switch {
		case !ok:
			providerID = access.NewID()
			if _, err = tx.Exec(ctx, "INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,$2,$3,'draft',$4,$5,$6,$7,$8)", providerID, entry.Name, entry.Configuration.Kind, configuration, access.NewID(), access.NewID(), p.UserID(), projectID); err != nil {
				return err
			}
		default:
			providerID = existing.ID
			current, err := currentProviderEntry(s, ctx, tx, existing, state.ProjectOf(existing.ProjectID))
			if err != nil {
				return err
			}
			if canonicalEqualProvider(entry, current) {
				if err = bindChangedCredentials(s, ctx, tx, providerID, entry, existing, bindings); err != nil {
					return err
				}
				if err := providerpromotion.BindNetwork(s, ctx, tx, providerID, entry, existing, bindings); err != nil {
					return err
				}
				if err := charge.ApplyBillingImport(s, ctx, tx, providerID, entry, bindings, p.UserID()); err != nil {
					return err
				}
				providerIDs[strings.ToLower(entry.Name)] = providerID
				continue
			}
		}
		if err := providerpromotion.BindNetwork(s, ctx, tx, providerID, entry, existing, bindings); err != nil {
			return err
		}
		if err := charge.ApplyBillingImport(s, ctx, tx, providerID, entry, bindings, p.UserID()); err != nil {
			return err
		}
		resolved, err := resolveSlots(s, ctx, tx, providerID, entry, existing, ok, bindings)
		if err != nil {
			return err
		}
		draftSlots := make([]providers.DraftSlot, 0, len(resolved))
		for _, slot := range resolved {
			restrictions := slot.entry.Restrictions
			restrictions.AllowedAPIKeys = slot.allowedAPIKeys
			draftSlots = append(draftSlots, providers.DraftSlot{ID: slot.id, Default: slot.entry.IsDefault, Position: slot.entry.Position, Name: slot.entry.Name,
				Enabled: slot.entry.Enabled, Priority: slot.entry.Priority, Weight: slot.entry.Weight, CredentialID: slot.credentialID,
				Restrictions: restrictions, Limits: slot.entry.Limits})
		}
		if err = providers.StageDraftInventory(ctx, tx, providerID, entry.Models, draftSlots); err != nil {
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
		draft, staged := state.Drafts[routeKey(route.Slug, route.Project)]
		if staged {
			current, err := currentRouteEntry(s, ctx, tx, draft.ID, route, state)
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
		Input := routes.DraftInput{Slug: route.Slug, Operations: route.Operations, OverallTimeoutMS: route.OverallTimeoutMS, MaxAttempts: route.MaxAttempts, ContentPolicy: route.ContentPolicy, Fidelity: route.Fidelity,
			ToolExecution: route.ToolExecution, Toolsets: route.Toolsets, ResponsesMCPItems: route.ResponsesMCPItems, RunLimits: route.RunLimits,
			Cache: route.Cache, PromptCache: route.PromptCache, CallerCostExempt: route.CallerCostExempt, ProviderFetchedDocuments: route.ProviderFetchedDocuments, MaxBodyBytes: route.MaxBodyBytes, Fallbacks: route.Fallbacks, Selectors: route.Selectors, Retry: route.Retry, Affinity: route.Affinity, Budget: route.Budget, Passthrough: route.Passthrough}
		for _, t := range route.Targets {
			providerID := providerIDs[strings.ToLower(t.Provider)]
			Input.Targets = append(Input.Targets, routes.TargetInput{ProviderID: &providerID, ProviderModel: &t.ProviderModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: t.TimeoutMS, Tags: t.Tags, Shadow: t.Shadow})
		}
		targets, err := routes.ValidateAndNormalizeDraftInput(ctx, tx, &Input, projectID, nil)
		if err != nil {
			return err
		}
		operations, _ := json.Marshal(Input.Operations)
		encoded, _ := json.Marshal(targets)
		var draftID string
		if staged {
			draftID = draft.ID
			if _, err = tx.Exec(ctx, "UPDATE olp.route_drafts SET state='draft',operations=$3,overall_timeout_ms=$4,max_attempts=$5,targets=$6,content_policy=$7,etag=$8,fidelity=$9,behavior=$10,catalog_expose_upstream_models=$11,updated_at=now() WHERE id=$1 AND slug=$2", draftID, Input.Slug, operations, Input.OverallTimeoutMS, Input.MaxAttempts, encoded, Input.ContentPolicy, access.NewID(), Input.Fidelity, Input.Behavior, route.ExposeUpstreamModels); err != nil {
				return err
			}
		} else {
			draftID = access.NewID()
			if _, err = tx.Exec(ctx, "INSERT INTO olp.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,content_policy,etag,created_by,project_id,fidelity,behavior,catalog_expose_upstream_models) VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)", draftID, Input.Slug, operations, Input.OverallTimeoutMS, Input.MaxAttempts, encoded, Input.ContentPolicy, access.NewID(), p.UserID(), projectID, Input.Fidelity, Input.Behavior, route.ExposeUpstreamModels); err != nil {
				return err
			}
		}
		if route.RoutingPolicy == nil {
			if _, err = tx.Exec(ctx, "DELETE FROM olp.routing_policies WHERE scope='route-draft' AND scope_id=$1", draftID); err != nil {
				return err
			}
		} else {
			policy, _ := json.Marshal(route.RoutingPolicy)
			if _, err = tx.Exec(ctx, "INSERT INTO olp.routing_policies(scope,scope_id,policy,etag,updated_by) VALUES('route-draft',$1,$2,$3,$4) ON CONFLICT(scope,scope_id) DO UPDATE SET policy=excluded.policy,etag=excluded.etag,updated_by=excluded.updated_by,configured=true,updated_at=now()", draftID, policy, access.NewID(), p.UserID()); err != nil {
				return err
			}
		}
	}
	if err := applyTemplates(ctx, tx, p, doc, projectIDs); err != nil {
		return err
	}
	if doc.Pricing != nil {
		changed, err := pricingChanged(s, ctx, tx, doc)
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
				price := entry.UsagePrice()
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
			if _, err = release.Publish(ctx, tx, p.UserID()); err != nil {
				return err
			}
		}
	}
	if err = charge.ApplyDiscounts(s, ctx, tx, p, doc, providerIDs); err != nil {
		return err
	}
	organizationIDs, err := charge.OrganizationIDs(ctx, tx)
	if err != nil {
		return err
	}
	if err = charge.ApplyRateCards(s, ctx, tx, p, doc, organizationIDs, projectIDs); err != nil {
		return err
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

	if err := sinks.ApplySinks(s, ctx, tx, p, doc, bindings); err != nil {
		return err
	}
	if err := guardrail.BindGuardrailCredentials(s, ctx, tx, p, doc, bindings); err != nil {
		return err
	}
	if err := guardrail.ApplyGuardrails(ctx, tx, p, doc); err != nil {
		return err
	}
	if err := guardrail.ApplyGuardrailPolicies(ctx, tx, p, doc); err != nil {
		return err
	}
	return modelcatalog.ApplyCatalogPolicies(ctx, tx, p, doc)
}

func resolveSlots(s *promotion.Server, ctx context.Context, tx pgx.Tx, providerID string, entry *promotion.ProviderEntry, existing *promotion.ExistingProvider, ok bool, bindings promotion.BindingSet) ([]resolvedSlot, error) {
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
				same, err := s.BindingMatches(ctx, tx, credentialID, secret)
				if err != nil {
					return nil, err
				}
				if !same {
					stored, err := storeBinding(s, ctx, tx, providerID, secret)
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

func bindChangedCredentials(s *promotion.Server, ctx context.Context, tx pgx.Tx, providerID string, entry *promotion.ProviderEntry, existing *promotion.ExistingProvider, bindings promotion.BindingSet) error {
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
		same, err := s.BindingMatches(ctx, tx, current.CredentialID, secret)
		if err != nil {
			return err
		}
		if same {
			continue
		}
		stored, err := storeBinding(s, ctx, tx, providerID, secret)
		if err != nil {
			return err
		}
		if err = providers.BindDraftCredential(ctx, tx, providerID, current.ID, stored); err != nil {
			return err
		}
	}
	return nil
}

// applyTemplates creates or replaces the document's route templates. Applying
// a template never generates routes itself; provider activation and the
// template's apply operation do.
func applyTemplates(ctx context.Context, tx pgx.Tx, p access.Principal, doc *promotion.Document, projectIDs map[string]string) error {
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
		Input := entry.Input(projectID)
		if err = routes.ValidateTemplateInput(&Input); err != nil {
			return withFieldPrefix(err, "templates."+strconv.Itoa(i))
		}
		if err = routes.WriteTemplate(ctx, tx, id, access.NewID(), p.UserID(), &Input, replace); err != nil {
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
