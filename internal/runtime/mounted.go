package runtime

import (
	"encoding/json"
	"errors"
	"maps"

	"github.com/tyk-swe/olp/internal/connectors"
)

// MountedProvider supplies a transport when a gateway has no database decryption
// key. Published capabilities, restrictions, quotas, and credential identities
// remain authoritative; a mounted file cannot create a route or credential pool.
type MountedProvider struct {
	Configuration Configuration
	Credential    []byte
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
