package authority

import (
	"github.com/tyk-swe/olp/internal/attribution"
	"maps"
)

func (p KeyPolicy) AttributionPolicy() attribution.Policy {
	return attribution.Policy{Required: p.RequiredAttributionKeys, Defaults: p.AttributionDefaults}
}

func (a Authority) ResolveAttribution(labels map[string]string) (map[string]string, error) {
	return attribution.Resolve(labels, a.ProjectAttributionPolicy, a.Policy.AttributionPolicy())
}

// Established sessions must retain their original accounting labels under current policy.
func (a Authority) AllowsAttribution(labels map[string]string) bool {
	resolved, err := a.ResolveAttribution(labels)
	return err == nil && maps.Equal(labels, resolved)
}
