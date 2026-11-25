package connectors

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

func (config Config) Clone() Config {
	out := config
	out.Plugin = ptr.Clone(config.Plugin)
	out.TuningDatasetURIPrefixes = slices.Clone(config.TuningDatasetURIPrefixes)
	out.DocumentURIPrefixes = slices.Clone(config.DocumentURIPrefixes)
	out.BatchStorage = ptr.Clone(config.BatchStorage)
	out.SemanticHeaders = maps.Clone(config.SemanticHeaders)
	out.QuerySettings = maps.Clone(config.QuerySettings)
	out.PluginOptions = maps.Clone(config.PluginOptions)
	out.CredentialHeaders = slices.Clone(config.CredentialHeaders)
	out.Models = CloneRawMap(config.Models)
	out.OperationDefaults = CloneDefaults(config.OperationDefaults)
	out.Bindings = CloneBindings(config.Bindings)
	out.Network = CloneNetwork(config.Network)
	return out
}

func CloneNetwork(network *egress.ConnectionOptions) *egress.ConnectionOptions {
	if network == nil {
		return nil
	}
	copied := *network
	copied.ConnectTimeoutMS = ptr.Clone(network.ConnectTimeoutMS)
	copied.TLSHandshakeTimeoutMS = ptr.Clone(network.TLSHandshakeTimeoutMS)
	copied.ResponseHeaderTimeoutMS = ptr.Clone(network.ResponseHeaderTimeoutMS)
	copied.IdleConnTimeoutMS = ptr.Clone(network.IdleConnTimeoutMS)
	copied.MaxIdleConns = ptr.Clone(network.MaxIdleConns)
	copied.MaxIdleConnsPerHost = ptr.Clone(network.MaxIdleConnsPerHost)
	copied.MaxConnsPerHost = ptr.Clone(network.MaxConnsPerHost)
	return &copied
}

func CloneRawMap(values map[string]json.RawMessage) map[string]json.RawMessage {
	if values == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		out[name] = bytes.Clone(value)
	}
	return out
}

func CloneDefaults(values map[string]DefaultSet) map[string]DefaultSet {
	if values == nil {
		return nil
	}
	out := make(map[string]DefaultSet, len(values))
	for name, value := range values {
		value.Values = CloneRawMap(value.Values)
		value.NativeOptions = CloneRawMap(value.NativeOptions)
		out[name] = value
	}
	return out
}

func CloneBindings(values map[string]Binding) map[string]Binding {
	if values == nil {
		return nil
	}
	out := make(map[string]Binding, len(values))
	for name, binding := range values {
		binding.Defaults = CloneDefaults(binding.Defaults)
		out[name] = binding
	}
	return out
}
