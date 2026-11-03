package configuration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	maxProjects      = 100
	maxProviders     = 500
	maxModels        = 10000
	maxSlots         = 64
	maxTargets       = 64
	maxRoutes        = 10000
	maxPrices        = 10000
	maxCredentialRef = 200
	maxSecretBytes   = 64 << 10
)

type planItem struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

type planResult struct {
	Digest    string     `json:"digest"`
	Actions   []planItem `json:"actions"`
	Conflicts []planItem `json:"conflicts"`
	Blockers  []planItem `json:"blockers"`
}

func (r *planResult) item(kind, key, action, detail string) {
	r.Actions = append(r.Actions, planItem{kind, key, action, detail})
}
func (r *planResult) conflict(kind, key, detail string) {
	r.Conflicts = append(r.Conflicts, planItem{kind, key, "conflict", detail})
}
func (r *planResult) blocker(kind, key, detail string) {
	r.Blockers = append(r.Blockers, planItem{kind, key, "blocker", detail})
}
func (r *planResult) sort() {
	by := func(a, b planItem) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		if c := strings.Compare(a.Action, b.Action); c != 0 {
			return c
		}
		return strings.Compare(a.Detail, b.Detail)
	}
	slices.SortFunc(r.Actions, by)
	slices.SortFunc(r.Conflicts, by)
	slices.SortFunc(r.Blockers, by)
}

type existingSlot struct {
	ID           string
	CredentialID *string
	// PluginDigest is the plugin build whose grant enrollment created the
	// credential version, or "" for a pasted one.
	PluginDigest string
	ProfileID    string
}

type existingProvider struct {
	NetworkCredentialID *string
	BillingImport       *billing.ImportState
	ID                  string
	Name                string
	Kind                string
	State               string
	ProjectID           *string
	Slots               map[string]existingSlot
}

type existingRoute struct {
	ID        string
	ProjectID *string
	State     string
}

type existingDraft struct {
	ID        string
	ProjectID *string
}

type stateView struct {
	projectAttributionBudgets  map[string]access.AttributionBudgets
	projectBudgets             map[string]*access.BudgetPolicy
	projectTemplates           map[string]access.LimitTemplates
	projectRouteGroups         map[string]access.RouteGroups
	projects                   map[string]string
	projectEndUserPolicies     map[string]*access.EndUserPolicy
	projectAttributionPolicies map[string]*attribution.Policy
	projectName                map[string]string
	projectOrganization        map[string]*string
	providers                  map[string]*existingProvider
	routes                     map[string]*existingRoute
	drafts                     map[string]*existingDraft
}

func (v *stateView) projectOf(id *string) *string {
	if id == nil {
		return nil
	}
	if name, ok := v.projectName[*id]; ok {
		return &name
	}
	return nil
}

func routeKey(slug string, project *string) string {
	return slug + "\x00" + lower(project)
}

func loadState(ctx context.Context, q access.Queryer) (*stateView, error) {
	v := &stateView{projectOrganization: map[string]*string{}, projectAttributionBudgets: map[string]access.AttributionBudgets{}, projectTemplates: map[string]access.LimitTemplates{}, projectBudgets: map[string]*access.BudgetPolicy{}, projectRouteGroups: map[string]access.RouteGroups{}, projects: map[string]string{}, projectName: map[string]string{}, projectEndUserPolicies: map[string]*access.EndUserPolicy{}, projectAttributionPolicies: map[string]*attribution.Policy{}, providers: map[string]*existingProvider{}, routes: map[string]*existingRoute{}, drafts: map[string]*existingDraft{}}
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
		v.projects[strings.ToLower(name)] = id
		v.projectName[id] = name
		v.projectOrganization[id] = organization
		v.projectEndUserPolicies[id] = policy
		v.projectAttributionPolicies[id] = labels
		v.projectRouteGroups[id] = groups
		v.projectBudgets[id] = budget
		v.projectTemplates[id] = templates
		v.projectAttributionBudgets[id] = caps
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
	var withImport []*existingProvider
	for providers.Next() {
		var p existingProvider
		var hasImport bool
		if err = providers.Scan(&p.ID, &p.Name, &p.Kind, &p.State, &p.ProjectID, &p.NetworkCredentialID, &hasImport); err != nil {
			return nil, err
		}
		p.Slots = map[string]existingSlot{}
		v.providers[strings.ToLower(p.Name)] = &p
		if hasImport {
			withImport = append(withImport, v.providers[strings.ToLower(p.Name)])
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
	byProvider := map[string]*existingProvider{}
	for _, p := range v.providers {
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
			p.Slots[strings.TrimSpace(name)] = existingSlot{ID: id, CredentialID: credentialID, PluginDigest: digest, ProfileID: profile}
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
		v.routes[slug] = &existingRoute{ID: id, ProjectID: projectID, State: state}
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
		key := routeKey(slug, v.projectOf(projectID))
		if _, ok := v.drafts[key]; !ok {
			v.drafts[key] = &existingDraft{ID: id, ProjectID: projectID}
		}
	}
	return v, drafts.Err()
}

func normalizeDocument(doc *Document) {
	for i := range doc.Projects {
		doc.Projects[i].Name = strings.TrimSpace(doc.Projects[i].Name)
		doc.Projects[i].Budget = normalizedBudget(doc.Projects[i].Budget)
		for _, members := range doc.Projects[i].RouteGroups {
			slices.Sort(members)
		}
	}
	for i := range doc.Providers {
		p := &doc.Providers[i]
		p.Name = strings.TrimSpace(p.Name)
		if p.Project != nil {
			v := strings.TrimSpace(*p.Project)
			p.Project = &v
		}
		for j := range p.Models {
			m := &p.Models[j]
			m.UpstreamModel = strings.TrimSpace(m.UpstreamModel)
			m.DisplayName = strings.TrimSpace(m.DisplayName)
		}

		for j := range p.Slots {
			slot := &p.Slots[j]
			slot.Name = strings.TrimSpace(slot.Name)
			if slot.CredentialRef != nil {
				v := strings.TrimSpace(*slot.CredentialRef)
				slot.CredentialRef = &v
			}
		}
	}
	for i := range doc.Routes {
		rt := &doc.Routes[i]
		rt.Slug = strings.TrimSpace(rt.Slug)
		if rt.Project != nil {
			v := strings.TrimSpace(*rt.Project)
			rt.Project = &v
		}
		for j := range rt.Targets {
			t := &rt.Targets[j]
			t.Provider = strings.TrimSpace(t.Provider)
			t.ProviderModel = strings.TrimSpace(t.ProviderModel)
		}
	}
	if doc.Pricing != nil {
		for i := range doc.Pricing.Prices {
			e := &doc.Pricing.Prices[i]
			if e.Provider != nil {
				v := strings.TrimSpace(*e.Provider)
				e.Provider = &v
			}
		}
	}
	for i := range doc.Discounts {
		d := &doc.Discounts[i]
		d.ProviderKind = strings.TrimSpace(d.ProviderKind)
		d.Multiplier = strings.TrimSpace(d.Multiplier)
		if d.VendorID != nil {
			v := strings.TrimSpace(*d.VendorID)
			d.VendorID = &v
		}
		if d.Provider != nil {
			v := strings.TrimSpace(*d.Provider)
			d.Provider = &v
		}
	}
	for i := range doc.RateCards {
		c := &doc.RateCards[i]
		c.Owner.Kind = strings.TrimSpace(c.Owner.Kind)
		c.Markup = strings.TrimSpace(c.Markup)
		if c.Owner.Organization != nil {
			v := strings.TrimSpace(*c.Owner.Organization)
			c.Owner.Organization = &v
		}
		if c.Owner.Project != nil {
			v := strings.TrimSpace(*c.Owner.Project)
			c.Owner.Project = &v
		}
		for j := range c.Prices {
			p := &c.Prices[j]
			p.Model = strings.TrimSpace(p.Model)
			p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
			if p.VendorID != nil {
				v := strings.TrimSpace(*p.VendorID)
				p.VendorID = &v
			}
			if p.Provider != nil {
				v := strings.TrimSpace(*p.Provider)
				p.Provider = &v
			}
		}
	}
}

func (s *Server) plan(ctx context.Context, q access.Queryer, doc *Document, bindings bindingSet, expected *string) (*planResult, error) {
	unavailable, err := s.validateDocument(ctx, q, doc)
	if err != nil {
		return nil, err
	}
	if err := validateBindings(doc, bindings); err != nil {
		return nil, err
	}
	doc.canonicalize()
	digest, err := CanonicalizeAndDigest(doc)
	if err != nil {
		return nil, err
	}
	result := &planResult{Digest: digest, Actions: []planItem{}, Conflicts: []planItem{}, Blockers: []planItem{}}
	if err := planCode(ctx, q, doc, result); err != nil {
		return nil, err
	}
	if err = planCatalogPolicies(ctx, q, doc, result); err != nil {
		return nil, err
	}
	for plugin, refusal := range unavailable {
		result.blocker("plugin", plugin, refusal)
	}
	state, err := loadState(ctx, q)
	if err != nil {
		return nil, err
	}
	if err = s.planAgentPublications(ctx, q, doc, bindings, state, result); err != nil {
		return nil, err
	}
	if err = s.planMCPServers(ctx, q, doc, bindings, state, result); err != nil {
		return nil, err
	}
	if err = planRetrievalBindings(ctx, q, doc, result); err != nil {
		return nil, err
	}
	if err = s.planSinks(ctx, q, doc, bindings, result); err != nil {
		return nil, err
	}
	if err = planGuardrailPolicies(ctx, q, doc, state, result); err != nil {
		return nil, err
	}
	if err = planGuardrails(ctx, q, doc, state, result); err != nil {
		return nil, err
	}
	if doc.SAML != nil {
		current, err := loadSAMLDefinition(ctx, q)
		if err != nil {
			return nil, err
		}
		action := "replace"
		if current == nil {
			action = "create"
		} else if reflect.DeepEqual(current, doc.SAML) {
			action = "reuse"
		}
		result.item("configuration", "saml", action, "Identity trust is portable; signing material and linked identities stay local.")
	}
	if err := planSCIMMappings(ctx, q, doc, state, result); err != nil {
		return nil, err
	}
	if err := planWorkloadIssuers(ctx, q, doc, state, result); err != nil {
		return nil, err
	}
	if expected != nil {
		current, err := s.exportDocument(ctx, q)
		if err != nil {
			return nil, err
		}
		currentDigest, err := CanonicalizeAndDigest(current)
		if err != nil {
			return nil, err
		}
		if *expected != currentDigest {
			result.conflict("configuration", "expected_digest", "configuration_changed")
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
		result.item("configuration", "budget_time_zone", action, "")
	}

	if doc.InstallationBudget != nil {
		var current *access.BudgetPolicy
		if err := q.QueryRow(ctx, "SELECT budget_policy FROM olp.installation WHERE singleton").Scan(&current); err != nil {
			return nil, err
		}
		action := "reuse"
		if !reflect.DeepEqual(current, normalizedBudget(doc.InstallationBudget)) {
			action = "replace"
		}
		result.item("configuration", "installation_budget", action, "")
	}
	projectNames := map[string]bool{}
	for name := range state.projects {
		projectNames[name] = true
	}
	if err = planOrganizations(ctx, q, doc, result); err != nil {
		return nil, err
	}
	for _, p := range doc.Projects {
		key := strings.ToLower(p.Name)
		projectNames[key] = true
	}
	for _, p := range doc.Projects {
		if id, ok := state.projects[strings.ToLower(p.Name)]; ok {
			action := "reuse"
			if p.Organization != nil {
				current := state.projectOrganization[id]
				if current != nil && !strings.EqualFold(*current, *p.Organization) {
					return nil, access.Fail(409, "project_organization_immutable", "A project cannot move between organizations.")
				}
				if current == nil {
					action = "replace"
				}
			}
			desired := projectEndUserPolicy(state.projectEndUserPolicies[id], p.EndUserDefaults, p.EndUserLimitTemplate)
			if !p.AttributionBudgets.Equal(state.projectAttributionBudgets[id]) || (p.LimitTemplates != nil && !reflect.DeepEqual(*p.LimitTemplates, state.projectTemplates[id])) || !reflect.DeepEqual(desired, state.projectEndUserPolicies[id]) || !reflect.DeepEqual(p.AttributionPolicy, state.projectAttributionPolicies[id]) || !p.RouteGroups.Equal(state.projectRouteGroups[id]) || !reflect.DeepEqual(p.Budget, state.projectBudgets[id]) {
				action = "replace"
			}
			result.item("project", p.Name, action, "")
		} else {
			result.item("project", p.Name, "create", "")
		}
	}
	docProviders := map[string]*ProviderEntry{}
	for i := range doc.Providers {
		docProviders[strings.ToLower(doc.Providers[i].Name)] = &doc.Providers[i]
	}
	for i := range doc.Providers {
		p := &doc.Providers[i]
		if p.Project != nil && !projectNames[strings.ToLower(*p.Project)] {
			result.blocker("provider", p.Name, "project_unknown")
		}
		existing, ok := state.providers[strings.ToLower(p.Name)]
		switch {
		case !ok:
			result.item("provider", p.Name, "create", "draft")
		case existing.Kind != p.Configuration.Kind:
			result.conflict("provider", p.Name, "provider_kind_changed")
		case lower(state.projectOf(existing.ProjectID)) != lower(p.Project):
			result.conflict("provider", p.Name, "provider_project_mismatch")
		default:
			current, err := s.currentProviderEntry(ctx, q, existing, state.projectOf(existing.ProjectID))
			if err != nil {
				return nil, err
			}
			if canonicalEqualProvider(p, current) {
				result.item("provider", p.Name, "noop", "")
			} else {
				result.item("provider", p.Name, "replace", "draft")
			}
		}
		if p.NetworkCredentialRef != nil {
			ref := *p.NetworkCredentialRef
			if _, supplied := bindings[ref]; supplied {
				result.item("network_credential", ref, "bind", "")
			} else if ok && existing.NetworkCredentialID != nil {
				result.item("network_credential", ref, "reuse", "")
			} else {
				result.blocker("network_credential", ref, "secret_binding_required")
			}
		}
		switch {
		case p.BillingImport != nil:
			ref := p.BillingImport.CredentialRef
			if _, supplied := bindings[ref]; supplied {
				result.item("billing_import", ref, "bind", "")
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
					result.item("billing_import", ref, "reuse", "")
				} else {
					result.blocker("billing_import", ref, "secret_binding_required")
				}
			} else {
				result.blocker("billing_import", ref, "secret_binding_required")
			}
		case ok && existing.BillingImport != nil:
			result.item("billing_import", billingRef(p.Name), "delete", "")
		}
		for j := range p.Slots {
			slot := &p.Slots[j]
			if slot.CredentialRef == nil {
				continue
			}
			ref := *slot.CredentialRef
			var current existingSlot
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
				if same, err := s.bindingMatches(ctx, q, current.CredentialID, secret); err == nil && same {
					result.item("credential", ref, "reuse", "")
				} else {
					result.item("credential", ref, "bind", "")
				}
			case fits:
				result.item("credential", ref, "reuse", "")
			case p.Configuration.Grant():
				result.item("credential", ref, "enroll", "grant_enrollment_required")
			default:
				result.blocker("credential", ref, "secret_binding_required")
			}
		}
	}
	for i := range doc.Routes {
		desired := doc.Routes[i]
		rt := &desired
		if rt.Project != nil && !projectNames[strings.ToLower(*rt.Project)] {
			result.blocker("route", rt.Slug, "project_unknown")
		}
		if existing, ok := state.routes[rt.Slug]; ok {
			if lower(state.projectOf(existing.ProjectID)) != lower(rt.Project) {
				result.conflict("route", rt.Slug, "route_project_mismatch")
			} else if rt.Retired != (existing.State == "retired") {
				code := "route_lifecycle_requires_activation"
				if rt.Retired {
					code = "route_lifecycle_requires_retirement"
				}
				result.blocker("route", rt.Slug, code)
			}
		}
		draft, staged := state.drafts[routeKey(rt.Slug, rt.Project)]
		switch {
		case !staged:
			result.item("route", rt.Slug, "stage", "route_draft")
		default:
			current, err := s.currentRouteEntry(ctx, q, draft.ID, rt, state)
			if err != nil {
				return nil, err
			}
			if canonicalEqualRoute(rt, current) {
				result.item("route", rt.Slug, "noop", "")
			} else {
				result.item("route", rt.Slug, "stage", "route_draft")
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
			result.blocker("route_template", desired.Name, "project_unknown")
		}
		existing := templateNamed(current, desired.Name)
		switch {
		case existing == nil:
			result.item("route_template", desired.Name, "create", "")
		case lower(existing.Project) != lower(desired.Project):
			result.conflict("route_template", desired.Name, "route_template_project_mismatch")
		case sameTemplate(desired, existing):
			result.item("route_template", desired.Name, "noop", "")
		default:
			result.item("route_template", desired.Name, "replace", "")
		}
	}
	if doc.Pricing != nil {
		changed, err := s.pricingChanged(ctx, q, doc)
		if err != nil {
			return nil, err
		}
		switch {
		case !changed:
			result.item("pricing", "pricing", "noop", "")
		default:
			effective, _ := time.Parse(time.RFC3339, doc.Pricing.EffectiveAt)
			detail := "revision"
			if !effective.After(time.Now()) {
				detail = "effective_at_rebased"
			}
			result.item("pricing", "pricing", "create", detail)
		}
	}
	if err := s.planDiscounts(ctx, q, doc, result); err != nil {
		return nil, err
	}
	if err := s.planRateCards(ctx, q, doc, result); err != nil {
		return nil, err
	}
	result.sort()
	if doc.RequireLocalMFA != nil {
		var current bool
		if err := q.QueryRow(ctx, "SELECT COALESCE((SELECT value='true' FROM olp.settings WHERE key='auth.mfa_required'),false)").Scan(&current); err != nil {
			return nil, err
		}
		action := "reuse"
		if current != *doc.RequireLocalMFA {
			action = "replace"
		}
		result.item("configuration", "require_local_mfa", action, "Change the local sign-in policy; factors, challenges and recovery codes remain installation-local.")
	}
	return result, nil
}

func (s *Server) pricingChanged(ctx context.Context, q access.Queryer, doc *Document) (bool, error) {
	revisions, _, err := usage.ListRevisions(ctx, q, nil, 1)
	if err != nil {
		return false, err
	}
	if len(revisions) == 0 {
		return true, nil
	}
	latest := revisions[0]
	providerNames, err := providerNameMap(ctx, q)
	if err != nil {
		return false, err
	}
	current := PricingEntry{EffectiveAt: latest.EffectiveAt.Format(time.RFC3339)}
	for _, price := range latest.Prices {
		entry := priceEntryFromUsage(price)
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
	normalizePrices := func(prices []PriceEntry) []PriceEntry {
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
		slices.SortFunc(copied, func(a, b PriceEntry) int {
			key := func(p *PriceEntry) string {
				return p.ProviderKind + "\x00" + lower(p.Provider) + "\x00" + lower(p.VendorID) + "\x00" + p.Model + "\x00" + p.Operation
			}
			return strings.Compare(key(&a), key(&b))
		})
		return copied
	}
	want := normalizePrices(desired.Prices)
	have := normalizePrices(current.Prices)
	for i := range want {
		if want[i].ProviderKind != have[i].ProviderKind ||
			lower(want[i].Provider) != lower(have[i].Provider) ||
			lower(want[i].VendorID) != lower(have[i].VendorID) ||
			want[i].Model != have[i].Model ||
			want[i].Operation != have[i].Operation ||
			!usage.SamePrice(want[i].usagePrice(), have[i].usagePrice()) {
			return true, nil
		}
	}
	return false, nil
}

func canonicalEqualProvider(desired, current *ProviderEntry) bool {
	a := *desired
	canonicalProvider(&a)
	b := *current
	named := make(map[string]bool, len(a.Models))
	for _, m := range a.Models {
		named[m.UpstreamModel] = true
	}
	// Apply keeps a model the document omits as a disabled row with no
	// capabilities, so that row is absent, not a difference. Filter into a new
	// slice: b shares its backing array with current.
	kept := make([]ModelEntry, 0, len(b.Models))
	for _, m := range b.Models {
		if !named[m.UpstreamModel] && !m.Enabled && len(m.Capabilities) == 0 {
			continue
		}
		kept = append(kept, m)
	}
	b.Models = kept
	canonicalProvider(&b)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func canonicalEqualRoute(desired, current *RouteEntry) bool {
	a := *desired
	a.Retired = false
	canonicalRoute(&a)
	b := *current
	b.Retired = false
	canonicalRoute(&b)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func (s *Server) currentProviderEntry(ctx context.Context, q access.Queryer, p *existingProvider, project *string) (*ProviderEntry, error) {
	var configuration []byte
	if err := q.QueryRow(ctx, "SELECT configuration FROM olp.providers WHERE id=$1", p.ID).Scan(&configuration); err != nil {
		return nil, err
	}
	entry := &ProviderEntry{Name: p.Name, Project: project}
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
	portableNetwork(entry)
	referenceGrants(entry)
	if err := attachBillingImport(ctx, q, entry, p.ID); err != nil {
		return nil, err
	}
	return entry, nil
}

func (s *Server) currentRouteEntry(ctx context.Context, q access.Queryer, draftID string, desired *RouteEntry, state *stateView) (*RouteEntry, error) {
	entry := &RouteEntry{Slug: desired.Slug, Project: desired.Project}
	var operations, targets, behavior []byte
	if err := q.QueryRow(ctx, "SELECT operations,overall_timeout_ms,max_attempts,targets,content_policy,fidelity,behavior,COALESCE(catalog_expose_upstream_models,(SELECT c.expose_upstream_models FROM olp.model_catalog_route_settings c JOIN olp.routes r ON r.id=c.route_id WHERE r.slug=route_drafts.slug),false) FROM olp.route_drafts WHERE id=$1", draftID).Scan(&operations, &entry.OverallTimeoutMS, &entry.MaxAttempts, &targets, &entry.ContentPolicy, &entry.Fidelity, &behavior, &entry.ExposeUpstreamModels); err != nil {
		return nil, err
	}
	if err := entry.setBehavior(behavior); err != nil {
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
		entry.Targets = append(entry.Targets, targetEntry(t))
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

func (s *Server) bindingMatches(ctx context.Context, q access.Queryer, credentialID *string, binding credentialBinding) (bool, error) {
	if credentialID == nil {
		return false, nil
	}
	if binding.reference != nil {
		var data []byte
		if err := q.QueryRow(ctx, "SELECT external_reference FROM olp.provider_credentials WHERE id=$1 AND revoked_at IS NULL", *credentialID).Scan(&data); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		var current secretstore.Reference
		return len(data) > 0 && json.Unmarshal(data, &current) == nil && current == *binding.reference, nil
	}
	if s.Access == nil || s.Access.Keys == nil {
		return false, errors.New("credential comparison unavailable")
	}
	current, err := s.Access.Keys.Read(ctx, q, s.Access.Installation, *credentialID, secrets.ProviderCredential)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer clear(current)
	return sha256.Sum256(current) == sha256.Sum256([]byte(binding.secret)), nil
}

func bindingFingerprint(doc *Document, digest string, bindings bindingSet, expected *string) map[string]any {
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}
	slices.Sort(names)
	sealed := make([]map[string]string, 0, len(names))
	for _, name := range names {
		value := []byte("sealed:" + bindings[name].secret)
		if bindings[name].reference != nil {
			value, _ = json.Marshal(bindings[name].reference)
			value = append([]byte("external:"), value...)
		}
		sum := sha256.Sum256(value)
		clear(value)
		sealed = append(sealed, map[string]string{"ref": name, "sha256": hex.EncodeToString(sum[:])})
	}
	if len(doc.codeBindings.Credentials)+len(doc.codeBindings.Users)+len(doc.codeBindings.APIKeys) == 0 {
		return map[string]any{"document_digest": digest, "expected_digest": expected, "secret_bindings": sealed}
	}
	return map[string]any{"document_digest": digest, "expected_digest": expected, "secret_bindings": sealed, "code_bindings": doc.codeBindings}
}

func templateNamed(templates []TemplateEntry, name string) *TemplateEntry {
	for i := range templates {
		if templates[i].Name == name {
			return &templates[i]
		}
	}
	return nil
}

// sameTemplate compares two normalized templates, ignoring project name case.
func sameTemplate(a, b *TemplateEntry) bool {
	x, y := *a, *b
	x.Project, y.Project = nil, nil
	if x.normalize() != nil || y.normalize() != nil {
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
