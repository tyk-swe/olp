package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
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
	Fidelity  json.RawMessage
}

type existingDraft struct {
	ID        string
	ProjectID *string
}

type stateView struct {
	projects    map[string]string
	projectName map[string]string
	providers   map[string]*existingProvider
	routes      map[string]*existingRoute
	drafts      map[string]*existingDraft
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
	v := &stateView{projects: map[string]string{}, projectName: map[string]string{}, providers: map[string]*existingProvider{}, routes: map[string]*existingRoute{}, drafts: map[string]*existingDraft{}}
	rows, err := q.Query(ctx, "SELECT id::text,name FROM olp_go.projects")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		v.projects[strings.ToLower(name)] = id
		v.projectName[id] = name
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	providers, err := q.Query(ctx, "SELECT p.id::text,p.name,p.kind,p.state,p.project_id::text,(SELECT c.id::text FROM olp_go.provider_network_credentials c WHERE c.id::text=p.configuration#>>'{options,network,credential_id}' AND c.provider_id=p.id AND c.revoked_at IS NULL) FROM olp_go.providers p")
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
	slots, err := q.Query(ctx, `SELECT s.provider_id::text,s.name,s.id::text,c.id::text
        FROM olp_go.provider_slots s LEFT JOIN olp_go.provider_credentials c ON c.id=s.credential_id AND c.revoked_at IS NULL`)
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
		if err = slots.Scan(&providerID, &name, &id, &credentialID); err != nil {
			return nil, err
		}
		if p, ok := byProvider[providerID]; ok {
			p.Slots[strings.TrimSpace(name)] = existingSlot{ID: id, CredentialID: credentialID}
		}
	}
	if err = slots.Err(); err != nil {
		return nil, err
	}
	routes, err := q.Query(ctx, "SELECT r.id::text,r.slug,r.project_id::text,r.state,v.fidelity FROM olp_go.routes r JOIN olp_go.route_revisions v ON v.id=r.latest_revision_id")
	if err != nil {
		return nil, err
	}
	defer routes.Close()
	for routes.Next() {
		var id, slug, state string
		var projectID *string
		var fidelity []byte
		if err = routes.Scan(&id, &slug, &projectID, &state, &fidelity); err != nil {
			return nil, err
		}
		v.routes[slug] = &existingRoute{ID: id, ProjectID: projectID, State: state, Fidelity: fidelity}
	}
	if err = routes.Err(); err != nil {
		return nil, err
	}
	drafts, err := q.Query(ctx, "SELECT id::text,slug,project_id::text FROM olp_go.route_drafts WHERE state IN ('draft','validated') AND based_on_revision_id IS NULL ORDER BY created_at DESC,id DESC")
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

func (s *Server) validateDocument(doc *Document) error {
	if doc.APIVersion != APIVersion {
		return access.Fail(422, "unsupported_api_version", "The artifact declares an unsupported api_version.")
	}
	normalizeDocument(doc)
	if len(doc.Projects) > maxProjects {
		return access.Invalid("projects", "Declare at most "+strconv.Itoa(maxProjects)+" projects.")
	}
	seen := map[string]bool{}
	for i, p := range doc.Projects {
		field := "projects." + strconv.Itoa(i) + ".name"
		if err := access.ValidText(field, p.Name, 100); err != nil {
			return err
		}
		if seen[strings.ToLower(p.Name)] {
			return access.Invalid(field, "Project names must be unique.")
		}
		seen[strings.ToLower(p.Name)] = true
	}
	if len(doc.Providers) > maxProviders {
		return access.Invalid("providers", "Declare at most "+strconv.Itoa(maxProviders)+" providers.")
	}
	seen = map[string]bool{}
	refs := map[string]bool{}
	totalModels := 0
	for i, p := range doc.Providers {
		prefix := "providers." + strconv.Itoa(i)
		if err := access.ValidText(prefix+".name", p.Name, 100); err != nil {
			return err
		}
		if seen[strings.ToLower(p.Name)] {
			return access.Invalid(prefix+".name", "Provider names must be unique.")
		}
		seen[strings.ToLower(p.Name)] = true
		if p.Project != nil {
			if err := access.ValidText(prefix+".project", *p.Project, 100); err != nil {
				return err
			}
		}
		p.Configuration.Normalize()
		if p.Configuration.Options.Network != nil && p.Configuration.Options.Network.CredentialID != "" {
			return access.Invalid(prefix+".configuration.options.network.credential_id", "Configuration artifacts use network_credential_ref instead of environment-specific credential IDs.")
		}
		if p.NetworkCredentialRef != nil && (p.Configuration.Options.Network == nil || *p.NetworkCredentialRef != networkRef(p.Name) || len(*p.NetworkCredentialRef) > maxCredentialRef) {
			return access.Invalid(prefix+".network_credential_ref", "Use the provider name followed by /network and configure network options.")
		}
		if p.NetworkCredentialRef != nil {
			if refs[*p.NetworkCredentialRef] {
				return access.Invalid(prefix+".network_credential_ref", "Credential references must be distinct.")
			}
			refs[*p.NetworkCredentialRef] = true
		}
		if err := p.Configuration.Validate(s.Egress); err != nil {
			return err
		}
		totalModels += len(p.Models)
		if totalModels > maxModels {
			return access.Invalid("providers.models", "Declare at most "+strconv.Itoa(maxModels)+" models in total.")
		}
		modelSeen := map[string]bool{}
		for j, m := range p.Models {
			mfield := prefix + ".models." + strconv.Itoa(j)
			if err := providers.ValidModelName(mfield+".upstream_model", m.UpstreamModel); err != nil {
				return err
			}
			if modelSeen[m.UpstreamModel] {
				return access.Invalid(mfield+".upstream_model", "Model names must be unique per provider.")
			}
			modelSeen[m.UpstreamModel] = true
			if m.DisplayName == "" {
				m.DisplayName = m.UpstreamModel
			}
			if err := providers.ValidModelName(mfield+".display_name", m.DisplayName); err != nil {
				return err
			}
			capabilities := make([]providers.CapabilityInput, 0, len(m.Capabilities))
			for _, c := range m.Capabilities {
				capabilities = append(capabilities, providers.CapabilityInput{Operation: c.Operation, Surface: c.Surface, Mode: c.Mode})
			}
			if _, err := providers.ValidCapabilities(capabilities); err != nil {
				return err
			}
		}
		if len(p.Slots) < 1 || len(p.Slots) > maxSlots {
			return access.Invalid(prefix+".slots", "Declare 1–"+strconv.Itoa(maxSlots)+" credential slots per provider.")
		}
		positions, names := map[int]bool{}, map[string]bool{}
		defaults := 0
		for j, slot := range p.Slots {
			sfield := prefix + ".slots." + strconv.Itoa(j)
			if err := access.ValidText(sfield+".name", slot.Name, 100); err != nil {
				return err
			}
			if names[slot.Name] {
				return access.Invalid(sfield+".name", "Slot names must be unique per provider.")
			}
			names[slot.Name] = true
			if slot.Position < 0 || slot.Position > 32767 || positions[slot.Position] {
				return access.Invalid(sfield+".position", "Use unique positions from 0 to 32767.")
			}
			positions[slot.Position] = true
			if slot.IsDefault {
				defaults++
			}
			if slot.Priority < 0 || slot.Priority > 32767 {
				return access.Invalid(sfield+".priority", "Use a priority from 0 to 32767.")
			}
			if slot.Weight < 1 || slot.Weight > 1000000 {
				return access.Invalid(sfield+".weight", "Use a weight from 1 to 1000000.")
			}
			if len(slot.Restrictions.AllowedAPIKeys) > 0 {
				return access.Fail(422, "non_portable_reference", "allowed_api_keys cannot be promoted; re-establish them on the destination after creating keys.")
			}
			if len(slot.Restrictions.AllowedRoutes) > 100 || len(slot.Restrictions.AllowedModels) > 2000 {
				return access.Invalid(sfield, "Use at most 100 routes and 2000 models per slot.")
			}
			for _, route := range slot.Restrictions.AllowedRoutes {
				if !access.RouteSlug.MatchString(route) {
					return access.Invalid(sfield+".allowed_routes", "Use route slugs.")
				}
			}
			for _, model := range slot.Restrictions.AllowedModels {
				if err := providers.ValidModelName(sfield+".allowed_models", model); err != nil {
					return err
				}
			}
			if !providers.ValidQuota(slot.Limits) {
				return access.Invalid(sfield, "Use positive limits: requests and concurrency at most 2147483647, tokens at most 9007199254740991.")
			}
			if slot.CredentialRef != nil {
				want := CredentialRef(p.Name, slot.Name)
				if *slot.CredentialRef != want {
					return access.Invalid(sfield+".credential_ref", "Credential references must read "+want+".")
				}
				if len(*slot.CredentialRef) > maxCredentialRef {
					return access.Invalid(sfield+".credential_ref", "Credential references must fit within 200 bytes.")
				}
				if refs[*slot.CredentialRef] {
					return access.Invalid(sfield+".credential_ref", "Credential references must be unique.")
				}
				refs[*slot.CredentialRef] = true
			}
		}
		if defaults != 1 {
			return access.Invalid(prefix+".slots", "Declare exactly one default slot per provider.")
		}
		if p.Configuration.CredentialRequired() {
			hasCredential := false
			for _, slot := range p.Slots {
				hasCredential = hasCredential || slot.CredentialRef != nil
			}
			if !hasCredential {
				return access.Invalid(prefix+".slots", "This authentication mode requires at least one credential slot.")
			}
		} else {
			for _, slot := range p.Slots {
				if slot.CredentialRef != nil {
					return access.Invalid(prefix+".slots.credential_ref", "This authentication mode takes no stored credential.")
				}
			}
		}
	}
	if len(doc.Routes) > maxRoutes {
		return access.Invalid("routes", "Declare at most "+strconv.Itoa(maxRoutes)+" routes.")
	}
	routeSeen := map[string]bool{}
	docProviders := map[string]*ProviderEntry{}
	for i := range doc.Providers {
		docProviders[strings.ToLower(doc.Providers[i].Name)] = &doc.Providers[i]
	}
	for i, rt := range doc.Routes {
		prefix := "routes." + strconv.Itoa(i)
		if err := routes.ValidateFidelityPolicy(rt.Fidelity, rt.ContentPolicy); err != nil {
			return err
		}
		if routeSeen[rt.Slug] {
			return access.Invalid(prefix+".slug", "Route slugs must be unique.")
		}
		routeSeen[rt.Slug] = true
		if !access.RouteSlug.MatchString(rt.Slug) {
			return access.Invalid(prefix+".slug", "Use 1-100 lowercase letters, digits, dots, underscores, or hyphens, starting with a letter or digit.")
		}
		if len(rt.Targets) > maxTargets {
			return access.Invalid(prefix+".targets", "Declare at most "+strconv.Itoa(maxTargets)+" targets per route.")
		}
		if rt.Project != nil {
			if err := access.ValidText(prefix+".project", *rt.Project, 100); err != nil {
				return err
			}
		}
		if rt.RoutingPolicy != nil {
			if err := rt.RoutingPolicy.Validate(); err != nil {
				return err
			}
		}
		for j, t := range rt.Targets {
			tfield := prefix + ".targets." + strconv.Itoa(j)
			provider, ok := docProviders[strings.ToLower(t.Provider)]
			if !ok {
				return access.Invalid(tfield+".provider", "Target "+strconv.Itoa(j)+" names a provider the document does not declare.")
			}
			found := false
			for _, m := range provider.Models {
				found = found || m.UpstreamModel == t.ProviderModel
			}
			if !found {
				return access.Invalid(tfield+".provider_model", "Target "+strconv.Itoa(j)+" names a model the provider does not declare.")
			}
			if lower(rt.Project) != lower(provider.Project) {
				return access.Fail(422, "target_project_mismatch", "Target "+strconv.Itoa(j)+" belongs to a different project than the route.")
			}
		}
	}
	if doc.Pricing != nil {
		if _, err := time.Parse(time.RFC3339, doc.Pricing.EffectiveAt); err != nil {
			return access.Invalid("pricing.effective_at", "Use an RFC3339 date and time.")
		}
		if len(doc.Pricing.Prices) > maxPrices {
			return access.Invalid("pricing.prices", "Declare at most "+strconv.Itoa(maxPrices)+" prices.")
		}
	}
	return nil
}

func validateBindings(doc *Document, bindings map[string]string) error {
	refs := map[string]bool{}
	for _, p := range doc.Providers {
		if p.NetworkCredentialRef != nil {
			refs[*p.NetworkCredentialRef] = true
		}
		for _, slot := range p.Slots {
			if slot.CredentialRef != nil {
				refs[*slot.CredentialRef] = true
			}
		}
	}
	for name, secret := range bindings {
		for _, provider := range doc.Providers {
			if provider.NetworkCredentialRef != nil && *provider.NetworkCredentialRef == name {
				if err := egress.ValidateConnectionSecret([]byte(secret)); err != nil {
					return access.Invalid("secret_bindings."+name, err.Error())
				}
			}
		}
		if !refs[name] {
			return access.Invalid("secret_bindings", "Secret binding "+name+" is not referenced by the document.")
		}
		if err := providers.ValidCredential(secret); err != nil {
			return access.Invalid("secret_bindings."+name, "Use a credential of 1–65536 bytes.")
		}
		if len(secret) > maxSecretBytes {
			return access.Invalid("secret_bindings."+name, "Secrets must fit within 64 KiB.")
		}
	}
	return nil
}

func (s *Server) plan(ctx context.Context, q access.Queryer, doc *Document, bindings map[string]string, expected *string) (*planResult, error) {
	if err := s.validateDocument(doc); err != nil {
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
	state, err := loadState(ctx, q)
	if err != nil {
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
	projectNames := map[string]bool{}
	for name := range state.projects {
		projectNames[name] = true
	}
	for _, p := range doc.Projects {
		key := strings.ToLower(p.Name)
		projectNames[key] = true
	}
	for _, p := range doc.Projects {
		if _, ok := state.projects[strings.ToLower(p.Name)]; ok {
			result.item("project", p.Name, "reuse", "")
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
			if secret, supplied := bindings[ref]; supplied {
				var live *string
				if ok {
					if current, present := existing.Slots[slot.Name]; present {
						live = current.CredentialID
					}
				}
				if same, err := s.bindingMatches(ctx, q, live, secret); err == nil && same {
					result.item("credential", ref, "reuse", "")
				} else {
					result.item("credential", ref, "bind", "")
				}
			} else if ok && existing.Slots[slot.Name].CredentialID != nil {
				result.item("credential", ref, "reuse", "")
			} else {
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
		if !staged && len(rt.Fidelity) == 0 {
			if existing, ok := state.routes[rt.Slug]; ok && lower(state.projectOf(existing.ProjectID)) == lower(rt.Project) {
				rt.Fidelity = existing.Fidelity
			}
		}
		if !staged {
			if err := routes.ValidateFidelityPolicy(rt.Fidelity, rt.ContentPolicy); err != nil {
				result.conflict("route", rt.Slug, "fidelity_policy_conflict")
				continue
			}
		}
		switch {
		case !staged:
			result.item("route", rt.Slug, "stage", "route_draft")
		default:
			current, err := s.currentRouteEntry(ctx, q, draft.ID, rt, state)
			if err != nil {
				return nil, err
			}
			if len(rt.Fidelity) == 0 {
				rt.Fidelity = current.Fidelity
			}
			if err := routes.ValidateFidelityPolicy(rt.Fidelity, rt.ContentPolicy); err != nil {
				result.conflict("route", rt.Slug, "fidelity_policy_conflict")
				continue
			}
			if canonicalEqualRoute(rt, current) {
				result.item("route", rt.Slug, "noop", "")
			} else {
				result.item("route", rt.Slug, "stage", "route_draft")
			}
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
	if err := q.QueryRow(ctx, "SELECT configuration FROM olp_go.providers WHERE id=$1", p.ID).Scan(&configuration); err != nil {
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
	return entry, nil
}

func (s *Server) currentRouteEntry(ctx context.Context, q access.Queryer, draftID string, desired *RouteEntry, state *stateView) (*RouteEntry, error) {
	entry := &RouteEntry{Slug: desired.Slug, Project: desired.Project}
	var operations, targets []byte
	if err := q.QueryRow(ctx, "SELECT operations,overall_timeout_ms,max_attempts,targets,content_policy,fidelity FROM olp_go.route_drafts WHERE id=$1", draftID).Scan(&operations, &entry.OverallTimeoutMS, &entry.MaxAttempts, &targets, &entry.ContentPolicy, &entry.Fidelity); err != nil {
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
		entry.Targets = append(entry.Targets, TargetEntry{Provider: t.ProviderName, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: int(t.TimeoutMS)})
	}
	var policy []byte
	err := q.QueryRow(ctx, "SELECT policy FROM olp_go.routing_policies WHERE scope='route-draft' AND scope_id=$1", draftID).Scan(&policy)
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

func (s *Server) bindingMatches(ctx context.Context, q access.Queryer, credentialID *string, secret string) (bool, error) {
	if credentialID == nil {
		return false, nil
	}
	if s.Access == nil || s.Access.Keys == nil {
		return false, errors.New("credential comparison unavailable")
	}
	var version int
	var encrypted []byte
	err := q.QueryRow(ctx, "SELECT key_version,ciphertext FROM olp_go.secrets WHERE id=$1 AND purpose='provider_credential' AND (expires_at IS NULL OR expires_at>now())", *credentialID).Scan(&version, &encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current, err := s.Access.Keys.Open(s.Access.Installation, "provider_credential", *credentialID, version, encrypted)
	if err != nil {
		return false, err
	}
	return sha256.Sum256(current) == sha256.Sum256([]byte(secret)), nil
}

func bindingFingerprint(doc *Document, digest string, bindings map[string]string, expected *string) map[string]any {
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}
	slices.Sort(names)
	sealed := make([]map[string]string, 0, len(names))
	for _, name := range names {
		sum := sha256.Sum256([]byte(bindings[name]))
		sealed = append(sealed, map[string]string{"ref": name, "sha256": hex.EncodeToString(sum[:])})
	}
	return map[string]any{"document_digest": digest, "expected_digest": expected, "secret_bindings": sealed}
}
