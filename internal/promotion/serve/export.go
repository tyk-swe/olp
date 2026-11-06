package serve

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/agentpublications"
	"github.com/tyk-swe/olp/internal/promotion/charge"
	"github.com/tyk-swe/olp/internal/promotion/code"
	"github.com/tyk-swe/olp/internal/promotion/guardrail"
	"github.com/tyk-swe/olp/internal/promotion/mcpentries"
	"github.com/tyk-swe/olp/internal/promotion/providerpromotion"
	"github.com/tyk-swe/olp/internal/promotion/retrievalbindings"
	"github.com/tyk-swe/olp/internal/promotion/scimmappings"
	"github.com/tyk-swe/olp/internal/promotion/workloadissuers"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage"
)

func exportDocument(s *promotion.Server, ctx context.Context, q access.Queryer) (*promotion.Document, error) {
	doc := &promotion.Document{APIVersion: promotion.APIVersion}
	if err := q.QueryRow(ctx, "SELECT COALESCE((SELECT value FROM olp.settings WHERE key='budgets.time_zone'),'UTC')").Scan(&doc.BudgetTimeZone); err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, "SELECT budget_policy FROM olp.installation WHERE singleton").Scan(&doc.InstallationBudget); err != nil {
		return nil, err
	}
	if doc.InstallationBudget == nil {
		doc.InstallationBudget = &keyauthority.BudgetPolicy{}
	}
	organizations, err := q.Query(ctx, "SELECT name,budget_policy FROM olp.organizations ORDER BY lower(name),name")
	if err != nil {
		return nil, err
	}
	for organizations.Next() {
		var o promotion.OrganizationEntry
		if err = organizations.Scan(&o.Name, &o.Budget); err != nil {
			organizations.Close()
			return nil, err
		}
		doc.Organizations = append(doc.Organizations, o)
	}
	organizations.Close()
	if err = organizations.Err(); err != nil {
		return nil, err
	}
	projects, err := q.Query(ctx, "SELECT (SELECT name FROM olp.organizations WHERE id=organization_id) AS organization_name,name,end_user_policy,attribution_policy,route_groups,budget_policy,limit_templates,attribution_budgets,COALESCE((SELECT enabled FROM olp.model_catalog_project_settings WHERE project_id=projects.id),false),COALESCE((SELECT prices_public FROM olp.model_catalog_project_settings WHERE project_id=projects.id),false) FROM olp.projects ORDER BY lower(name),name")
	if err != nil {
		return nil, err
	}
	defer projects.Close()
	for projects.Next() {
		var organization *string
		var name string
		var policy *keyauthority.EndUserPolicy
		var labels *attribution.Policy
		var groups keyauthority.RouteGroups
		var budget *keyauthority.BudgetPolicy
		var templates keyauthority.LimitTemplates
		var caps keyauthority.AttributionBudgets
		var publicCatalog, publicPrices bool
		if err = projects.Scan(&organization, &name, &policy, &labels, &groups, &budget, &templates, &caps, &publicCatalog, &publicPrices); err != nil {
			return nil, err
		}
		entry := promotion.ProjectEntry{Organization: organization, AttributionBudgets: caps, LimitTemplates: &templates, Name: name, Budget: budget, AttributionPolicy: labels, RouteGroups: groups}
		entry.PublicCatalog, entry.PublicCatalogPrices = publicCatalog, publicPrices
		if policy != nil {
			entry.EndUserDefaults = &policy.Defaults
			entry.EndUserLimitTemplate = policy.LimitTemplate
		}
		doc.Projects = append(doc.Projects, entry)
	}
	if err = projects.Err(); err != nil {
		return nil, err
	}
	providersRows, err := q.Query(ctx, `SELECT p.id::text,p.name,pr.name,p.state,r.configuration,r.models,r.slots
        FROM olp.providers p LEFT JOIN olp.projects pr ON pr.id=p.project_id
        LEFT JOIN olp.provider_revisions r ON r.id=p.active_revision_id AND p.state IN ('active','disabled')
        ORDER BY lower(p.name),p.name`)
	if err != nil {
		return nil, err
	}
	defer providersRows.Close()
	type pendingProvider struct {
		entry *promotion.ProviderEntry
		id    string
	}
	var pending []pendingProvider
	// live maps an active provider's index in doc.Providers to its ID, so its
	// slots take their stored positions once the provider rows are read.
	live := map[int]string{}
	for providersRows.Next() {
		var id, name, state string
		var ProjectName *string
		var configuration, models, slots []byte
		if err = providersRows.Scan(&id, &name, &ProjectName, &state, &configuration, &models, &slots); err != nil {
			return nil, err
		}
		entry := promotion.ProviderEntry{Name: name, Project: ProjectName}
		if configuration != nil {

			portable, err := providerpromotion.PortableProviderRevision(entry, configuration, models, slots)
			if err != nil {
				return nil, err
			}
			entry = portable

			if err = charge.AttachBillingImport(ctx, q, &entry, id); err != nil {
				return nil, err
			}
			live[len(doc.Providers)] = id
			doc.Providers = append(doc.Providers, entry)
			continue
		}
		pending = append(pending, pendingProvider{entry: &entry, id: id})
	}
	if err = providersRows.Err(); err != nil {
		return nil, err
	}
	providersRows.Close()
	for index, id := range live {
		if err = exportSlotPositions(ctx, q, id, doc.Providers[index].Slots); err != nil {
			return nil, err
		}
	}
	for _, pp := range pending {
		var configuration []byte
		if err = q.QueryRow(ctx, "SELECT configuration FROM olp.providers WHERE id=$1", pp.id).Scan(&configuration); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(configuration, &pp.entry.Configuration); err != nil {
			return nil, err
		}
		models, err := exportModels(ctx, q, pp.id)
		if err != nil {
			return nil, err
		}
		pp.entry.Models = models
		slots, err := exportSlots(ctx, q, pp.id, pp.entry.Name)
		if err != nil {
			return nil, err
		}
		pp.entry.Slots = slots
		providerpromotion.PortableNetwork(pp.entry)
		providerpromotion.ReferenceGrants(pp.entry)
		if err = charge.AttachBillingImport(ctx, q, pp.entry, pp.id); err != nil {
			return nil, err
		}
		doc.Providers = append(doc.Providers, *pp.entry)
	}
	routes, err := q.Query(ctx, `SELECT r.slug,pr.name,r.state='retired',v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.routing_policy,v.content_policy,v.fidelity,v.Behavior,COALESCE((SELECT expose_upstream_models FROM olp.model_catalog_route_settings WHERE route_id=r.id),false)
        FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id
        LEFT JOIN olp.projects pr ON pr.id=r.project_id ORDER BY r.slug`)
	if err != nil {
		return nil, err
	}
	defer routes.Close()
	for routes.Next() {
		var route promotion.RouteEntry
		var operations, targets, policy, Behavior []byte
		if err = routes.Scan(&route.Slug, &route.Project, &route.Retired, &operations, &route.OverallTimeoutMS, &route.MaxAttempts, &targets, &policy, &route.ContentPolicy, &route.Fidelity, &Behavior, &route.ExposeUpstreamModels); err != nil {
			return nil, err
		}
		if err = route.SetBehavior(Behavior); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(operations, &route.Operations); err != nil {
			return nil, err
		}
		var published []routing.PublishedTarget
		if err = json.Unmarshal(targets, &published); err != nil {
			return nil, err
		}
		for _, t := range published {
			route.Targets = append(route.Targets, promotion.TargetEntryFromPublished(t))
		}
		if policy != nil {
			var p routing.Policy
			if err = json.Unmarshal(policy, &p); err != nil {
				return nil, err
			}
			route.RoutingPolicy = &p
		}
		doc.Routes = append(doc.Routes, route)
	}
	if err = routes.Err(); err != nil {
		return nil, err
	}
	if doc.Templates, err = exportTemplates(ctx, q); err != nil {
		return nil, err
	}
	revisions, _, err := usage.ListRevisions(ctx, q, nil, 1)
	if err != nil {
		return nil, err
	}
	if len(revisions) == 1 {
		providerNames, err := promotion.ProviderNameMap(ctx, q)
		if err != nil {
			return nil, err
		}
		latest := revisions[0]
		doc.Pricing = &promotion.PricingEntry{EffectiveAt: latest.EffectiveAt.Format(time.RFC3339)}
		for _, price := range latest.Prices {
			entry := promotion.PriceEntryFromUsage(price)
			if price.ProviderID != nil {
				name := providerNames[*price.ProviderID]
				entry.Provider = &name
			}
			doc.Pricing.Prices = append(doc.Pricing.Prices, entry)
		}
	}
	if doc.Discounts, err = exportDiscounts(ctx, q); err != nil {
		return nil, err
	}
	if doc.RateCards, err = exportRateCards(ctx, q); err != nil {
		return nil, err
	}
	if doc.AgentPublications, err = agentpublications.ExportAgentPublications(ctx, q); err != nil {
		return nil, err
	}
	doc.RetrievalBindings, err = retrievalbindings.ExportRetrievalBindings(ctx, q)
	if err != nil {
		return nil, err
	}
	doc.MCPServers, err = mcpentries.ExportMCPServers(ctx, q)
	if err != nil {
		return nil, err
	}
	doc.Sinks, err = sinks.ExportSinks(ctx, q)
	if err != nil {
		return nil, err
	}
	doc.Guardrails, err = guardrail.ExportGuardrails(ctx, q)
	if err != nil {
		return nil, err
	}
	doc.Canonicalize()
	doc.SCIMGroupMappings, err = scimmappings.ExportSCIMMappings(ctx, q)
	if err != nil {
		return nil, err
	}
	doc.SAML, err = access.LoadSAMLDefinition(ctx, q)
	if err != nil {
		return nil, err
	}
	doc.WorkloadIssuers, err = workloadissuers.ExportWorkloadIssuers(ctx, q)
	if err != nil {
		return nil, err
	}
	if err := q.QueryRow(ctx, "SELECT COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.mfa_required'),false)").Scan(&doc.RequireLocalMFA); err != nil {
		return doc, err
	}
	if err := guardrail.ExportGuardrailPlatform(ctx, q, doc); err != nil {
		return nil, err
	}
	if doc.Code, err = code.ExportCode(ctx, q); err != nil {
		return nil, err
	}
	doc.Code = promotion.CanonicalCode(doc.Code)
	return doc, nil
}

func exportModels(ctx context.Context, q access.Queryer, providerID string) ([]promotion.ModelEntry, error) {
	rows, err := q.Query(ctx, "SELECT upstream_model,display_name,enabled,capabilities FROM olp.provider_models WHERE provider_id=$1 ORDER BY upstream_model", providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []promotion.ModelEntry
	for rows.Next() {
		var m promotion.ModelEntry
		var capabilities []byte
		if err = rows.Scan(&m.UpstreamModel, &m.DisplayName, &m.Enabled, &capabilities); err != nil {
			return nil, err
		}
		var stored []struct {
			Operation string `json:"operation"`
			Surface   string `json:"surface"`
			Mode      string `json:"mode"`
		}
		if err = json.Unmarshal(capabilities, &stored); err != nil {
			return nil, err
		}
		for _, c := range stored {
			m.Capabilities = append(m.Capabilities, promotion.CapabilityEntry{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode})
		}
		models = append(models, m)
	}
	return models, rows.Err()
}

func exportSlots(ctx context.Context, q access.Queryer, providerID, providerName string) ([]promotion.SlotEntry, error) {
	rows, err := q.Query(ctx, "SELECT name,is_default,position,enabled,priority,weight,credential_id::text,restrictions,limits FROM olp.provider_slots WHERE provider_id=$1 ORDER BY position", providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slots []promotion.SlotEntry
	for rows.Next() {
		var s promotion.SlotEntry
		var credentialID *string
		var restrictions, limits []byte
		if err = rows.Scan(&s.Name, &s.IsDefault, &s.Position, &s.Enabled, &s.Priority, &s.Weight, &credentialID, &restrictions, &limits); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(restrictions, &s.Restrictions); err != nil {
			return nil, err
		}
		s.Restrictions.AllowedAPIKeys = []string{}
		if err = json.Unmarshal(limits, &s.Limits); err != nil {
			return nil, err
		}
		if credentialID != nil {
			ref := promotion.CredentialRef(providerName, s.Name)
			s.CredentialRef = &ref
		}
		slots = append(slots, s)
	}
	return slots, rows.Err()
}

// exportSlotPositions gives revision slots the positions their draft slots
// store, so an export plans as a noop against its own installation.
func exportSlotPositions(ctx context.Context, q access.Queryer, providerID string, slots []promotion.SlotEntry) error {
	rows, err := q.Query(ctx, "SELECT name,position FROM olp.provider_slots WHERE provider_id=$1", providerID)
	if err != nil {
		return err
	}
	defer rows.Close()
	positions := map[string]int{}
	for rows.Next() {
		var name string
		var position int
		if err = rows.Scan(&name, &position); err != nil {
			return err
		}
		positions[name] = position
	}
	if err = rows.Err(); err != nil {
		return err
	}
	applySlotPositions(slots, positions)
	return nil
}

// applySlotPositions sets each slot to its stored draft position when the
// draft holds every slot at a distinct position. Otherwise, for example when
// the draft has deleted a revision slot, the slots keep their revision order,
// because mixing the two numberings could repeat a position.
func applySlotPositions(slots []promotion.SlotEntry, positions map[string]int) {
	seen := make(map[int]bool, len(slots))
	for _, slot := range slots {
		position, ok := positions[slot.Name]
		if !ok || seen[position] {
			for i := range slots {
				slots[i].Position = i
			}
			return
		}
		seen[position] = true
	}
	for i := range slots {
		slots[i].Position = positions[slots[i].Name]
	}
}

// exportDiscounts reads the multiplier in force for each discount scope —
// scopes ended by a multiplier-one discount included, so ending a discount
// promotes as multiplier "1".
func exportDiscounts(ctx context.Context, q access.Queryer) ([]promotion.DiscountEntry, error) {
	names, err := promotion.ProviderNameMap(ctx, q)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (provider_kind,COALESCE(vendor_id,''),COALESCE(provider_id::text,''))
 provider_kind,vendor_id,provider_id::text,multiplier::text
 FROM olp.pricing_discounts WHERE effective_at<=now()
 ORDER BY provider_kind,COALESCE(vendor_id,''),COALESCE(provider_id::text,''),
 effective_at DESC,created_at DESC,id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []promotion.DiscountEntry{}
	for rows.Next() {
		var entry promotion.DiscountEntry
		var providerID *string
		if err = rows.Scan(&entry.ProviderKind, &entry.VendorID, &providerID, &entry.Multiplier); err != nil {
			return nil, err
		}
		if providerID != nil {
			name := names[*providerID]
			entry.Provider = &name
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// exportRateCards reads each card's in-force revision, at most one per owner.
func exportRateCards(ctx context.Context, q access.Queryer) ([]promotion.RateCardEntry, error) {
	inForce, _, err := charge.InForceCards(ctx, q)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(inForce))
	for key, entry := range inForce {
		if entry.Markup != "" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	entries := make([]promotion.RateCardEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, inForce[key])
	}
	return entries, nil
}

// exportTemplates reads every route template, keyed by name.
func exportTemplates(ctx context.Context, q access.Queryer) ([]promotion.TemplateEntry, error) {
	rows, err := q.Query(ctx, `SELECT t.name,pr.name,t.provider_selector,t.model_filter,t.slug_pattern,t.overall_timeout_ms,t.max_attempts,t.fidelity,t.routing_policy,t.auto_publish
		FROM olp.route_templates t LEFT JOIN olp.projects pr ON pr.id=t.project_id ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []promotion.TemplateEntry
	for rows.Next() {
		var t promotion.TemplateEntry
		var policy []byte
		if err = rows.Scan(&t.Name, &t.Project, &t.ProviderSelector, &t.ModelFilter, &t.SlugPattern, &t.OverallTimeoutMS, &t.MaxAttempts, &t.Fidelity, &policy, &t.AutoPublish); err != nil {
			return nil, err
		}
		if len(policy) > 0 {
			t.RoutingPolicy = &routing.Policy{}
			if err = json.Unmarshal(policy, t.RoutingPolicy); err != nil {
				return nil, err
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
