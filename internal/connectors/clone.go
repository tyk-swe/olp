package connectors

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
)

// Clone owns every mutable configuration value. PluginProfile is immutable.
func (config Config) Clone() Config {
	out := config
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
		network.ConnectTimeoutMS = copyPointer(network.ConnectTimeoutMS)
		network.TLSHandshakeTimeoutMS = copyPointer(network.TLSHandshakeTimeoutMS)
		network.ResponseHeaderTimeoutMS = copyPointer(network.ResponseHeaderTimeoutMS)
		network.IdleConnTimeoutMS = copyPointer(network.IdleConnTimeoutMS)
		network.MaxIdleConns = copyPointer(network.MaxIdleConns)
		network.MaxIdleConnsPerHost = copyPointer(network.MaxIdleConnsPerHost)
		network.MaxConnsPerHost = copyPointer(network.MaxConnsPerHost)
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
func copyPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}
