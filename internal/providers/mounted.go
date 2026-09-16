package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
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
			ProviderID     string        `json:"provider_id"`
			Configuration  Configuration `json:"configuration"`
			Model          *string       `json:"model"`
			CredentialFile *string       `json:"credential_file"`
		} `json:"providers"`
	}
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("mounted connector configuration exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&document) != nil || decoder.Decode(new(any)) != io.EOF || len(document.Providers) > 2000 {
		return nil, errors.New("invalid mounted connector configuration")
	}
	out := map[string]runtime.MountedProvider{}
	for _, entry := range document.Providers {
		if _, err = uuid.Parse(entry.ProviderID); err != nil {
			return nil, errors.New("invalid mounted provider identifier")
		}
		if _, exists := out[entry.ProviderID]; exists {
			return nil, errors.New("duplicate mounted provider identifier")
		}
		entry.Configuration.normalize()
		if err = entry.Configuration.validate(policy); err != nil {
			return nil, err
		}
		required := entry.Configuration.credentialRequired()
		if required != (entry.CredentialFile != nil) {
			return nil, errors.New("mounted credential file does not match authentication mode")
		}
		var secret []byte
		if required {
			secret, err = secrets.ReadFile(*entry.CredentialFile)
			if err != nil {
				return nil, err
			}
		}
		if entry.Model != nil {
			if err = validModelName("model", *entry.Model); err != nil {
				return nil, err
			}
		}
		var config runtime.Configuration
		data, _ := json.Marshal(entry.Configuration)
		if err = json.Unmarshal(data, &config); err != nil {
			return nil, err
		}
		out[entry.ProviderID] = runtime.MountedProvider{Configuration: config, Credential: secret}
	}
	return out, nil
}
