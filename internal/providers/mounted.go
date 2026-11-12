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
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/secrets"
)

func LoadMounted(path string, policy *egress.Policy) (map[string]release.MountedProvider, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read mounted connector configuration")
	}
	defer file.Close()
	var document struct {
		Providers []struct {
			ProviderID            string                   `json:"provider_id"`
			Configuration         connection.Configuration `json:"configuration"`
			CredentialFile        *string                  `json:"credential_file"`
			NetworkCredentialFile *string                  `json:"network_credential_file"`
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
	out := map[string]release.MountedProvider{}
	for _, entry := range document.Providers {
		if _, err = uuid.Parse(entry.ProviderID); err != nil {
			return nil, errors.New("invalid mounted provider identifier")
		}
		if _, exists := out[entry.ProviderID]; exists {
			return nil, errors.New("duplicate mounted provider identifier")
		}
		cfg, err := connection.ValidateUnpinned(policy, entry.Configuration)
		if err != nil {
			return nil, err
		}
		// A grant is refreshed beneath its credential version in the database,
		// which a gateway without the master key can't read.
		if cfg.Grant() {
			return nil, errors.New("mounted connectors can't serve a provider that authenticates with a grant; run the gateway with the master key")
		}
		required := cfg.CredentialRequired()
		if required != (entry.CredentialFile != nil) {
			return nil, errors.New("mounted credential file does not match authentication mode")
		}
		var networkSecret []byte
		networkRequired := cfg.Options.Network != nil && cfg.Options.Network.CredentialID != ""
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
			if err = ValidCredential(string(secret)); err != nil {
				return nil, errors.New("mounted credential file must not contain NUL bytes, and a multiline credential must be JSON")
			}
		}
		config, err := cfg.Serving()
		if err != nil {
			return nil, err
		}
		out[entry.ProviderID] = release.MountedProvider{Configuration: config, Credential: secret, NetworkCredential: networkSecret}
	}
	return out, nil
}
