package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routing"
)

func resolveCodePaidPolicy(ctx context.Context, q access.Queryer, policy CodeRoutePolicy, project string, bindings CodeBindings) (CodeRoutePolicy, error) {
	direct, err := resolveCodePaidTargets(ctx, q, policy.PaidTargets, project, bindings)
	if err != nil {
		return CodeRoutePolicy{}, err
	}
	fallback, fallbackError := resolveCodePaidTargets(ctx, q, policy.PaidFallbackTargets, project, bindings)
	if fallbackError != nil {
		return CodeRoutePolicy{}, fallbackError
	}
	return codePolicyPaid(policy, direct, fallback), nil
}
func resolveCodePaidTargets(ctx context.Context, q access.Queryer, references map[string]CodePaidTarget, project string, bindings CodeBindings) (map[string]CodePaidTarget, error) {
	type target struct {
		Model string
		Value CodePaidTarget
	}
	targets, err := mapCodeResult(slices.Collect(maps.Keys(references)), func(model string) (target, error) {
		ref := references[model]
		credential, err := codeBound(ref.CredentialRef, bindings.Credentials)
		if err != nil {
			return target{}, err
		}
		var provider, slot string
		err = q.QueryRow(ctx, `SELECT p.id::text,s.id::text FROM olp.providers p JOIN olp.provider_revisions v ON v.id=p.active_revision_id JOIN olp.provider_slots s ON s.provider_id=p.id JOIN olp.provider_credentials c ON c.id=s.credential_id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND s.name=$3 AND s.credential_id=$4 AND c.revoked_at IS NULL AND s.enabled AND p.state='active' AND EXISTS(SELECT 1 FROM jsonb_array_elements(v.slots) x WHERE x->>'id'=s.id::text AND x->>'credential_id'=c.id::text AND (x->>'enabled')::boolean)`, ref.Provider, project, ref.Slot, credential).Scan(&provider, &slot)
		if errors.Is(err, pgx.ErrNoRows) {
			return target{}, access.Invalid("code.paid_targets", "Bind the current credential of an enabled published destination provider slot.")
		}
		if err != nil {
			return target{}, err
		}
		return target{Model: model, Value: CodePaidTarget{Provider: provider, Slot: slot}}, nil
	})
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, CodePaidTarget) bool) {
		for _, target := range targets {
			if !yield(target.Model, target.Value) {
				return
			}
		}
	}), nil
}
func codePolicyPaid(policy CodeRoutePolicy, targets, fallback map[string]CodePaidTarget) CodeRoutePolicy {
	return CodeRoutePolicy{ChatToAntigravity: policy.ChatToAntigravity, Pool: policy.Pool, Models: policy.Models, Enabled: policy.Enabled, ModelPools: maps.Collect(maps.All(policy.ModelPools)), InitialRecoveryAttempts: policy.InitialRecoveryAttempts, ModelRewriting: policy.ModelRewriting, Aliases: maps.Collect(maps.All(policy.Aliases)), CodexCLIIdentity: policy.CodexCLIIdentity, ChatToCodex: policy.ChatToCodex, MessagesToCodex: policy.MessagesToCodex, Tools: policy.Tools, MaxBodyBytes: policy.MaxBodyBytes, PaidParticipation: policy.PaidParticipation, PaidTargets: targets, PaidFallbackTargets: fallback}
}

func resolveCodePaid(ctx context.Context, q access.Queryer, publication *CodePublication, policy CodeRoutePolicy, project string, bindings CodeBindings) (map[string]routing.Provider, error) {
	entries, err := mapCodeResult(slices.Collect(maps.Values(publication.PaidConnections)), func(connection CodePaidConnection) (routing.Provider, error) {
		entry := connection.Provider
		var current routing.ProviderRevision
		if err := q.QueryRow(ctx, `SELECT p.id::text,v.id::text,p.name,p.state,p.project_id::text,v.configuration,v.models,v.slots FROM olp.providers p JOIN olp.provider_revisions v ON v.id=p.active_revision_id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND p.state='active'`, entry.Name, project).Scan(&current.ID, &current.RevisionID, &current.Name, &current.State, &current.ProjectID, &current.Configuration, &current.Models, &current.Slots); err != nil {
			return routing.Provider{}, access.Invalid("code.paid_connections", "Prepare the published destination metered provider first.")
		}
		var currentModels []routing.RevisionModel
		if err := json.Unmarshal(current.Models, &currentModels); err != nil {
			return routing.Provider{}, err
		}
		var currentSlots []routing.RevisionSlot
		if err := json.Unmarshal(current.Slots, &currentSlots); err != nil {
			return routing.Provider{}, err
		}
		models, err := mapCodeResult(entry.Models, func(model ModelEntry) (routing.RevisionModel, error) {
			index := slices.IndexFunc(currentModels, func(m routing.RevisionModel) bool { return m.UpstreamModel == model.UpstreamModel })
			if index < 0 || !model.Enabled {
				return routing.RevisionModel{}, access.Invalid("code.paid_connections.models", "Frozen models must remain enabled in the destination publication.")
			}
			destination := currentModels[index]
			capabilities, err := mapCodeResult(model.Capabilities, func(capability CapabilityEntry) (routing.RevisionCapability, error) {
				index := slices.IndexFunc(destination.Capabilities, func(c routing.RevisionCapability) bool {
					return c.Operation == capability.Operation && c.Surface == capability.Surface && c.Mode == capability.Mode && c.Source == "certified"
				})
				if index < 0 {
					return routing.RevisionCapability{}, access.Invalid("code.paid_connections.models", "Certify each frozen model capability on the destination provider first.")
				}
				return destination.Capabilities[index], nil
			})
			return routing.RevisionModel{ID: destination.ID, UpstreamModel: model.UpstreamModel, DisplayName: model.DisplayName, Capabilities: capabilities}, err
		})
		if err != nil {
			return routing.Provider{}, err
		}
		slots, err := mapCodeResult(entry.Slots, func(slot SlotEntry) (routing.RevisionSlot, error) {
			index := slices.IndexFunc(currentSlots, func(s routing.RevisionSlot) bool { return s.Name == slot.Name })
			if index < 0 {
				return routing.RevisionSlot{}, access.Invalid("code.paid_connections.slots", "Every frozen slot needs its published destination slot.")
			}
			currentSlot := currentSlots[index]
			credential, err := func() (*string, error) {
				if slot.CredentialRef == nil {
					return nil, nil
				}
				id, err := codeBound(*slot.CredentialRef, bindings.Credentials)
				if err != nil {
					return nil, err
				}
				var allowed bool
				if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.provider_slots s JOIN olp.provider_credentials c ON c.id=s.credential_id WHERE s.id=$1 AND s.provider_id=$2 AND c.id=$3 AND c.revoked_at IS NULL AND ($4=false OR s.enabled))`, currentSlot.ID, current.ID, id, slot.Enabled).Scan(&allowed); err != nil {
					return nil, err
				}
				if !allowed || currentSlot.CredentialID == nil || *currentSlot.CredentialID != id || slot.Enabled && !currentSlot.Enabled {
					return nil, access.Invalid("code_bindings.credentials", "Bind the enabled destination slot's current published credential.")
				}
				return &id, nil
			}()
			if err != nil {
				return routing.RevisionSlot{}, err
			}
			keys, err := mapCodeResult(connection.SlotKeyRefs[slot.Name], func(ref string) (string, error) { return resolveCodeKey(ctx, q, ref, project, nil, bindings.APIKeys) })
			if err != nil {
				return routing.RevisionSlot{}, err
			}
			version := func() *int {
				if credential == nil {
					return nil
				}
				return currentSlot.CredentialVersion
			}()
			return routing.RevisionSlot{Default: slot.IsDefault, Slot: routing.Slot{ID: currentSlot.ID, Name: slot.Name, Enabled: slot.Enabled, Priority: slot.Priority, Weight: slot.Weight, CredentialID: credential, CredentialVersion: version, AllowedModels: slot.Restrictions.AllowedModels, AllowedRoutes: slot.Restrictions.AllowedRoutes, AllowedAPIKeys: keys, MaxConcurrency: slot.Limits.MaxConcurrency, RequestsPerMinute: slot.Limits.RequestsPerMinute, TokensPerMinute: slot.Limits.TokensPerMinute, Supply: slot.Limits.Supply}}, nil
		})
		if err != nil {
			return routing.Provider{}, err
		}
		var currentConfiguration providers.Configuration
		if err := json.Unmarshal(current.Configuration, &currentConfiguration); err != nil {
			return routing.Provider{}, err
		}
		if entry.Configuration.Kind != currentConfiguration.Kind || entry.Configuration.AuthMode != currentConfiguration.AuthMode || entry.Configuration.ProfileID != currentConfiguration.ProfileID {
			return routing.Provider{}, access.Invalid("code.paid_connections", "Frozen paid providers must retain the destination provider kind, authentication and profile.")
		}
		cfg, err := codeNetworkConfiguration(entry.Configuration, currentConfiguration, entry.NetworkCredentialRef, entry.Name)
		if err != nil {
			return routing.Provider{}, err
		}
		configurationJSON, err := json.Marshal(cfg)
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
	})
	if err != nil {
		return nil, err
	}
	providers := maps.Collect(func(yield func(string, routing.Provider) bool) {
		for _, provider := range entries {
			if !yield(provider.ID, provider) {
				return
			}
		}
	})
	if err := routes.ValidateCodePaidProviders(policy.paidRoute(project), providers); err != nil {
		return nil, err
	}
	for _, target := range policy.allPaidTargets() {
		provider := providers[target.Provider]
		slot := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool { return slot.ID == target.Slot })
		if slot < 0 {
			return nil, access.Invalid("code.paid_connections.slots", "Resolve every frozen paid target slot before checking its destination limits.")
		}
		if err := validateCodePaidLimits(ctx, q, provider, provider.Slots[slot], project); err != nil {
			return nil, err
		}
	}
	if len(providers) != len(publication.PaidConnections) {
		return nil, access.Invalid("code.paid_connections", "Frozen paid provider identities must be distinct.")
	}
	return providers, nil
}

func validateCodePaidLimits(ctx context.Context, q access.Queryer, provider routing.Provider, slot routing.Slot, project string) error {
	var configurationJSON, slotJSON []byte
	if err := q.QueryRow(ctx, `SELECT p.configuration,s.limits FROM olp.providers p JOIN olp.provider_slots s ON s.provider_id=p.id WHERE p.id=$1 AND s.id=$2 AND p.project_id=$3`, provider.ID, slot.ID, project).Scan(&configurationJSON, &slotJSON); err != nil {
		return err
	}
	var configuration routing.Configuration
	if err := json.Unmarshal(configurationJSON, &configuration); err != nil {
		return err
	}
	var liveSlot routing.Slot
	if err := json.Unmarshal(slotJSON, &liveSlot); err != nil {
		return err
	}
	if !routing.CodePaidLimitsMatch(configuration.Options.Limits, provider.Limits, liveSlot, slot) {
		return access.Invalid("code.paid_connections.limits", "Frozen paid provider and slot limits must match the destination's live serving policy.")
	}
	return nil
}
