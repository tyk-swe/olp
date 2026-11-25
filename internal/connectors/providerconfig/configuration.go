package providerconfig

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

type Identity[T string | *string] struct {
	CredentialSource string                   `json:"credential_source,omitempty"`
	ProfileID        string                   `json:"profile_id,omitempty"`
	ProfileRevision  string                   `json:"profile_revision,omitempty"`
	Kind             connectors.Kind          `json:"kind"`
	AuthMode         connectors.AuthMode      `json:"auth_mode"`
	Endpoint         T                        `json:"endpoint"`
	CloudRegion      T                        `json:"cloud_region"`
	CloudProject     T                        `json:"cloud_project"`
	BatchStorage     *connectors.BatchStorage `json:"batch_storage,omitempty"`
	Deployment       T                        `json:"deployment"`
	APIVersion       T                        `json:"api_version"`
}

type Options struct {
	TuningDatasetURIPrefixes []string                         `json:"tuning_dataset_uri_prefixes,omitempty"`
	DocumentURIPrefixes      []string                         `json:"document_uri_prefixes,omitempty"`
	Network                  *egress.ConnectionOptions        `json:"network,omitempty"`
	SemanticHeaders          map[string]string                `json:"semantic_headers,omitempty"`
	QuerySettings            map[string]string                `json:"query_settings,omitempty"`
	OperationDefaults        map[string]connectors.DefaultSet `json:"operation_defaults,omitempty"`
	Bindings                 map[string]connectors.Binding    `json:"bindings,omitempty"`
	CredentialHeaders        []string                         `json:"credential_headers"`
	Models                   map[string]json.RawMessage       `json:"models"`
	ParameterDefaults        map[string]json.RawMessage       `json:"parameter_defaults"`
	VendorID                 string                           `json:"vendor_id"`
	PluginOptions            map[string]string                `json:"plugin_options,omitempty"`
}

func (c Identity[T]) StringIdentity() Identity[string] {
	return Identity[string]{
		CredentialSource: c.CredentialSource,
		ProfileID:        c.ProfileID,
		ProfileRevision:  c.ProfileRevision,
		Kind:             c.Kind,
		AuthMode:         c.AuthMode,
		Endpoint:         Value(c.Endpoint),
		CloudRegion:      Value(c.CloudRegion),
		CloudProject:     Value(c.CloudProject),
		BatchStorage:     ptr.Clone(c.BatchStorage),
		Deployment:       Value(c.Deployment),
		APIVersion:       Value(c.APIVersion),
	}
}

func (c Identity[T]) Connector(options Options, plugin *connectors.PluginProfile, credentialSource, principal string) connectors.Config {
	return connectors.Config{
		Network: options.Network, Plugin: plugin, ProfileID: c.ProfileID, ProfileRevision: c.ProfileRevision,
		SemanticHeaders: options.SemanticHeaders, QuerySettings: options.QuerySettings,
		OperationDefaults: options.OperationDefaults, Bindings: options.Bindings,
		PluginOptions: options.PluginOptions, Kind: c.Kind, AuthMode: c.AuthMode,
		Endpoint: Value(c.Endpoint), CloudRegion: Value(c.CloudRegion), CloudProject: Value(c.CloudProject),
		BatchStorage: c.BatchStorage, TuningDatasetURIPrefixes: options.TuningDatasetURIPrefixes,
		DocumentURIPrefixes: options.DocumentURIPrefixes, Deployment: Value(c.Deployment), APIVersion: Value(c.APIVersion),
		VendorID: options.VendorID, CredentialHeaders: options.CredentialHeaders, Models: options.Models,
		ObservedPrincipal: principal, CredentialSource: credentialSource,
	}
}

func (o Options) Clone() Options {
	return Options{
		TuningDatasetURIPrefixes: slices.Clone(o.TuningDatasetURIPrefixes),
		DocumentURIPrefixes:      slices.Clone(o.DocumentURIPrefixes),
		Network:                  connectors.CloneNetwork(o.Network),
		SemanticHeaders:          maps.Clone(o.SemanticHeaders),
		QuerySettings:            maps.Clone(o.QuerySettings),
		OperationDefaults:        connectors.CloneDefaults(o.OperationDefaults),
		Bindings:                 connectors.CloneBindings(o.Bindings),
		CredentialHeaders:        slices.Clone(o.CredentialHeaders),
		Models:                   connectors.CloneRawMap(o.Models),
		ParameterDefaults:        connectors.CloneRawMap(o.ParameterDefaults),
		VendorID:                 o.VendorID,
		PluginOptions:            maps.Clone(o.PluginOptions),
	}
}

func Value[T string | *string](value T) string {
	switch v := any(value).(type) {
	case string:
		return v
	case *string:
		if v != nil {
			return *v
		}
	}
	return ""
}
