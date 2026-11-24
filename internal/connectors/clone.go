package connectors

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/ptr"
)

func (config Config) Clone() Config {
	out := config
	out.Plugin = ptr.Clone(config.Plugin)
	out.TuningDatasetURIPrefixes = slices.Clone(config.TuningDatasetURIPrefixes)
	out.DocumentURIPrefixes = slices.Clone(config.DocumentURIPrefixes)
	if config.BatchStorage != nil {
		storage := *config.BatchStorage
		out.BatchStorage = &storage
	}
	out.SemanticHeaders = maps.Clone(config.SemanticHeaders)
	out.QuerySettings = maps.Clone(config.QuerySettings)
	out.PluginOptions = maps.Clone(config.PluginOptions)
	out.CredentialHeaders = slices.Clone(config.CredentialHeaders)
	out.Models = copyValues(config.Models)
	out.OperationDefaults = copyDefaults(config.OperationDefaults)
	if config.Bindings != nil {
		out.Bindings = make(map[string]Binding, len(config.Bindings))
		for name, binding := range config.Bindings {
			binding.Defaults = copyDefaults(binding.Defaults)
			out.Bindings[name] = binding
		}
	}
	if config.Network != nil {
		network := *config.Network
		network.ConnectTimeoutMS = ptr.Clone(network.ConnectTimeoutMS)
		network.TLSHandshakeTimeoutMS = ptr.Clone(network.TLSHandshakeTimeoutMS)
		network.ResponseHeaderTimeoutMS = ptr.Clone(network.ResponseHeaderTimeoutMS)
		network.IdleConnTimeoutMS = ptr.Clone(network.IdleConnTimeoutMS)
		network.MaxIdleConns = ptr.Clone(network.MaxIdleConns)
		network.MaxIdleConnsPerHost = ptr.Clone(network.MaxIdleConnsPerHost)
		network.MaxConnsPerHost = ptr.Clone(network.MaxConnsPerHost)
		out.Network = &network
	}
	return out
}

func copyValues(values map[string]json.RawMessage) map[string]json.RawMessage {
	if values == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		out[name] = bytes.Clone(value)
	}
	return out
}

func copyDefaults(values map[string]DefaultSet) map[string]DefaultSet {
	if values == nil {
		return nil
	}
	out := make(map[string]DefaultSet, len(values))
	for name, value := range values {
		value.Values = copyValues(value.Values)
		value.NativeOptions = copyValues(value.NativeOptions)
		out[name] = value
	}
	return out
}
