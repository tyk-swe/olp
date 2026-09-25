package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

func LoadMounted(path string, policy *egress.Policy) (map[string]runtime.MountedProvider, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read mounted connector configuration")
	}
	defer file.Close()
	var document struct {
		Providers []struct {
			ProviderID            string        `json:"provider_id"`
			Configuration         Configuration `json:"configuration"`
			Model                 *string       `json:"model"`
			CredentialFile        *string       `json:"credential_file"`
			NetworkCredentialFile *string       `json:"network_credential_file"`
		} `json:"providers"`
	}
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("mounted connector configuration exceeds its bound")
	}
	if _, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 4 << 20}); err != nil {
		return nil, errors.New("ambiguous mounted connector configuration")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF || len(document.Providers) > 2000 {
		return nil, errors.New("invalid mounted connector configuration")
	}
	out := map[string]runtime.MountedProvider{}
	for _, entry := range document.Providers {
		// Published snapshots key providers by canonical UUID.
		if id, err := uuid.Parse(entry.ProviderID); err != nil || id.String() != entry.ProviderID {
			return nil, errors.New("invalid mounted provider identifier")
		}
		if _, exists := out[entry.ProviderID]; exists {
			return nil, errors.New("duplicate mounted provider identifier")
		}
		entry.Configuration.Normalize()
		if err = entry.Configuration.Validate(policy); err != nil {
			return nil, err
		}
		required := entry.Configuration.CredentialRequired()
		if required != (entry.CredentialFile != nil) {
			return nil, errors.New("mounted credential file does not match authentication mode")
		}
		var networkSecret []byte
		networkRequired := entry.Configuration.Options.Network != nil && entry.Configuration.Options.Network.CredentialID != ""
		if networkRequired != (entry.NetworkCredentialFile != nil) {
			return nil, errors.New("mounted network credential file must match a configured reference")
		}
		if networkRequired {
			networkSecret, err = secrets.ReadFile(*entry.NetworkCredentialFile)
			if err != nil {
				return nil, err
			}
			if err := egress.ValidateConnectionSecret(networkSecret); err != nil {
				return nil, err
			}
		}
		var secret []byte
		if required {
			secret, err = secrets.ReadFile(*entry.CredentialFile)
			if err != nil {
				return nil, err
			}
		}
		if entry.Model != nil {
			if err = ValidModelName("model", *entry.Model); err != nil {
				return nil, err
			}
		}
		var config runtime.Configuration
		data, _ := json.Marshal(entry.Configuration)
		if err = json.Unmarshal(data, &config); err != nil {
			return nil, err
		}
		out[entry.ProviderID] = runtime.MountedProvider{Configuration: config, Credential: secret, NetworkCredential: networkSecret}
	}
	return out, nil
}
