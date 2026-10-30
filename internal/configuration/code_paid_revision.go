package configuration

import (
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routing"
)

func codePaidRevision(provider routing.Provider, labels []routing.RevisionModel) ([]byte, []byte, []byte, error) {
	if provider.PendingCommitment != nil {
		return nil, nil, nil, access.Invalid("code.paid_connections", "Publish the pending monetary commitment before exporting this paid provider.")
	}
	limits := func() *providers.Limits {
		if provider.Limits == nil {
			return nil
		}
		value := provider.Limits
		return &providers.Limits{MaxConcurrency: value.MaxConcurrency, RequestsPerMinute: value.RequestsPerMinute, TokensPerMinute: value.TokensPerMinute, Supply: value.Supply}
	}()
	configuration, err := json.Marshal(providers.Configuration{CredentialSource: provider.CredentialSource, ProfileID: provider.ProfileID, ProfileRevision: provider.ProfileRevision, Kind: provider.Kind, AuthMode: provider.AuthMode, Endpoint: codeString(provider.Endpoint), CloudRegion: codeString(provider.CloudRegion), CloudProject: codeString(provider.CloudProject), BatchStorage: provider.BatchStorage, Deployment: codeString(provider.Deployment), APIVersion: codeString(provider.APIVersion), Options: providers.Options{TuningDatasetURIPrefixes: provider.TuningDatasetURIPrefixes, DocumentURIPrefixes: provider.DocumentURIPrefixes, Network: provider.Network, SemanticHeaders: provider.SemanticHeaders, QuerySettings: provider.QuerySettings, OperationDefaults: provider.OperationDefaults, Bindings: provider.Bindings, CredentialHeaders: provider.CredentialHeaders, Limits: limits, Models: provider.Models, ParameterDefaults: provider.ParameterDefaults, VendorID: codeString(provider.VendorID), Commitment: provider.Commitment, PluginOptions: provider.PluginOptions, HealthProbe: provider.HealthProbe}})
	if err != nil {
		return nil, nil, nil, err
	}
	models, err := json.Marshal(mapCode(slices.Compact(slices.Sorted(func(yield func(string) bool) {
		for _, capability := range provider.Capabilities {
			if !yield(capability.Model) {
				return
			}
		}
	})), func(model string) routing.RevisionModel {
		label := func() string {
			index := slices.IndexFunc(labels, func(m routing.RevisionModel) bool { return m.UpstreamModel == model })
			if index < 0 {
				return model
			}
			return labels[index].DisplayName
		}()
		return routing.RevisionModel{UpstreamModel: model, DisplayName: label, Capabilities: slices.Collect(func(yield func(routing.RevisionCapability) bool) {
			for _, capability := range provider.Capabilities {
				if capability.Model == model && !yield(routing.RevisionCapability{Operation: capability.Operation, Surface: capability.Surface, Mode: capability.Mode, Source: "certified"}) {
					return
				}
			}
		})}
	}))
	if err != nil {
		return nil, nil, nil, err
	}
	slots, err := json.Marshal(mapCode(provider.Slots, func(slot routing.Slot) routing.RevisionSlot {
		return routing.RevisionSlot{Slot: slot, Default: slot.ID == provider.DefaultSlotID}
	}))
	return configuration, models, slots, err
}
func codeString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
