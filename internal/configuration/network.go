package configuration

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers"
)

func networkRef(name string) string { return name + "/network" }

func portableNetwork(entry *ProviderEntry) {
	if network := entry.Configuration.Options.Network; network != nil && network.CredentialID != "" {
		copy := *network
		copy.CredentialID = ""
		entry.Configuration.Options.Network = &copy
		ref := networkRef(entry.Name)
		entry.NetworkCredentialRef = &ref
	}
}

// networkConfiguration binds portable network references without mutating the
// canonical import document or exposing a destination environment's secret IDs.
func (s *Server) networkConfiguration(ctx context.Context, tx pgx.Tx, providerID string, entry *ProviderEntry, existing *existingProvider, bindings map[string]string) (providers.Configuration, error) {
	cfg := entry.Configuration
	if cfg.Options.Network == nil {
		return cfg, nil
	}
	network := *cfg.Options.Network
	cfg.Options.Network = &network
	if entry.NetworkCredentialRef == nil {
		return cfg, nil
	}
	var current *string
	if existing != nil {
		current = existing.NetworkCredentialID
	}
	if secret, provided := bindings[*entry.NetworkCredentialRef]; provided {
		same, err := s.bindingMatches(ctx, tx, current, secret)
		if err != nil {
			return cfg, err
		}
		if !same {
			if s.StoreNetworkCredential == nil {
				return cfg, access.Invalid("secret_bindings", "Network credential storage is unavailable.")
			}
			id, err := s.StoreNetworkCredential(ctx, tx, providerID, secret)
			if err != nil {
				return cfg, err
			}
			current = &id
		}
	}
	if current == nil {
		return cfg, access.Invalid("secret_bindings", "Supply the provider's network credential binding.")
	}
	network.CredentialID = *current
	return cfg, nil
}

func (s *Server) bindNetwork(ctx context.Context, tx pgx.Tx, providerID string, entry *ProviderEntry, existing *existingProvider, bindings map[string]string) error {
	cfg, err := s.networkConfiguration(ctx, tx, providerID, entry, existing, bindings)
	if err != nil {
		return err
	}
	var current []byte
	if err := tx.QueryRow(ctx, "SELECT configuration FROM olp.providers WHERE id=$1", providerID).Scan(&current); err != nil {
		return err
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	var stored providers.Configuration
	if err := json.Unmarshal(current, &stored); err != nil {
		return err
	}
	canonical, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if string(canonical) == string(encoded) {
		return nil
	}
	_, err = tx.Exec(ctx, "UPDATE olp.providers SET configuration=$2,etag=$3,draft_dirty=true,updated_at=now() WHERE id=$1", providerID, encoded, access.NewID())
	return err
}
