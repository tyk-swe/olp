package runtime

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"

	"github.com/tyk-swe/olp/internal/connectors"
)

// MountedProvider supplies a transport when a gateway has no database decryption
// key. Published capabilities, restrictions, quotas, and credential identities
// remain authoritative; a mounted file cannot create a route or credential pool.
type MountedProvider struct {
	Configuration     Configuration
	Credential        []byte `json:"-"`
	NetworkCredential []byte `json:"-"`
}

func (m MountedProvider) String() string { return "MountedProvider([REDACTED])" }
func installMounted(snapshot *Snapshot, entries map[string]MountedProvider) (map[string][]byte, error) {
	credentials := map[string][]byte{}
	for id, p := range snapshot.Providers {
		if !p.Enabled {
			continue
		}
		mounted, ok := entries[id]
		if !ok {
			return nil, errors.New("enabled provider has no mounted connector")
		}
		cfg := mounted.Configuration
		if cfg.Kind != p.Kind || cfg.AuthMode != p.AuthMode {
			return nil, errors.New("mounted connector kind and authentication must match the published provider")
		}
		if p.DefaultSlotID == "" {
			return nil, errors.New("republish the provider to record its default slot before mounting credentials")
		}
		for _, slot := range p.Slots {
			if !slot.Enabled {
				continue
			}
			isDefault := slot.ID == p.DefaultSlotID
			if !isDefault {
				return nil, errors.New("OLP_MASTER_KEY_FILE is required for named credential slots")
			}
			if connectors.SecretRequired(cfg.AuthMode) {
				if slot.CredentialID == nil {
					return nil, errors.New("mounted connector requires a published credential identity")
				}
				credentials[*slot.CredentialID] = append([]byte(nil), mounted.Credential...)
			}
		}
		if p.Network != nil && p.Network.CredentialID != "" {
			if cfg.Options.Network == nil || cfg.Options.Network.CredentialID != p.Network.CredentialID || len(mounted.NetworkCredential) == 0 {
				return nil, errors.New("mounted network credential must match the published provider reference")
			}
			credentials[p.Network.CredentialID] = append([]byte(nil), mounted.NetworkCredential...)
		} else if len(mounted.NetworkCredential) > 0 {
			return nil, errors.New("mounted network credential has no published authority")
		}
		if p.ProfileID != "" {
			// Explicit profiles cannot silently acquire unpublished model-significant
			// options from a local file. Only secret material is mounted.
			left := []any{p.ProfileID, p.ProfileRevision, p.Endpoint, p.CloudRegion, p.CloudProject, p.Deployment, p.APIVersion, p.CredentialHeaders, p.ParameterDefaults, p.SemanticHeaders, p.QuerySettings, p.OperationDefaults, p.Bindings, p.Network}
			right := []any{cfg.ProfileID, cfg.ProfileRevision, cfg.Endpoint, cfg.CloudRegion, cfg.CloudProject, cfg.Deployment, cfg.APIVersion, cfg.Options.CredentialHeaders, cfg.Options.ParameterDefaults, cfg.Options.SemanticHeaders, cfg.Options.QuerySettings, cfg.Options.OperationDefaults, cfg.Options.Bindings, cfg.Options.Network}
			if !mountedSettingsEqual(left, right) {
				return nil, errors.New("mounted explicit profile configuration must match the published revision")
			}
			for model, raw := range cfg.Options.Models {
				var override, current ModelMetadata
				_ = json.Unmarshal(raw, &override)
				_ = json.Unmarshal(p.Models[model], &current)
				if override.Deployment != nil && (current.Deployment == nil || *override.Deployment != *current.Deployment) {
					return nil, errors.New("mounted model deployment differs from the published binding")
				}
			}
			continue
		}
		if cfg.ProfileID != "" {
			return nil, errors.New("mounting a profile requires an explicitly published profile")
		}
		p.Network = cfg.Options.Network
		if cfg.Endpoint != "" {
			p.Endpoint = cfg.Endpoint
		}
		p.CloudRegion = cfg.CloudRegion
		p.CloudProject = cfg.CloudProject
		p.Deployment = cfg.Deployment
		p.APIVersion = cfg.APIVersion
		p.CredentialHeaders = cfg.Options.CredentialHeaders
		p.ParameterDefaults = maps.Clone(cfg.Options.ParameterDefaults)
		// Model facts and vendor identity are published policy inputs. Mounted files
		// may supply model deployment mappings but cannot assert new privacy facts.
		p.Models = maps.Clone(p.Models)
		for model, value := range cfg.Options.Models {
			var override, current ModelMetadata
			_ = json.Unmarshal(value, &override)
			if override.Deployment == nil {
				continue
			}
			_ = json.Unmarshal(p.Models[model], &current)
			current.Deployment = override.Deployment
			if p.Models == nil {
				p.Models = map[string]json.RawMessage{}
			}
			p.Models[model], _ = json.Marshal(current)
		}
		snapshot.Providers[id] = p
	}
	return credentials, nil
}

func mountedSettingsEqual(left, right []any) bool {
	normalize := func(values []any) []byte {
		for i, value := range values {
			v := reflect.ValueOf(value)
			if v.IsValid() && (v.Kind() == reflect.Map || v.Kind() == reflect.Slice) && v.Len() == 0 {
				values[i] = nil
			}
		}
		encoded, _ := json.Marshal(values)
		return encoded
	}
	return string(normalize(left)) == string(normalize(right))
}
