package configuration

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/budgetcalendar"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routeidentity"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routing"
)

// validateDocument refuses an artifact that is invalid anywhere, and pins the
// plugin profiles its plugin providers name. It returns the plugins this
// installation can't use yet, by digest, with why: they block the plan.
func (s *Server) validateDocument(ctx context.Context, q access.Queryer, doc *Document) (map[string]string, error) {
	if doc.Code != nil {
		if err := doc.Code.Validate(); err != nil {
			return nil, err
		}
	}
	if doc.APIVersion != APIVersion && doc.APIVersion != LegacyAPIVersion {
		return nil, access.Fail(422, "unsupported_api_version", "The artifact declares an unsupported api_version.")
	}
	if err := normalizeGuardrailVersion(doc); err != nil {
		return nil, err
	}
	normalizeDocument(doc)
	if err := s.validateAgentPublications(doc); err != nil {
		return nil, err
	}
	if err := s.validateMCPServers(doc); err != nil {
		return nil, err
	}
	if err := validateAndNormalizeRetrievalBindings(doc); err != nil {
		return nil, err
	}
	if err := s.validateAndNormalizeSinks(doc); err != nil {
		return nil, err
	}
	if err := validateGuardrails(doc); err != nil {
		return nil, err
	}
	if err := validateGuardrailPolicies(doc); err != nil {
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
	if err := validateAndNormalizeWorkloadEntries(doc); err != nil {
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
	if err := doc.InstallationBudget.ValidateAndNormalize(); err != nil {
		return nil, err
	}
	if len(doc.Projects) > maxProjects {
		return nil, access.Invalid("projects", "Declare at most "+strconv.Itoa(maxProjects)+" projects.")
	}
	seen := map[string]bool{}
	for i, p := range doc.Projects {
		if err := p.Budget.ValidateAndNormalize(); err != nil {
			return nil, err
		}
		if err := p.AttributionBudgets.ValidateAndNormalize(); err != nil {
			return nil, err
		}
		if p.LimitTemplates != nil {
			if err := p.LimitTemplates.ValidateAndNormalize(); err != nil {
				return nil, err
			}
		}
		if p.EndUserLimitTemplate != nil && !routeidentity.ValidSlug(*p.EndUserLimitTemplate) {
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
			if err := p.EndUserDefaults.ValidateAndNormalize(); err != nil {
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
	preparedCode, err := s.prepareCode(ctx, q, doc.Code)
	if err != nil {
		return nil, err
	}
	doc.Code = preparedCode

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
		if err := validateBillingImport(prefix, &doc.Providers[i]); err != nil {
			return nil, err
		}
		if p.BillingImport != nil {
			if refs[p.BillingImport.CredentialRef] {
				return nil, access.Invalid(prefix+".billing_import.credential_ref", "Credential references must be distinct.")
			}
			refs[p.BillingImport.CredentialRef] = true
		}
		if refusal != "" {
			unavailable[p.Configuration.ProfileRevision] = refusal
		} else if err := p.Configuration.ValidateAndNormalize(s.Egress); err != nil {
			return nil, err
		} else {
			doc.Providers[i].Configuration = p.Configuration
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
				if !routeidentity.ValidSlug(route) {
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
		if _, err := routing.DecodeFidelity(rt.Fidelity); err != nil {
			return nil, access.Invalid(prefix+".fidelity", err.Error())
		}
		fidelity, _ := routing.DecodeFidelity(rt.Fidelity)
		if err := (routing.Route{Fidelity: fidelity, Behavior: rt.behavior()}).ValidateCaching(); err != nil {
			return nil, access.Invalid(prefix+".cache", err.Error())
		}
		if err := routes.ValidateFidelityPolicy(rt.Fidelity, rt.ContentPolicy); err != nil {
			return nil, err
		}
		if routeSeen[rt.Slug] {
			return nil, access.Invalid(prefix+".slug", "Route slugs must be unique.")
		}
		routeSeen[rt.Slug] = true
		if !routeidentity.ValidSlug(rt.Slug) {
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
		if err := rt.validatePassthrough(); err != nil {
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
	if err := s.validateCharges(ctx, q, doc, docProviders); err != nil {
		return nil, err
	}
	return unavailable, nil
}

func validateBindings(doc *Document, bindings bindingSet) error {
	refs, grants := map[string]bool{}, map[string]bool{}
	guardrailRefs, guardrailNetworkRefs := map[string]bool{}, map[string]bool{}
	for _, entry := range doc.Guardrails {
		if strings.HasPrefix(entry.Type, "builtin.") {
			continue
		}
		var config guardrails.ExternalConfig
		if json.Unmarshal(entry.Configuration, &config) != nil {
			return access.Invalid("guardrails", "Invalid guardrail configuration.")
		}
		if config.CredentialRef != "" {
			refs[config.CredentialRef], guardrailRefs[config.CredentialRef] = true, true
		}
		if config.Network != nil && config.Network.CredentialID != "" {
			name := config.Network.CredentialID
			refs[name], guardrailRefs[name], guardrailNetworkRefs[name] = true, true, true
		}
	}
	bearerRefs := map[string]bool{}
	for _, e := range doc.MCPServers {
		if e.CredentialRef != nil {
			refs[*e.CredentialRef] = true
			bearerRefs[*e.CredentialRef] = true
		}
	}
	for _, e := range doc.AgentPublications {
		for _, ref := range []*string{e.CredentialRef, e.PublishedCredentialRef} {
			if ref != nil {
				refs[*ref], bearerRefs[*ref] = true, true
			}
		}
	}
	sinkRefs := map[string]bool{}
	for _, entry := range doc.Sinks {
		if entry.CredentialRef != nil {
			refs[*entry.CredentialRef] = true
			sinkRefs[*entry.CredentialRef] = true
		}
	}
	billingRefs := map[string]string{}
	for _, p := range doc.Providers {
		if p.NetworkCredentialRef != nil {
			refs[*p.NetworkCredentialRef] = true
		}
		if p.BillingImport != nil {
			refs[p.BillingImport.CredentialRef] = true
			source, err := billing.SourceFor(p.Configuration.Kind)
			if err != nil {
				return err
			}
			billingRefs[p.BillingImport.CredentialRef] = source
		}
		for _, slot := range p.Slots {
			if slot.CredentialRef != nil {
				refs[*slot.CredentialRef] = true
				grants[*slot.CredentialRef] = p.Configuration.Grant()
			}
		}
	}
	for name, secret := range bindings {
		if guardrailRefs[name] {
			if secret.reference != nil {
				return access.Invalid("external_credential_bindings", "Guardrail credentials require sealed secret bindings.")
			}
			if err := guardrails.ValidateCredential([]byte(secret.secret)); err != nil || len(secret.secret) > maxSecretBytes {
				return access.Invalid("secret_bindings."+name, "Supply a bounded guardrail credential object.")
			}
			if guardrailNetworkRefs[name] && egress.ValidateConnectionSecret([]byte(secret.secret)) != nil {
				return access.Invalid("secret_bindings."+name, "Supply a valid network credential object.")
			}
			continue
		}
		if bearerRefs[name] {
			if secret.reference != nil {
				return access.Invalid("external_credential_bindings", "Static bearer material uses sealed bindings.")
			}
			encoded, _ := json.Marshal(secret.secret)
			if _, err := mcpservers.Credential(encoded); err != nil {
				return err
			}
			continue
		}
		if sinkRefs[name] {
			if secret.reference != nil || len(secret.secret) < 1 || len(secret.secret) > 4096 {
				return access.Invalid("secret_bindings", "Sink signing credentials require 1–4096 sealed bytes.")
			}
			continue
		}
		if source := billingRefs[name]; source != "" {
			if secret.reference != nil {
				return access.Invalid("external_credential_bindings", "Billing credentials require sealed secret bindings.")
			}
			if err := billing.ValidateCredential(source, secret.secret); err != nil {
				return err
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
