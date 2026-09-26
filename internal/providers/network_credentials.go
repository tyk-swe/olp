package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
)

// StoreNetworkCredential uses the existing encryption authority, but a distinct
// provider-owned reference table so API credential slots cannot select TLS keys.
func (s *Server) StoreNetworkCredential(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error) {
	if len(secret) > maxCredentialBytes {
		return "", access.Invalid("credential", "Network credentials must fit within 64 KiB.")
	}
	if err := egress.ValidateConnectionSecret([]byte(secret)); err != nil {
		return "", access.Invalid("credential", err.Error())
	}
	id := access.NewID()
	if _, err := tx.Exec(ctx, "INSERT INTO olp.provider_network_credentials(id,provider_id,version) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_network_credentials WHERE provider_id=$2))", id, providerID); err != nil {
		return "", err
	}
	if err := s.Access.Keys.Store(ctx, tx, s.Access.Installation, id, "provider_credential", []byte(secret), nil); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Server) networkCredentials(r *http.Request) (access.Reply, error) {
	principal, err := s.Access.Principal(r, s.Access.Pool, "read")
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err := checkProvider(r.Context(), s.Access.Pool, principal, id, false); err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',id,'version',version,'created_at',created_at,'revoked_at',revoked_at) FROM olp.provider_network_credentials WHERE provider_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReply(items, page), nil
}

func (s *Server) createNetworkCredential(r *http.Request) (access.Reply, error) {
	var input rotateRequest
	if err := access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	principal, err := s.Access.Principal(r, tx, "configure")
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	provider, err := load(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.ProjectAccess(principal, provider.ProjectID, true); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, provider.ETag); err != nil {
		return access.Reply{}, err
	}
	credentialID, err := s.StoreNetworkCredential(r.Context(), tx, id, input.Credential)
	if err != nil {
		return access.Reply{}, err
	}
	etag, err := touch(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, principal.ID, "provider.network_credential.create", "provider", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusCreated, ETag: etag, Body: map[string]any{"provider_id": id, "credential_id": credentialID, "etag": etag}}
	if err := s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) revokeNetworkCredential(r *http.Request) (access.Reply, error) {
	return s.mutation(r, "provider.network_credential.revoke", func(ctx context.Context, tx pgx.Tx, principal access.Principal, current *record) (access.Reply, error) {
		id, err := access.IDParam(r, "credential_id")
		if err != nil {
			return access.Reply{}, err
		}
		var version int
		if err := tx.QueryRow(ctx, "UPDATE olp.provider_network_credentials SET revoked_at=now() WHERE id=$1 AND provider_id=$2 AND revoked_at IS NULL RETURNING version", id, current.ID).Scan(&version); err != nil {
			return access.Reply{}, err
		}
		generation, err := access.AdvanceAuthority(r, tx)
		if err != nil {
			return access.Reply{}, err
		}
		etag, err := touch(ctx, tx, current.ID)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(map[string]any{"provider_id": current.ID, "credential_id": id, "etag": etag, "runtime_generation": generation}, etag), nil
	})
}

func (s *Server) validateNetworkReference(ctx context.Context, q access.Queryer, providerID string, cfg *Configuration) error {
	if cfg.Options.Network == nil || cfg.Options.Network.CredentialID == "" {
		return nil
	}
	var valid bool
	err := q.QueryRow(ctx, "SELECT revoked_at IS NULL FROM olp.provider_network_credentials WHERE id=$1 AND provider_id=$2", cfg.Options.Network.CredentialID, providerID).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !valid {
		return access.Invalid("configuration.options.network.credential_id", "Select an unrevoked network credential owned by this provider.")
	}
	return err
}

func (s *Server) networkSecret(ctx context.Context, cfg *Configuration) ([]byte, error) {
	if cfg.Options.Network == nil || cfg.Options.Network.CredentialID == "" {
		return nil, nil
	}
	tx, err := s.Access.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := s.validateNetworkReference(ctx, tx, cfg.ProviderID, cfg); err != nil {
		return nil, err
	}
	return s.Access.Keys.Read(ctx, tx, s.Access.Installation, cfg.Options.Network.CredentialID, "provider_credential")
}

func (s *Server) connectionClient(ctx context.Context, cfg *Configuration, credential []byte) (*http.Client, error) {
	if cfg.Options.Network == nil && cfg.ProfileID == "" {
		return s.client, nil
	}
	secret, err := s.networkSecret(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if cfg.ProviderID == "" {
		return nil, errors.New("provider identity is required for configured connections")
	}
	digest := sha256.Sum256(credential)
	scope := cfg.ProviderID + "/" + cfg.transportFingerprint() + "/" + hex.EncodeToString(digest[:])
	return s.connections.ClientScoped(scope, *s.Egress, cfg.Options.Network, secret, probeTimeout)
}
