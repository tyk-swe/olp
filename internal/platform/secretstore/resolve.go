package secretstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// Resolve fetches only the pinned version. No alias or older cached version is
// consulted when the store refuses the requested one.
func (r *Resolver) Resolve(ctx context.Context, reference Reference) ([]byte, error) {
	if err := reference.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var secret []byte
	switch reference.Store {
	case "aws":
		config, err := r.awsConfig(ctx, reference.Region)
		if err != nil {
			return nil, ErrUnavailable
		}
		output, err := secretsmanager.NewFromConfig(config).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &reference.SecretID, VersionId: &reference.Version})
		if err != nil || output.VersionId == nil || *output.VersionId != reference.Version {
			return nil, ErrUnavailable
		}
		if output.SecretString != nil {
			secret = []byte(*output.SecretString)
		} else {
			secret = output.SecretBinary
		}
	case "gcp":
		token, err := r.googleToken(ctx)
		if err != nil {
			return nil, ErrUnavailable
		}
		var output struct {
			Name    string `json:"name"`
			Payload struct {
				Data string `json:"data"`
				CRC  string `json:"dataCrc32c"`
			} `json:"payload"`
		}
		name := reference.SecretID + "/versions/" + reference.Version
		if r.json(ctx, http.MethodGet, "https://secretmanager.googleapis.com/v1/"+name+":access", bearer(token), nil, &output) != nil || output.Name != name {
			return nil, ErrUnavailable
		}
		secret, err = base64.StdEncoding.Strict().DecodeString(output.Payload.Data)
		checksum, crcErr := strconv.ParseUint(output.Payload.CRC, 10, 32)
		if err != nil || crcErr != nil || uint32(checksum) != crc32.Checksum(secret, crc32.MakeTable(crc32.Castagnoli)) {
			clear(secret)
			return nil, ErrUnavailable
		}
	case "azure":
		u, err := azureResource(reference.SecretID, "secrets", false)
		if err != nil {
			return nil, ErrUnavailable
		}
		token, err := r.azureToken(ctx)
		if err != nil {
			return nil, ErrUnavailable
		}
		u.Path += "/" + reference.Version
		u.RawQuery = "api-version=7.4"
		var output struct {
			Value string `json:"value"`
			ID    string `json:"id"`
		}
		if r.json(ctx, http.MethodGet, u.String(), bearer(token), nil, &output) != nil || output.ID != reference.SecretID+"/"+reference.Version {
			return nil, ErrUnavailable
		}
		secret = []byte(output.Value)
	case "vault":
		u, err := vaultResource(reference.SecretID)
		if err != nil {
			return nil, ErrUnavailable
		}
		token, err := r.vaultToken(ctx, u)
		if err != nil {
			return nil, ErrUnavailable
		}
		u.RawQuery = "version=" + reference.Version
		var output struct {
			Data struct {
				Data     map[string]json.RawMessage `json:"data"`
				Metadata struct {
					Version   int64  `json:"version"`
					Destroyed bool   `json:"destroyed"`
					Deleted   string `json:"deletion_time"`
				} `json:"metadata"`
			} `json:"data"`
		}
		if r.json(ctx, http.MethodGet, u.String(), vaultHeaders(token), nil, &output) != nil || strconv.FormatInt(output.Data.Metadata.Version, 10) != reference.Version || output.Data.Metadata.Destroyed || output.Data.Metadata.Deleted != "" {
			return nil, ErrUnavailable
		}
		var value string
		if json.Unmarshal(output.Data.Data[reference.Field], &value) != nil {
			return nil, ErrUnavailable
		}
		secret = []byte(value)
	}
	if len(secret) == 0 || len(secret) > maxSecret {
		clear(secret)
		return nil, ErrUnavailable
	}
	return secret, nil
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}
func vaultHeaders(token string) map[string]string { return map[string]string{"X-Vault-Token": token} }
