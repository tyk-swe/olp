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
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/budgetcalendar"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
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
}

type existingProvider struct {
	NetworkCredentialID *string
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
	providers, err := q.Query(ctx, "SELECT p.id::text,p.name,p.kind,p.state,p.project_id::text,(SELECT c.id::text FROM olp.provider_network_credentials c WHERE c.id::text=p.configuration#>>'{options,network,credential_id}' AND c.provider_id=p.id AND c.revoked_at IS NULL) FROM olp.providers p")
	if err != nil {
		return nil, err
	}
	defer providers.Close()
	for providers.Next() {
		var p existingProvider
		if err = providers.Scan(&p.ID, &p.Name, &p.Kind, &p.State, &p.ProjectID, &p.NetworkCredentialID); err != nil {
			return nil, err
		}
		p.Slots = map[string]existingSlot{}
		v.providers[strings.ToLower(p.Name)] = &p
	}
	if err = providers.Err(); err != nil {
		return nil, err
	}
	// A lapsed grant serves no more: the slot's credential reads as absent, so
	// a grant provider plans its enrollment again.
	slots, err := q.Query(ctx, `SELECT s.provider_id::text,s.name,s.id::text,c.id::text,coalesce(c.plugin_digest,'')
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
		var digest string
		if err = slots.Scan(&providerID, &name, &id, &credentialID, &digest); err != nil {
			return nil, err
		}
		if p, ok := byProvider[providerID]; ok {
			p.Slots[strings.TrimSpace(name)] = existingSlot{ID: id, CredentialID: credentialID, PluginDigest: digest}
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
}

// validateDocument refuses an artifact that is invalid anywhere, and pins the
// plugin profiles its plugin providers name. It returns the plugins this
// installation can't use yet, by digest, with why: they block the plan.
func (s *Server) validateDocument(ctx context.Context, q access.Queryer, doc *Document) (map[string]string, error) {
	if doc.APIVersion != APIVersion {
		return nil, access.Fail(422, "unsupported_api_version", "The artifact declares an unsupported api_version.")
	}
	normalizeDocument(doc)
	if err := s.validateSinks(doc); err != nil {
		return nil, err
	}
	if err := validateGuardrails(doc); err != nil {
		return nil, err
	}
	if err := validateSCIMMappings(doc); err != nil {
		return nil, err
	}
	if doc.SAML != nil {
		if err := access.ValidateSAMLDefinition(*doc.SAML); err != nil {
			return nil, err
		}
	}
	if err := validateWorkloadEntries(doc); err != nil {
		return nil, err
	}
	if err := validateOrganizations(ctx, q, doc); err != nil {
		return nil, err
	}
	if doc.BudgetTimeZone != nil {
		if _, err := budgetcalendar.New(*doc.BudgetTimeZone); err != nil {
			return nil, access.Invalid("budget_time_zone", "Use a supported IANA time zone.")
		}
	}
	if err := doc.InstallationBudget.Validate(); err != nil {
		return nil, err
	}
	if len(doc.Projects) > maxProjects {
		return nil, access.Invalid("projects", "Declare at most "+strconv.Itoa(maxProjects)+" projects.")
	}
	seen := map[string]bool{}
	for i, p := range doc.Projects {
		if err := p.Budget.Validate(); err != nil {
			return nil, err
		}
		if err := p.AttributionBudgets.Validate(); err != nil {
			return nil, err
		}
		if p.LimitTemplates != nil {
			if err := p.LimitTemplates.Validate(); err != nil {
				return nil, err
			}
		}
		if p.EndUserLimitTemplate != nil && !access.RouteSlug.MatchString(*p.EndUserLimitTemplate) {
			return nil, access.Invalid("end_user_limit_template", "Use a valid template name.")
		}
		if err := p.RouteGroups.Validate(); err != nil {
			return nil, err
		}

		for _, members := range p.RouteGroups {
			for _, route := range doc.Routes {
				if slices.Contains(members, route.Slug) && (route.Project == nil || !strings.EqualFold(*route.Project, p.Name)) {
					return nil, access.Invalid("route_groups", "Group routes must belong to their project.")
				}
			}
			var foreign bool
			if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r LEFT JOIN olp.projects p ON p.id=r.project_id WHERE r.slug=ANY($1::text[]) AND lower(p.name) IS DISTINCT FROM lower($2::text))`, members, p.Name).Scan(&foreign); err != nil {
				return nil, err
			}
			if foreign {
				return nil, access.Invalid("route_groups", "Group routes must belong to their project.")
			}
		}
		if p.AttributionPolicy != nil {
			if err := p.AttributionPolicy.Validate(); err != nil {
				return nil, access.Invalid("projects.attribution_policy", "Use at most four distinct valid attribution labels.")
			}
		}
		if p.EndUserDefaults != nil {
			if err := p.EndUserDefaults.Validate(); err != nil {
				return nil, err
			}
		}
		field := "projects." + strconv.Itoa(i) + ".name"
		if err := access.ValidText(field, p.Name, 100); err != nil {
			return nil, err
		}
		if seen[strings.ToLower(p.Name)] {
			return nil, access.Invalid(field, "Project names must be unique.")
		}
		seen[strings.ToLower(p.Name)] = true
	}
	if len(doc.Providers) > maxProviders {
		return nil, access.Invalid("providers", "Declare at most "+strconv.Itoa(maxProviders)+" providers.")
	}
	seen = map[string]bool{}
	refs := map[string]bool{}
	unavailable := map[string]string{}
	totalModels := 0
	for i := range doc.Providers {
		// Applying stores the entry's configuration, so a plugin provider
		// pins its plugin profile there and takes its address. One whose
		// plugin this installation can't use yet is validated with its
		// plugin profile once it can.
		refusal, err := pinPlugin(ctx, q, s.Unconfined, &doc.Providers[i].Configuration)
		if err != nil {
			return nil, err
		}
		p := doc.Providers[i]
		prefix := "providers." + strconv.Itoa(i)
		if err := access.ValidText(prefix+".name", p.Name, 100); err != nil {
			return nil, err
		}
		if seen[strings.ToLower(p.Name)] {
			return nil, access.Invalid(prefix+".name", "Provider names must be unique.")
		}
		seen[strings.ToLower(p.Name)] = true
		if p.Project != nil {
			if err := access.ValidText(prefix+".project", *p.Project, 100); err != nil {
				return nil, err
			}
		}
		p.Configuration.Normalize()
		if p.Configuration.Options.Network != nil && p.Configuration.Options.Network.CredentialID != "" {
			return nil, access.Invalid(prefix+".configuration.options.network.credential_id", "Configuration artifacts use network_credential_ref instead of environment-specific credential IDs.")
		}
		if p.NetworkCredentialRef != nil && (p.Configuration.Options.Network == nil || *p.NetworkCredentialRef != networkRef(p.Name) || len(*p.NetworkCredentialRef) > maxCredentialRef) {
			return nil, access.Invalid(prefix+".network_credential_ref", "Use the provider name followed by /network and configure network options.")
		}
		if p.NetworkCredentialRef != nil {
			if refs[*p.NetworkCredentialRef] {
				return nil, access.Invalid(prefix+".network_credential_ref", "Credential references must be distinct.")
			}
			refs[*p.NetworkCredentialRef] = true
		}
		if refusal != "" {
			unavailable[p.Configuration.ProfileRevision] = refusal
		} else if err := p.Configuration.Validate(s.Egress); err != nil {
			return nil, err
		}
		totalModels += len(p.Models)
		if totalModels > maxModels {
			return nil, access.Invalid("providers.models", "Declare at most "+strconv.Itoa(maxModels)+" models in total.")
		}
		modelSeen := map[string]bool{}
		for j, m := range p.Models {
			mfield := prefix + ".models." + strconv.Itoa(j)
			if err := providers.ValidModelName(mfield+".upstream_model", m.UpstreamModel); err != nil {
				return nil, err
			}
			if modelSeen[m.UpstreamModel] {
				return nil, access.Invalid(mfield+".upstream_model", "Model names must be unique per provider.")
			}
			modelSeen[m.UpstreamModel] = true
			if m.DisplayName == "" {
				m.DisplayName = m.UpstreamModel
			}
			if err := providers.ValidModelName(mfield+".display_name", m.DisplayName); err != nil {
				return nil, err
			}
			capabilities := make([]providers.CapabilityInput, 0, len(m.Capabilities))
			for _, c := range m.Capabilities {
				capabilities = append(capabilities, providers.CapabilityInput{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode})
			}
			if _, err := providers.ValidCapabilities(capabilities); err != nil {
				return nil, err
			}
		}
		if len(p.Slots) < 1 || len(p.Slots) > maxSlots {
			return nil, access.Invalid(prefix+".slots", "Declare 1–"+strconv.Itoa(maxSlots)+" credential slots per provider.")
		}
		positions, names := map[int]bool{}, map[string]bool{}
		defaults := 0
		for j, slot := range p.Slots {
			sfield := prefix + ".slots." + strconv.Itoa(j)
			if err := access.ValidText(sfield+".name", slot.Name, 100); err != nil {
				return nil, err
			}
			if names[slot.Name] {
				return nil, access.Invalid(sfield+".name", "Slot names must be unique per provider.")
			}
			names[slot.Name] = true
			if slot.Position < 0 || slot.Position > 32767 || positions[slot.Position] {
				return nil, access.Invalid(sfield+".position", "Use unique positions from 0 to 32767.")
			}
			positions[slot.Position] = true
			if slot.IsDefault {
				defaults++
			}
			if slot.Priority < 0 || slot.Priority > 32767 {
				return nil, access.Invalid(sfield+".priority", "Use a priority from 0 to 32767.")
			}
			if slot.Weight < 1 || slot.Weight > 1000000 {
				return nil, access.Invalid(sfield+".weight", "Use a weight from 1 to 1000000.")
			}
			if len(slot.Restrictions.AllowedAPIKeys) > 0 {
				return nil, access.Fail(422, "non_portable_reference", "allowed_api_keys cannot be promoted; re-establish them on the destination after creating keys.")
			}
			if len(slot.Restrictions.AllowedRoutes) > 100 || len(slot.Restrictions.AllowedModels) > 2000 {
				return nil, access.Invalid(sfield, "Use at most 100 routes and 2000 models per slot.")
			}
			for _, route := range slot.Restrictions.AllowedRoutes {
				if !access.RouteSlug.MatchString(route) {
					return nil, access.Invalid(sfield+".allowed_routes", "Use route slugs.")
				}
			}
			for _, model := range slot.Restrictions.AllowedModels {
				if err := providers.ValidModelName(sfield+".allowed_models", model); err != nil {
					return nil, err
				}
			}
			if err := providers.ValidLimits(sfield+".limits", slot.Limits); err != nil {
				return nil, err
			}
			if slot.CredentialRef != nil {
				want := CredentialRef(p.Name, slot.Name)
				if *slot.CredentialRef != want {
					return nil, access.Invalid(sfield+".credential_ref", "Credential references must read "+want+".")
				}
				if len(*slot.CredentialRef) > maxCredentialRef {
					return nil, access.Invalid(sfield+".credential_ref", "Credential references must fit within 200 bytes.")
				}
				if refs[*slot.CredentialRef] {
					return nil, access.Invalid(sfield+".credential_ref", "Credential references must be unique.")
				}
				refs[*slot.CredentialRef] = true
			} else if p.Configuration.Grant() {
				return nil, access.Invalid(sfield+".credential_ref", "A grant backs each of this provider's credential slots: reference its credential as "+CredentialRef(p.Name, slot.Name)+".")
			}
		}
		if defaults != 1 {
			return nil, access.Invalid(prefix+".slots", "Declare exactly one default slot per provider.")
		}
		if p.Configuration.CredentialRequired() {
			hasCredential := false
			for _, slot := range p.Slots {
				hasCredential = hasCredential || slot.CredentialRef != nil
			}
			if !hasCredential {
				return nil, access.Invalid(prefix+".slots", "This authentication mode requires at least one credential slot.")
			}
		} else {
			for _, slot := range p.Slots {
				if slot.CredentialRef != nil {
					return nil, access.Invalid(prefix+".slots.credential_ref", "This authentication mode takes no stored credential.")
				}
			}
		}
	}
	if len(doc.Routes) > maxRoutes {
		return nil, access.Invalid("routes", "Declare at most "+strconv.Itoa(maxRoutes)+" routes.")
	}
	routeSeen := map[string]bool{}
	docProviders := map[string]*ProviderEntry{}
	for i := range doc.Providers {
		docProviders[strings.ToLower(doc.Providers[i].Name)] = &doc.Providers[i]
	}
	for i, rt := range doc.Routes {
		prefix := "routes." + strconv.Itoa(i)
		if _, err := runtime.DecodeFidelity(rt.Fidelity); err != nil {
			return nil, access.Invalid(prefix+".fidelity", err.Error())
		}
		if err := routes.ValidateFidelityPolicy(rt.Fidelity, rt.ContentPolicy); err != nil {
			return nil, err
		}
		if routeSeen[rt.Slug] {
			return nil, access.Invalid(prefix+".slug", "Route slugs must be unique.")
		}
		routeSeen[rt.Slug] = true
		if !access.RouteSlug.MatchString(rt.Slug) {
			return nil, access.Invalid(prefix+".slug", "Use 1-100 lowercase letters, digits, dots, underscores, or hyphens, starting with a letter or digit.")
		}
		if len(rt.Targets) > maxTargets {
			return nil, access.Invalid(prefix+".targets", "Declare at most "+strconv.Itoa(maxTargets)+" targets per route.")
		}
		if rt.Project != nil {
			if err := access.ValidText(prefix+".project", *rt.Project, 100); err != nil {
				return nil, err
			}
		}
		if rt.RoutingPolicy != nil {
			if err := rt.RoutingPolicy.Validate(); err != nil {
				return nil, err
			}
		}
		tagged := func(tag string) bool {
			return slices.ContainsFunc(rt.Targets, func(t TargetEntry) bool { return t.Shadow == nil && slices.Contains(t.Tags, tag) })
		}
		if err := rt.behavior().Validate(rt.Slug, tagged); err != nil {
			return nil, err
		}
		for j, t := range rt.Targets {
			tfield := prefix + ".targets." + strconv.Itoa(j)
			provider, ok := docProviders[strings.ToLower(t.Provider)]
			if !ok {
				return nil, access.Invalid(tfield+".provider", "Target "+strconv.Itoa(j)+" names a provider the document does not declare.")
			}
			found := false
			for _, m := range provider.Models {
				found = found || m.UpstreamModel == t.ProviderModel
			}
			if !found {
				return nil, access.Invalid(tfield+".provider_model", "Target "+strconv.Itoa(j)+" names a model the provider does not declare.")
			}
			if lower(rt.Project) != lower(provider.Project) {
				return nil, access.Fail(422, "target_project_mismatch", "Target "+strconv.Itoa(j)+" belongs to a different project than the route.")
			}
		}
	}
	templateNames := map[string]bool{}
	for i := range doc.Templates {
		t := &doc.Templates[i]
		field := "templates." + strconv.Itoa(i)
		if templateNames[t.Name] {
			return nil, access.Invalid(field+".name", "Template names must be unique.")
		}
		templateNames[t.Name] = true
		if err := t.normalize(); err != nil {
			return nil, withFieldPrefix(err, field)
		}
	}
	if doc.Pricing != nil {
		if _, err := time.Parse(time.RFC3339, doc.Pricing.EffectiveAt); err != nil {
			return nil, access.Invalid("pricing.effective_at", "Use an RFC3339 date and time.")
		}
		if len(doc.Pricing.Prices) > maxPrices {
			return nil, access.Invalid("pricing.prices", "Declare at most "+strconv.Itoa(maxPrices)+" prices.")
		}
		// A price that names an unknown provider would otherwise apply to every
		// provider of its kind. Destination providers resolve as apply does.
		var destination map[string]bool
		for i, e := range doc.Pricing.Prices {
			if e.Provider == nil {
				continue
			}
			name := strings.ToLower(*e.Provider)
			if _, ok := docProviders[name]; ok {
				continue
			}
			if destination == nil {
				names, err := providerNameMap(ctx, q)
				if err != nil {
					return nil, err
				}
				destination = map[string]bool{}
				for _, n := range names {
					destination[strings.ToLower(n)] = true
				}
			}
			if !destination[name] {
				return nil, access.Invalid("pricing.prices."+strconv.Itoa(i)+".provider", "Price "+strconv.Itoa(i)+" names a provider neither the document nor this installation declares.")
			}
		}
	}
	return unavailable, nil
}

func validateBindings(doc *Document, bindings bindingSet) error {
	refs, grants := map[string]bool{}, map[string]bool{}
	sinkRefs := map[string]bool{}
	for _, entry := range doc.Sinks {
		if entry.CredentialRef != nil {
			refs[*entry.CredentialRef] = true
			sinkRefs[*entry.CredentialRef] = true
		}
	}
	for _, p := range doc.Providers {
		if p.NetworkCredentialRef != nil {
			refs[*p.NetworkCredentialRef] = true
		}
		for _, slot := range p.Slots {
			if slot.CredentialRef != nil {
				refs[*slot.CredentialRef] = true
				grants[*slot.CredentialRef] = p.Configuration.Grant()
			}
		}
	}
	for name, secret := range bindings {
		if sinkRefs[name] {
			if secret.reference != nil || len(secret.secret) < 1 || len(secret.secret) > 4096 {
				return access.Invalid("secret_bindings", "Sink signing credentials require 1–4096 sealed bytes.")
			}
			continue
		}
		if grants[name] {
			return access.Invalid("secret_bindings."+name, grantBindingRefused)
		}
		if secret.reference != nil {
			if !refs[name] || secret.reference.Validate() != nil {
				return access.Invalid("external_credential_bindings", "Use a document reference and an immutable store version.")
			}
			for _, provider := range doc.Providers {
				if provider.NetworkCredentialRef != nil && *provider.NetworkCredentialRef == name {
					return access.Invalid("external_credential_bindings", "Network identities require sealed secret bindings.")
				}
			}
			continue
		}
		for _, provider := range doc.Providers {
			if provider.NetworkCredentialRef != nil && *provider.NetworkCredentialRef == name {
				if err := egress.ValidateConnectionSecret([]byte(secret.secret)); err != nil {
					return access.Invalid("secret_bindings."+name, err.Error())
				}
			}
		}
		if !refs[name] {
			return access.Invalid("secret_bindings", "Secret binding "+name+" is not referenced by the document.")
		}
		if err := providers.ValidCredential(secret.secret); err != nil {
			return access.Invalid("secret_bindings."+name, "Use a credential of 1–65536 bytes.")
		}
		if len(secret.secret) > maxSecretBytes {
			return access.Invalid("secret_bindings."+name, "Secrets must fit within 64 KiB.")
		}
	}
	return nil
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
	digest, err := Digest(doc)
	if err != nil {
		return nil, err
	}
	result := &planResult{Digest: digest, Actions: []planItem{}, Conflicts: []planItem{}, Blockers: []planItem{}}
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
	if err = s.planSinks(ctx, q, doc, bindings, result); err != nil {
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
		currentDigest, err := Digest(current)
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
			fits := current.CredentialID != nil && p.Configuration.Authenticates(current.PluginDigest)
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
		entry := PriceEntry{VendorID: price.VendorID, ProviderKind: price.ProviderKind, Model: price.Model, Operation: price.Operation,
			InputPerMillion: price.InputPerMillion, CachedInputPerMillion: price.CachedInputPerMillion,
			OutputPerMillion: price.OutputPerMillion, CacheWriteInputPerMillion: price.CacheWriteInputPerMillion,
			CacheWrite5MInputPerMillion: price.CacheWrite5MInputPerMillion,
			CacheWrite1HInputPerMillion: price.CacheWrite1HInputPerMillion,
			UnitPrice:                   price.UnitPrice, Currency: price.Currency}
		if price.ProviderID != nil {
			name := providerNames[*price.ProviderID]
			entry.Provider = &name
		}
		current.Prices = append(current.Prices, entry)
	}
	desired := *doc.Pricing
	normalizePricing := func(p *PricingEntry) string {
		for i := range p.Prices {
			e := &p.Prices[i]
			e.Model = strings.TrimSpace(e.Model)
			e.Currency = strings.ToUpper(strings.TrimSpace(e.Currency))
			if e.VendorID != nil {
				v := strings.TrimSpace(*e.VendorID)
				e.VendorID = &v
			}
			if e.Provider != nil {
				v := strings.ToLower(*e.Provider)
				e.Provider = &v
			}
		}
		slices.SortFunc(p.Prices, func(a, b PriceEntry) int {
			key := func(p *PriceEntry) string {
				return p.ProviderKind + "\x00" + lower(p.Provider) + "\x00" + lower(p.VendorID) + "\x00" + p.Model + "\x00" + p.Operation
			}
			return strings.Compare(key(&a), key(&b))
		})
		data, _ := json.Marshal(p)
		return string(data)
	}
	desired.EffectiveAt = ""
	current.EffectiveAt = ""
	return normalizePricing(&desired) != normalizePricing(&current), nil
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
	var published []runtime.PublishedTarget
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
		var p runtime.Policy
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
	return map[string]any{"document_digest": digest, "expected_digest": expected, "secret_bindings": sealed}
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
