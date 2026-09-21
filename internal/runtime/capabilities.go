package runtime

import (
	"encoding/json"
	"slices"
)

type RouteCapabilities struct {
	Operations          []string          `json:"operations"`
	OperationSupport    map[string]string `json:"operation_support"`
	InputModalities     []string          `json:"input_modalities"`
	OutputModalities    []string          `json:"output_modalities"`
	ContextLength       *int64            `json:"context_length"`
	MaxOutputTokens     *int64            `json:"max_output_tokens"`
	SupportedParameters *[]string         `json:"supported_parameters"`
	Unknown             []string          `json:"unknown"`
}

func EffectiveCapabilities(snapshot *Snapshot, route Route) RouteCapabilities {
	operations := slices.Clone(route.Operations)
	slices.Sort(operations)
	capabilities := RouteCapabilities{Operations: operations, OperationSupport: map[string]string{}}
	var enabled []map[string]bool
	var considered []ModelMetadata
	for _, target := range route.Targets {
		provider, ok := snapshot.Providers[target.ProviderID]
		if !ok || !provider.Enabled {
			continue
		}
		certified := map[string]bool{}
		for _, c := range provider.Capabilities {
			if c.Model == target.ProviderModel && slices.Contains(route.Operations, c.Operation) {
				certified[c.Operation] = true
			}
		}
		enabled = append(enabled, certified)
		if len(certified) == 0 {
			continue
		}
		var metadata ModelMetadata
		_ = json.Unmarshal(provider.Models[target.ProviderModel], &metadata)
		considered = append(considered, metadata)
	}
	for _, operation := range operations {
		status := "unknown"
		if len(enabled) > 0 {
			count := 0
			for _, certified := range enabled {
				if certified[operation] {
					count++
				}
			}
			switch count {
			case len(enabled):
				status = "guaranteed"
			case 0:
			default:
				status = "target_dependent"
			}
		}
		capabilities.OperationSupport[operation] = status
	}
	unknown := []string{}
	minimum := func(name string, fact func(ModelMetadata) *int64) *int64 {
		var result *int64
		for _, metadata := range considered {
			value := fact(metadata)
			if value == nil {
				unknown = append(unknown, name)
				return nil
			}
			if result == nil || *value < *result {
				result = value
			}
		}
		if len(considered) == 0 {
			unknown = append(unknown, name)
		}
		return result
	}
	intersection := func(fact func(ModelMetadata) []string) ([]string, bool) {
		var common []string
		for i, metadata := range considered {
			values := fact(metadata)
			if len(values) == 0 {
				return nil, false
			}
			sorted := slices.Clone(values)
			slices.Sort(sorted)
			sorted = slices.Compact(sorted)
			if i == 0 {
				common = sorted
				continue
			}
			common = slices.DeleteFunc(common, func(value string) bool { return !slices.Contains(sorted, value) })
		}
		if len(considered) == 0 {
			return nil, false
		}
		return common, true
	}
	capabilities.ContextLength = minimum("context_length", func(m ModelMetadata) *int64 { return m.ContextLength })
	capabilities.MaxOutputTokens = minimum("max_output_tokens", func(m ModelMetadata) *int64 { return m.MaxOutputTokens })
	if values, ok := intersection(func(m ModelMetadata) []string { return m.InputModalities }); ok {
		capabilities.InputModalities = values
	} else {
		capabilities.InputModalities = []string{}
		unknown = append(unknown, "input_modalities")
	}
	if values, ok := intersection(func(m ModelMetadata) []string { return m.OutputModalities }); ok {
		capabilities.OutputModalities = values
	} else {
		capabilities.OutputModalities = []string{}
		unknown = append(unknown, "output_modalities")
	}
	if values, ok := intersection(func(m ModelMetadata) []string {
		if m.SupportedParameters == nil {
			return nil
		}
		return *m.SupportedParameters
	}); ok {
		capabilities.SupportedParameters = &values
	}
	if capabilities.SupportedParameters == nil {
		unknown = append(unknown, "supported_parameters")
	}
	slices.Sort(unknown)
	capabilities.Unknown = unknown
	return capabilities
}
