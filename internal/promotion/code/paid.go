package code

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/providers/codeaccounts"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routing"
)

type PaidConnection struct {
	Provider    provider.Entry
	SlotKeyRefs map[string][]string
}

func parsePaidConnection(e contract.ConfigurationCodePaidConnection, names promotion.Names, deps provider.Deps) (PaidConnection, error) {
	entry, err := provider.Parse(e.Provider, names, deps)
	if err != nil {
		return PaidConnection{}, err
	}
	return PaidConnection{Provider: entry, SlotKeyRefs: maps.Clone(e.SlotKeyRefs)}, nil
}

func (c PaidConnection) slotRef(slot provider.Slot) (string, bool) {
	if _, unbound := slot.Credential.(provider.Unbound); unbound {
		return "", false
	}
	return string(provider.SlotRef(c.Provider.Name, slot.Name)), true
}

func (c PaidConnection) credentialRefs() []string {
	return slices.Collect(func(yield func(string) bool) {
		for _, slot := range c.Provider.Slots {
			if ref, referenced := c.slotRef(slot); referenced && !yield(ref) {
				return
			}
		}
	})
}

func (c PaidConnection) keyRefs() []string {
	return slices.Concat(promotion.Map(slices.Sorted(maps.Keys(c.SlotKeyRefs)), func(slot string) []string { return c.SlotKeyRefs[slot] })...)
}

func (r Route) validPaid() error {
	publication := r.Published
	for _, name := range slices.Sorted(maps.Keys(publication.PaidConnections)) {
		if err := publication.PaidConnections[name].valid(name, r.Project, publication.Policy); err != nil {
			return err
		}
	}
	for _, target := range publication.Policy.paidTargets() {
		connection, frozen := publication.PaidConnections[target.Provider]
		if !frozen {
			return access.Invalid("code.paid_connections", "Freeze every published paid provider.")
		}
		if !slices.ContainsFunc(connection.Provider.Slots, func(slot provider.Slot) bool {
			ref, referenced := connection.slotRef(slot)
			return slot.Name == target.Slot && referenced && ref == target.CredentialRef && slot.Enabled
		}) {
			return access.Invalid("code.paid_connections", "Freeze the enabled target slot with its explicit credential reference.")
		}
	}
	return nil
}

func (c PaidConnection) valid(name, project string, policy Policy) error {
	entry := c.Provider
	if name != entry.Name || entry.Project == nil || !strings.EqualFold(*entry.Project, project) || entry.Billing != nil || !policy.targets(name) {
		return access.Invalid("code.paid_connections", "Frozen providers must name the published paid targets in the same project.")
	}
	for _, slot := range slices.Sorted(maps.Keys(c.SlotKeyRefs)) {
		if !slices.ContainsFunc(entry.Slots, func(s provider.Slot) bool { return s.Name == slot }) {
			return access.Invalid("code.paid_connections.slot_key_refs", "Key restrictions must name a frozen slot.")
		}
		if slices.ContainsFunc(c.SlotKeyRefs[slot], func(ref string) bool { return access.ValidText("code", ref, 300) != nil }) {
			return access.Invalid("code.paid_connections.slot_key_refs", "Name explicit destination key references.")
		}
	}
	return nil
}

func (c PaidConnection) render() (contract.ConfigurationCodePaidConnection, error) {
	entries, err := provider.Render([]provider.Entry{c.Provider})
	if err != nil {
		return contract.ConfigurationCodePaidConnection{}, err
	}
	refs := promotion.MapValues(c.SlotKeyRefs, func(refs []string) []string { return promotion.NonNil(slices.Clone(refs)) })
	return contract.ConfigurationCodePaidConnection{Provider: entries[0], SlotKeyRefs: refs}, nil
}

func resolvePaid(ctx context.Context, q access.Queryer, connections map[string]PaidConnection, policy routes.CodeRouteInput, values bindings, deps provider.Deps) (map[string]routing.Provider, error) {
	frozen, err := promotion.TryMap(slices.Sorted(maps.Keys(connections)), func(name string) (routing.Provider, error) {
		return connections[name].resolve(ctx, q, policy.ProjectID, values, deps)
	})
	if err != nil {
		return nil, err
	}
	byProvider := promotion.Index(frozen, func(p routing.Provider) string { return p.ID })
	route := paidRoute(policy)
	if err := routes.ValidateCodePaidProviders(route, byProvider); err != nil {
		return nil, err
	}
	for _, targets := range []map[string]codemode.PaidTarget{route.PaidTargets, route.PaidFallbackTargets} {
		for _, model := range slices.Sorted(maps.Keys(targets)) {
			target := targets[model]
			slots := byProvider[target.ProviderID].Slots
			at := slices.IndexFunc(slots, func(slot routing.Slot) bool { return slot.ID == target.SlotID })
			if at < 0 {
				return nil, access.Invalid("code.paid_connections.slots", "Resolve every frozen paid target slot before checking its destination limits.")
			}
			if err := validLimits(ctx, q, byProvider[target.ProviderID], slots[at], policy.ProjectID); err != nil {
				return nil, err
			}
		}
	}
	if len(byProvider) != len(connections) {
		return nil, access.Invalid("code.paid_connections", "Frozen paid provider identities must be distinct.")
	}
	return byProvider, nil
}

func (c PaidConnection) resolve(ctx context.Context, q access.Queryer, project string, values bindings, deps provider.Deps) (routing.Provider, error) {
	entry := c.Provider
	active, err := providers.ReadActiveRevision(ctx, q, entry.Name, project)
	if err != nil {
		return routing.Provider{}, err
	}
	if active == nil {
		return routing.Provider{}, access.Invalid("code.paid_connections", "Prepare the published destination metered provider first.")
	}
	current := *active
	var currentModels []routing.RevisionModel
	if err := json.Unmarshal(current.Models, &currentModels); err != nil {
		return routing.Provider{}, err
	}
	var currentSlots []routing.RevisionSlot
	if err := json.Unmarshal(current.Slots, &currentSlots); err != nil {
		return routing.Provider{}, err
	}
	var currentConfiguration connection.Configuration
	if err := json.Unmarshal(current.Configuration, &currentConfiguration); err != nil {
		return routing.Provider{}, err
	}
	models, err := promotion.TryMapIndex(entry.Models, func(_ int, model providers.DraftModel) (routing.RevisionModel, error) {
		return frozenModel(model, currentModels)
	})
	if err != nil {
		return routing.Provider{}, err
	}
	slots, err := promotion.TryMapIndex(entry.Slots, func(_ int, slot provider.Slot) (routing.RevisionSlot, error) {
		return c.frozenSlot(ctx, q, slot, current.ID, currentSlots, project, values)
	})
	if err != nil {
		return routing.Provider{}, err
	}
	prepared, err := preparedPaid(ctx, q, entry.Configuration, deps)
	if err != nil {
		return routing.Provider{}, err
	}
	if prepared.Kind != currentConfiguration.Kind || prepared.AuthMode != currentConfiguration.AuthMode || prepared.ProfileID != currentConfiguration.ProfileID {
		return routing.Provider{}, access.Invalid("code.paid_connections", "Frozen paid providers must retain the destination provider kind, authentication and profile.")
	}
	configuration, err := networked(prepared, currentConfiguration, entry.Network)
	if err != nil {
		return routing.Provider{}, err
	}
	return decoded(current, configuration, models, slots)
}

func paidRoute(policy routes.CodeRouteInput) codemode.Route {
	return codemode.Route{Models: policy.Models, ProjectID: policy.ProjectID, PaidParticipation: policy.PaidParticipation, PaidTargets: policy.PaidTargets, PaidFallbackTargets: policy.PaidFallbackTargets, InitialRecoveryAttempts: policy.InitialRecoveryAttempts}
}

func preparedPaid(ctx context.Context, q access.Queryer, configuration connection.Configuration, deps provider.Deps) (connection.Configuration, error) {
	prepared, err := connection.Prepare(ctx, deps.Catalogue.ConnectionPolicy(q), configuration)
	if _, refused := provider.Refusal(err); refused {
		return configuration, nil
	}
	if err != nil {
		return connection.Configuration{}, err
	}
	return prepared.Document(), nil
}

func decoded(current routing.ProviderRevision, configuration connection.Configuration, models []routing.RevisionModel, slots []routing.RevisionSlot) (routing.Provider, error) {
	configurationJSON, err := json.Marshal(configuration)
	if err != nil {
		return routing.Provider{}, err
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		return routing.Provider{}, err
	}
	slotsJSON, err := json.Marshal(slots)
	if err != nil {
		return routing.Provider{}, err
	}
	return routing.DecodeProviderRevision(routing.ProviderRevision{ID: current.ID, Name: current.Name, State: current.State, RevisionID: current.RevisionID, ProjectID: current.ProjectID, Configuration: configurationJSON, Models: modelsJSON, Slots: slotsJSON})
}

func frozenModel(model providers.DraftModel, current []routing.RevisionModel) (routing.RevisionModel, error) {
	at := slices.IndexFunc(current, func(m routing.RevisionModel) bool { return m.UpstreamModel == model.UpstreamModel })
	if at < 0 || !model.Enabled {
		return routing.RevisionModel{}, access.Invalid("code.paid_connections.models", "Frozen models must remain enabled in the destination publication.")
	}
	destination := current[at]
	capabilities, err := promotion.TryMapIndex(model.Capabilities, func(_ int, capability connection.Capability) (routing.RevisionCapability, error) {
		found := slices.IndexFunc(destination.Capabilities, func(c routing.RevisionCapability) bool {
			return c.Operation == capability.Operation && c.Surface == capability.Surface && c.Mode == capability.Mode && c.Source == "certified"
		})
		if found < 0 {
			return routing.RevisionCapability{}, access.Invalid("code.paid_connections.models", "Certify each frozen model capability on the destination provider first.")
		}
		return destination.Capabilities[found], nil
	})
	return routing.RevisionModel{ID: destination.ID, UpstreamModel: model.UpstreamModel, DisplayName: model.DisplayName, Capabilities: capabilities}, err
}

func (c PaidConnection) frozenSlot(ctx context.Context, q access.Queryer, slot provider.Slot, providerID string, current []routing.RevisionSlot, project string, values bindings) (routing.RevisionSlot, error) {
	at := slices.IndexFunc(current, func(s routing.RevisionSlot) bool { return s.Name == slot.Name })
	if at < 0 {
		return routing.RevisionSlot{}, access.Invalid("code.paid_connections.slots", "Every frozen slot needs its published destination slot.")
	}
	live := current[at]
	credential, err := c.slotCredential(ctx, q, slot, live, providerID, values.credentials)
	if err != nil {
		return routing.RevisionSlot{}, err
	}
	keys, err := keysOf(ctx, q, c.SlotKeyRefs[slot.Name], project, nil, values.keys)
	if err != nil {
		return routing.RevisionSlot{}, err
	}
	return routing.RevisionSlot{Default: slot.Default, Slot: routing.Slot{
		ID: live.ID, Name: slot.Name, Enabled: slot.Enabled, Priority: slot.Priority, Weight: slot.Weight, CredentialID: credential, CredentialVersion: versionOf(credential, live),
		AllowedModels: slot.AllowedModels, AllowedRoutes: slot.AllowedRoutes, AllowedAPIKeys: keys,
		MaxConcurrency: slot.Limits.MaxConcurrency, RequestsPerMinute: slot.Limits.RequestsPerMinute, TokensPerMinute: slot.Limits.TokensPerMinute, Supply: slot.Limits.Supply,
	}}, nil
}

func (c PaidConnection) slotCredential(ctx context.Context, q access.Queryer, slot provider.Slot, live routing.RevisionSlot, providerID string, credentials map[string]string) (*string, error) {
	ref, referenced := c.slotRef(slot)
	if !referenced {
		return nil, nil
	}
	id, err := bound(ref, credentials)
	if err != nil {
		return nil, err
	}
	allowed, err := codeaccounts.SlotCredentialCurrent(ctx, q, live.ID, providerID, id, slot.Enabled)
	if err != nil {
		return nil, err
	}
	if !allowed || live.CredentialID == nil || *live.CredentialID != id || slot.Enabled && !live.Enabled {
		return nil, access.Invalid("code_bindings.credentials", "Bind the enabled destination slot's current published credential.")
	}
	return &id, nil
}

func versionOf(credential *string, live routing.RevisionSlot) *int {
	if credential == nil {
		return nil
	}
	return live.CredentialVersion
}
