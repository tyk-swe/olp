package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/tyk-swe/olp/internal/modelcatalog"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/agentpublications"
	"github.com/tyk-swe/olp/internal/promotion/charge"
	"github.com/tyk-swe/olp/internal/promotion/code"
	"github.com/tyk-swe/olp/internal/promotion/guardrail"
	"github.com/tyk-swe/olp/internal/promotion/mcpentries"
	"github.com/tyk-swe/olp/internal/promotion/organizations"
	"github.com/tyk-swe/olp/internal/promotion/providerpromotion"
	"github.com/tyk-swe/olp/internal/promotion/retrievalbindings"
	"github.com/tyk-swe/olp/internal/promotion/scimmappings"
	"github.com/tyk-swe/olp/internal/promotion/workloadissuers"
	"github.com/tyk-swe/olp/internal/sinks"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	maxProjects  = 100
	maxProviders = 500
	maxModels    = 10000
	maxSlots     = 64
	maxTargets   = 64
	maxRoutes    = 10000
	maxPrices    = 10000
)

func routeKey(slug string, project *string) string {
	return slug + "\x00" + promotion.Lower(project)
}

func loadState(ctx context.Context, q access.Queryer) (*promotion.StateView, error) {
	v := &promotion.StateView{ProjectOrganization: map[string]*string{}, ProjectAttributionBudgets: map[string]access.AttributionBudgets{}, ProjectTemplates: map[string]access.LimitTemplates{}, ProjectBudgets: map[string]*access.BudgetPolicy{}, ProjectRouteGroups: map[string]access.RouteGroups{}, Projects: map[string]string{}, ProjectName: map[string]string{}, ProjectEndUserPolicies: map[string]*access.EndUserPolicy{}, ProjectAttributionPolicies: map[string]*attribution.Policy{}, Providers: map[string]*promotion.ExistingProvider{}, Routes: map[string]*promotion.ExistingRoute{}, Drafts: map[string]*promotion.ExistingDraft{}}
	rows, err := q.Query(ctx, "SELECT id::text,name,end_user_policy,attribution_policy,route_groups,budget_policy,limit_templates,attribution_budgets,(SELECT name FROM olp.organizations WHERE id=organization_id) FROM olp.projects")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var organization *string
		var policy *access.EndUserPolicy
		var labels *attribution.Policy
		var groups access.RouteGroups
		var budget *access.BudgetPolicy
		var templates access.LimitTemplates
		var caps access.AttributionBudgets
		if err = rows.Scan(&id, &name, &policy, &labels, &groups, &budget, &templates, &caps, &organization); err != nil {
			return nil, err
		}
		v.Projects[strings.ToLower(name)] = id
		v.ProjectName[id] = name
		v.ProjectOrganization[id] = organization
		v.ProjectEndUserPolicies[id] = policy
		v.ProjectAttributionPolicies[id] = labels
		v.ProjectRouteGroups[id] = groups
		v.ProjectBudgets[id] = budget
		v.ProjectTemplates[id] = templates
		v.ProjectAttributionBudgets[id] = caps
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	providers, err := q.Query(ctx, `SELECT p.id::text,p.name,p.kind,p.state,p.project_id::text,
		(SELECT c.id::text FROM olp.provider_network_credentials c WHERE c.id::text=p.configuration#>>'{options,network,credential_id}' AND c.provider_id=p.id AND c.revoked_at IS NULL),
		(SELECT bi.credential_id::text FROM olp.provider_billing_imports bi WHERE bi.provider_id=p.id) IS NOT NULL
		FROM olp.providers p`)
	if err != nil {
		return nil, err
	}
	defer providers.Close()
	var withImport []*promotion.ExistingProvider
	for providers.Next() {
		var p promotion.ExistingProvider
		var hasImport bool
		if err = providers.Scan(&p.ID, &p.Name, &p.Kind, &p.State, &p.ProjectID, &p.NetworkCredentialID, &hasImport); err != nil {
			return nil, err
		}
		p.Slots = map[string]promotion.ExistingSlot{}
		v.Providers[strings.ToLower(p.Name)] = &p
		if hasImport {
			withImport = append(withImport, v.Providers[strings.ToLower(p.Name)])
		}
	}
	if err = providers.Err(); err != nil {
		return nil, err
	}
	// ReadImportState runs another query on the same connection, so the
	// providers cursor has to be closed first.
	providers.Close()
	for _, p := range withImport {
		if p.BillingImport, err = billing.ReadImportState(ctx, q, p.ID); err != nil {
			return nil, err
		}
	}
	// A lapsed grant serves no more: the slot's credential reads as absent, so
	// a grant provider plans its enrollment again.
	slots, err := q.Query(ctx, `SELECT s.provider_id::text,s.name,s.id::text,c.id::text,coalesce(c.plugin_digest,''),coalesce(c.profile_id,'')
        FROM olp.provider_slots s LEFT JOIN olp.provider_credentials c ON c.id=s.credential_id AND c.revoked_at IS NULL
        AND NOT EXISTS (SELECT 1 FROM olp.provider_grants g WHERE g.credential_id=c.id AND g.lapsed_at IS NOT NULL)`)
	if err != nil {
		return nil, err
	}
	defer slots.Close()
	byProvider := map[string]*promotion.ExistingProvider{}
	for _, p := range v.Providers {
		byProvider[p.ID] = p
	}
	for slots.Next() {
		var providerID, name, id string
		var credentialID *string
		var digest, profile string
		if err = slots.Scan(&providerID, &name, &id, &credentialID, &digest, &profile); err != nil {
			return nil, err
		}
		if p, ok := byProvider[providerID]; ok {
			p.Slots[strings.TrimSpace(name)] = promotion.ExistingSlot{ID: id, CredentialID: credentialID, PluginDigest: digest, ProfileID: profile}
		}
	}
	if err = slots.Err(); err != nil {
		return nil, err
	}
	routes, err := q.Query(ctx, "SELECT id::text,slug,project_id::text,state FROM olp.routes")
	if err != nil {
		return nil, err
	}
	defer routes.Close()
	for routes.Next() {
		var id, slug, state string
		var projectID *string
		if err = routes.Scan(&id, &slug, &projectID, &state); err != nil {
			return nil, err
		}
		v.Routes[slug] = &promotion.ExistingRoute{ID: id, ProjectID: projectID, State: state}
	}
	if err = routes.Err(); err != nil {
		return nil, err
	}
	drafts, err := q.Query(ctx, "SELECT id::text,slug,project_id::text FROM olp.route_drafts WHERE state IN ('draft','validated') AND based_on_revision_id IS NULL ORDER BY created_at DESC,id DESC")
	if err != nil {
		return nil, err
	}
	defer drafts.Close()
	for drafts.Next() {
		var id, slug string
		var projectID *string
		if err = drafts.Scan(&id, &slug, &projectID); err != nil {
			return nil, err
		}
		key := routeKey(slug, v.ProjectOf(projectID))
		if _, ok := v.Drafts[key]; !ok {
			v.Drafts[key] = &promotion.ExistingDraft{ID: id, ProjectID: projectID}
		}
	}
	return v, drafts.Err()
}

func plan(s *promotion.Server, ctx context.Context, q access.Queryer, doc *promotion.Document, bindings promotion.BindingSet, expected *string) (*promotion.PlanResult, error) {
	unavailable, err := validateDocument(s, ctx, q, doc)
	if err != nil {
		return nil, err
	}
	if err := promotion.ValidateBindings(doc, bindings); err != nil {
		return nil, err
	}
	doc.Canonicalize()
	digest, err := promotion.CanonicalizeAndDigest(doc)
	if err != nil {
		return nil, err
	}
	result := &promotion.PlanResult{Digest: digest, Actions: []promotion.PlanItem{}, Conflicts: []promotion.PlanItem{}, Blockers: []promotion.PlanItem{}}
	if err := code.PlanCode(ctx, q, doc, result); err != nil {
		return nil, err
	}
	if err = modelcatalog.PlanCatalogPolicies(ctx, q, doc, result); err != nil {
		return nil, err
	}
	for plugin, refusal := range unavailable {
		result.Blocker("plugin", plugin, refusal)
	}
	state, err := loadState(ctx, q)
	if err != nil {
		return nil, err
	}
	if err = agentpublications.PlanAgentPublications(s, ctx, q, doc, bindings, state, result); err != nil {
		return nil, err
	}
	if err = mcpentries.PlanMCPServers(s, ctx, q, doc, bindings, state, result); err != nil {
		return nil, err
	}
	if err = retrievalbindings.PlanRetrievalBindings(ctx, q, doc, result); err != nil {
		return nil, err
	}
	if err = sinks.PlanSinks(s, ctx, q, doc, bindings, result); err != nil {
		return nil, err
	}
	if err = guardrail.PlanGuardrailPolicies(ctx, q, doc, state, result); err != nil {
		return nil, err
	}
	if err = guardrail.PlanGuardrails(ctx, q, doc, state, result); err != nil {
		return nil, err
	}
	if doc.SAML != nil {
		current, err := access.LoadSAMLDefinition(ctx, q)
		if err != nil {
			return nil, err
		}
		action := "replace"
		if current == nil {
			action = "create"
		} else if reflect.DeepEqual(current, doc.SAML) {
			action = "reuse"
		}
		result.Add("configuration", "saml", action, "Identity trust is portable; signing material and linked identities stay local.")
	}
	if err := scimmappings.PlanSCIMMappings(ctx, q, doc, state, result); err != nil {
		return nil, err
	}
	if err := workloadissuers.PlanWorkloadIssuers(ctx, q, doc, state, result); err != nil {
		return nil, err
	}
	if expected != nil {
		current, err := exportDocument(s, ctx, q)
		if err != nil {
			return nil, err
		}
		currentDigest, err := promotion.CanonicalizeAndDigest(current)
		if err != nil {
			return nil, err
		}
		if *expected != currentDigest {
			result.Conflict("configuration", "expected_digest", "configuration_changed")
		}
	}
	if doc.BudgetTimeZone != nil {
		var current string
		if err := q.QueryRow(ctx, "SELECT COALESCE((SELECT value FROM olp.settings WHERE key='budgets.time_zone'),'UTC')").Scan(&current); err != nil {
			return nil, err
		}
		action := "reuse"
		if current != *doc.BudgetTimeZone {
			action = "replace"
		}
		result.Add("configuration", "budget_time_zone", action, "")
	}

	if doc.InstallationBudget != nil {
		var current *access.BudgetPolicy
		if err := q.QueryRow(ctx, "SELECT budget_policy FROM olp.installation WHERE singleton").Scan(&current); err != nil {
			return nil, err
		}
		action := "reuse"
		if !reflect.DeepEqual(current, access.NormalizedBudget(doc.InstallationBudget)) {
			action = "replace"
		}
		result.Add("configuration", "installation_budget", action, "")
	}
	projectNames := map[string]bool{}
	for name := range state.Projects {
		projectNames[name] = true
	}
	if err = organizations.PlanOrganizations(ctx, q, doc, result); err != nil {
		return nil, err
	}
	for _, p := range doc.Projects {
		key := strings.ToLower(p.Name)
		projectNames[key] = true
	}
	for _, p := range doc.Projects {
		if id, ok := state.Projects[strings.ToLower(p.Name)]; ok {
			action := "reuse"
			if p.Organization != nil {
				current := state.ProjectOrganization[id]
				if current != nil && !strings.EqualFold(*current, *p.Organization) {
					return nil, access.Fail(409, "project_organization_immutable", "A project cannot move between organizations.")
				}
				if current == nil {
					action = "replace"
				}
			}
			desired := projectEndUserPolicy(state.ProjectEndUserPolicies[id], p.EndUserDefaults, p.EndUserLimitTemplate)
			if !p.AttributionBudgets.Equal(state.ProjectAttributionBudgets[id]) || (p.LimitTemplates != nil && !reflect.DeepEqual(*p.LimitTemplates, state.ProjectTemplates[id])) || !reflect.DeepEqual(desired, state.ProjectEndUserPolicies[id]) || !reflect.DeepEqual(p.AttributionPolicy, state.ProjectAttributionPolicies[id]) || !p.RouteGroups.Equal(state.ProjectRouteGroups[id]) || !reflect.DeepEqual(p.Budget, state.ProjectBudgets[id]) {
				action = "replace"
			}
			result.Add("project", p.Name, action, "")
		} else {
			result.Add("project", p.Name, "create", "")
		}
	}
	docProviders := map[string]*promotion.ProviderEntry{}
	for i := range doc.Providers {
		docProviders[strings.ToLower(doc.Providers[i].Name)] = &doc.Providers[i]
	}
	for i := range doc.Providers {
		p := &doc.Providers[i]
		if p.Project != nil && !projectNames[strings.ToLower(*p.Project)] {
			result.Blocker("provider", p.Name, "project_unknown")
		}
		existing, ok := state.Providers[strings.ToLower(p.Name)]
		switch {
		case !ok:
			result.Add("provider", p.Name, "create", "draft")
		case existing.Kind != p.Configuration.Kind:
			result.Conflict("provider", p.Name, "provider_kind_changed")
		case promotion.Lower(state.ProjectOf(existing.ProjectID)) != promotion.Lower(p.Project):
			result.Conflict("provider", p.Name, "provider_project_mismatch")
		default:
			current, err := currentProviderEntry(s, ctx, q, existing, state.ProjectOf(existing.ProjectID))
			if err != nil {
				return nil, err
			}
			if canonicalEqualProvider(p, current) {
				result.Add("provider", p.Name, "noop", "")
			} else {
				result.Add("provider", p.Name, "replace", "draft")
			}
		}
		if p.NetworkCredentialRef != nil {
			ref := *p.NetworkCredentialRef
			if _, supplied := bindings[ref]; supplied {
				result.Add("network_credential", ref, "bind", "")
			} else if ok && existing.NetworkCredentialID != nil {
				result.Add("network_credential", ref, "reuse", "")
			} else {
				result.Blocker("network_credential", ref, "secret_binding_required")
			}
		}
		switch {
		case p.BillingImport != nil:
			ref := p.BillingImport.CredentialRef
			if _, supplied := bindings[ref]; supplied {
				result.Add("billing_import", ref, "bind", "")
			} else if ok && existing.BillingImport != nil && existing.BillingImport.CredentialID != nil {
				source, err := billing.SourceFor(p.Configuration.Kind)
				if err != nil {
					return nil, err
				}
				compatible, err := billing.ImportCredentialCompatible(ctx, q, s.Access.Keys, s.Access.Installation, source, existing.BillingImport.CredentialID)
				if err != nil {
					return nil, err
				}
				if compatible {
					result.Add("billing_import", ref, "reuse", "")
				} else {
					result.Blocker("billing_import", ref, "secret_binding_required")
				}
			} else {
				result.Blocker("billing_import", ref, "secret_binding_required")
			}
		case ok && existing.BillingImport != nil:
			result.Add("billing_import", promotion.BillingRef(p.Name), "delete", "")
		}
		for j := range p.Slots {
			slot := &p.Slots[j]
			if slot.CredentialRef == nil {
				continue
			}
			ref := *slot.CredentialRef
			var current promotion.ExistingSlot
			if ok {
				current = existing.Slots[slot.Name]
			}
			// The slot's credential version serves on only if the provider
			// authenticates with it: a static credential, or a grant the
			// plugin build it pins enrolled.
			fits := current.CredentialID != nil && p.Configuration.Authenticates(current.PluginDigest, current.ProfileID)
			secret, supplied := bindings[ref]
			switch {
			case supplied:
				if same, err := s.BindingMatches(ctx, q, current.CredentialID, secret); err == nil && same {
					result.Add("credential", ref, "reuse", "")
				} else {
					result.Add("credential", ref, "bind", "")
				}
			case fits:
				result.Add("credential", ref, "reuse", "")
			case p.Configuration.Grant():
				result.Add("credential", ref, "enroll", "grant_enrollment_required")
			default:
				result.Blocker("credential", ref, "secret_binding_required")
			}
		}
	}
	for i := range doc.Routes {
		desired := doc.Routes[i]
		rt := &desired
		if rt.Project != nil && !projectNames[strings.ToLower(*rt.Project)] {
			result.Blocker("route", rt.Slug, "project_unknown")
		}
		if existing, ok := state.Routes[rt.Slug]; ok {
			if promotion.Lower(state.ProjectOf(existing.ProjectID)) != promotion.Lower(rt.Project) {
				result.Conflict("route", rt.Slug, "route_project_mismatch")
			} else if rt.Retired != (existing.State == "retired") {
				code := "route_lifecycle_requires_activation"
				if rt.Retired {
					code = "route_lifecycle_requires_retirement"
				}
				result.Blocker("route", rt.Slug, code)
			}
		}
		draft, staged := state.Drafts[routeKey(rt.Slug, rt.Project)]
		switch {
		case !staged:
			result.Add("route", rt.Slug, "stage", "route_draft")
		default:
			current, err := currentRouteEntry(s, ctx, q, draft.ID, rt, state)
			if err != nil {
				return nil, err
			}
			if canonicalEqualRoute(rt, current) {
				result.Add("route", rt.Slug, "noop", "")
			} else {
				result.Add("route", rt.Slug, "stage", "route_draft")
			}
		}
	}
	current, err := exportTemplates(ctx, q)
	if err != nil {
		return nil, err
	}
	for i := range doc.Templates {
		desired := &doc.Templates[i]
		if desired.Project != nil && !projectNames[strings.ToLower(*desired.Project)] {
			result.Blocker("route_template", desired.Name, "project_unknown")
		}
		existing := templateNamed(current, desired.Name)
		switch {
		case existing == nil:
			result.Add("route_template", desired.Name, "create", "")
		case promotion.Lower(existing.Project) != promotion.Lower(desired.Project):
			result.Conflict("route_template", desired.Name, "route_template_project_mismatch")
		case sameTemplate(desired, existing):
			result.Add("route_template", desired.Name, "noop", "")
		default:
			result.Add("route_template", desired.Name, "replace", "")
		}
	}
	if doc.Pricing != nil {
		changed, err := pricingChanged(s, ctx, q, doc)
		if err != nil {
			return nil, err
		}
		switch {
		case !changed:
			result.Add("pricing", "pricing", "noop", "")
		default:
			effective, _ := time.Parse(time.RFC3339, doc.Pricing.EffectiveAt)
			detail := "revision"
			if !effective.After(time.Now()) {
				detail = "effective_at_rebased"
			}
			result.Add("pricing", "pricing", "create", detail)
		}
	}
	if err := charge.PlanDiscounts(s, ctx, q, doc, result); err != nil {
		return nil, err
	}
	if err := charge.PlanRateCards(s, ctx, q, doc, result); err != nil {
		return nil, err
	}
	result.Sort()
	if doc.RequireLocalMFA != nil {
		var current bool
		if err := q.QueryRow(ctx, "SELECT COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.mfa_required'),false)").Scan(&current); err != nil {
			return nil, err
		}
		action := "reuse"
		if current != *doc.RequireLocalMFA {
			action = "replace"
		}
		result.Add("configuration", "require_local_mfa", action, "Change the local sign-in policy; factors, challenges and recovery codes remain installation-local.")
	}
	return result, nil
}

func pricingChanged(s *promotion.Server, ctx context.Context, q access.Queryer, doc *promotion.Document) (bool, error) {
	revisions, _, err := usage.ListRevisions(ctx, q, nil, 1)
	if err != nil {
		return false, err
	}
	if len(revisions) == 0 {
		return true, nil
	}
	latest := revisions[0]
	providerNames, err := promotion.ProviderNameMap(ctx, q)
	if err != nil {
		return false, err
	}
	current := promotion.PricingEntry{EffectiveAt: latest.EffectiveAt.Format(time.RFC3339)}
	for _, price := range latest.Prices {
		entry := promotion.PriceEntryFromUsage(price)
		if price.ProviderID != nil {
			name := providerNames[*price.ProviderID]
			entry.Provider = &name
		}
		current.Prices = append(current.Prices, entry)
	}
	desired := *doc.Pricing
	if len(desired.Prices) != len(current.Prices) {
		return true, nil
	}
	normalizePrices := func(prices []promotion.PriceEntry) []promotion.PriceEntry {
		copied := slices.Clone(prices)
		for i := range copied {
			e := &copied[i]
			e.Model = strings.TrimSpace(e.Model)
			e.Currency = strings.ToUpper(strings.TrimSpace(e.Currency))
			if e.VendorID != nil {
				v := strings.TrimSpace(*e.VendorID)
				e.VendorID = &v
			}
			if e.Provider != nil {
				v := strings.ToLower(strings.TrimSpace(*e.Provider))
				e.Provider = &v
			}
		}
		slices.SortFunc(copied, func(a, b promotion.PriceEntry) int {
			key := func(p *promotion.PriceEntry) string {
				return p.ProviderKind + "\x00" + promotion.Lower(p.Provider) + "\x00" + promotion.Lower(p.VendorID) + "\x00" + p.Model + "\x00" + p.Operation
			}
			return strings.Compare(key(&a), key(&b))
		})
		return copied
	}
	want := normalizePrices(desired.Prices)
	have := normalizePrices(current.Prices)
	for i := range want {
		if want[i].ProviderKind != have[i].ProviderKind ||
			promotion.Lower(want[i].Provider) != promotion.Lower(have[i].Provider) ||
			promotion.Lower(want[i].VendorID) != promotion.Lower(have[i].VendorID) ||
			want[i].Model != have[i].Model ||
			want[i].Operation != have[i].Operation ||
			!usage.SamePrice(want[i].UsagePrice(), have[i].UsagePrice()) {
			return true, nil
		}
	}
	return false, nil
}

func canonicalEqualProvider(desired, current *promotion.ProviderEntry) bool {
	a := *desired
	promotion.CanonicalProvider(&a)
	b := *current
	named := make(map[string]bool, len(a.Models))
	for _, m := range a.Models {
		named[m.UpstreamModel] = true
	}
	// Apply keeps a model the document omits as a disabled row with no
	// capabilities, so that row is absent, not a difference. Filter into a new
	// slice: b shares its backing array with current.
	kept := make([]promotion.ModelEntry, 0, len(b.Models))
	for _, m := range b.Models {
		if !named[m.UpstreamModel] && !m.Enabled && len(m.Capabilities) == 0 {
			continue
		}
		kept = append(kept, m)
	}
	b.Models = kept
	promotion.CanonicalProvider(&b)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func canonicalEqualRoute(desired, current *promotion.RouteEntry) bool {
	a := *desired
	a.Retired = false
	promotion.CanonicalRoute(&a)
	b := *current
	b.Retired = false
	promotion.CanonicalRoute(&b)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func currentProviderEntry(s *promotion.Server, ctx context.Context, q access.Queryer, p *promotion.ExistingProvider, project *string) (*promotion.ProviderEntry, error) {
	var configuration []byte
	if err := q.QueryRow(ctx, "SELECT configuration FROM olp.providers WHERE id=$1", p.ID).Scan(&configuration); err != nil {
		return nil, err
	}
	entry := &promotion.ProviderEntry{Name: p.Name, Project: project}
	if err := json.Unmarshal(configuration, &entry.Configuration); err != nil {
		return nil, err
	}
	models, err := exportModels(ctx, q, p.ID)
	if err != nil {
		return nil, err
	}
	slots, err := exportSlots(ctx, q, p.ID, p.Name)
	if err != nil {
		return nil, err
	}
	entry.Models = models
	entry.Slots = slots
	providerpromotion.PortableNetwork(entry)
	providerpromotion.ReferenceGrants(entry)
	if err := charge.AttachBillingImport(ctx, q, entry, p.ID); err != nil {
		return nil, err
	}
	return entry, nil
}

func currentRouteEntry(s *promotion.Server, ctx context.Context, q access.Queryer, draftID string, desired *promotion.RouteEntry, state *promotion.StateView) (*promotion.RouteEntry, error) {
	entry := &promotion.RouteEntry{Slug: desired.Slug, Project: desired.Project}
	var operations, targets, Behavior []byte
	if err := q.QueryRow(ctx, "SELECT operations,overall_timeout_ms,max_attempts,targets,content_policy,fidelity,behavior,COALESCE(catalog_expose_upstream_models,(SELECT c.expose_upstream_models FROM olp.model_catalog_route_settings c JOIN olp.routes r ON r.id=c.route_id WHERE r.slug=route_drafts.slug),false) FROM olp.route_drafts WHERE id=$1", draftID).Scan(&operations, &entry.OverallTimeoutMS, &entry.MaxAttempts, &targets, &entry.ContentPolicy, &entry.Fidelity, &Behavior, &entry.ExposeUpstreamModels); err != nil {
		return nil, err
	}
	if err := entry.SetBehavior(Behavior); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(operations, &entry.Operations); err != nil {
		return nil, err
	}
	var published []routing.PublishedTarget
	if err := json.Unmarshal(targets, &published); err != nil {
		return nil, err
	}
	for _, t := range published {
		entry.Targets = append(entry.Targets, promotion.TargetEntryFromPublished(t))
	}
	var policy []byte
	err := q.QueryRow(ctx, "SELECT policy FROM olp.routing_policies WHERE scope='route-draft' AND scope_id=$1", draftID).Scan(&policy)
	switch {
	case err == nil:
		var p routing.Policy
		if err = json.Unmarshal(policy, &p); err != nil {
			return nil, err
		}
		entry.RoutingPolicy = &p
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return nil, err
	}
	return entry, nil
}

func bindingFingerprint(doc *promotion.Document, digest string, bindings promotion.BindingSet, expected *string) map[string]any {
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}
	slices.Sort(names)
	sealed := make([]map[string]string, 0, len(names))
	for _, name := range names {
		value := []byte("sealed:" + bindings[name].Secret)
		if bindings[name].Reference != nil {
			value, _ = json.Marshal(bindings[name].Reference)
			value = append([]byte("external:"), value...)
		}
		sum := sha256.Sum256(value)
		clear(value)
		sealed = append(sealed, map[string]string{"ref": name, "sha256": hex.EncodeToString(sum[:])})
	}
	if len(doc.CodeBindings.Credentials)+len(doc.CodeBindings.Users)+len(doc.CodeBindings.APIKeys) == 0 {
		return map[string]any{"document_digest": digest, "expected_digest": expected, "secret_bindings": sealed}
	}
	return map[string]any{"document_digest": digest, "expected_digest": expected, "secret_bindings": sealed, "code_bindings": doc.CodeBindings}
}

func templateNamed(templates []promotion.TemplateEntry, name string) *promotion.TemplateEntry {
	for i := range templates {
		if templates[i].Name == name {
			return &templates[i]
		}
	}
	return nil
}

// sameTemplate compares two normalized templates, ignoring project name case.
func sameTemplate(a, b *promotion.TemplateEntry) bool {
	x, y := *a, *b
	x.Project, y.Project = nil, nil
	if x.Normalize() != nil || y.Normalize() != nil {
		return false
	}
	left, _ := json.Marshal(x)
	right, _ := json.Marshal(y)
	return bytes.Equal(left, right)
}

// withFieldPrefix places a validation problem under the document field that
// holds the value it names.
func withFieldPrefix(err error, prefix string) error {
	var problem *access.Problem
	if errors.As(err, &problem) && problem.Field != "" {
		copied := *problem
		copied.Field = prefix + "." + problem.Field
		return &copied
	}
	return err
}
