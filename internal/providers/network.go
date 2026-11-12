package providers

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
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
	if err := s.access.Keys.Store(ctx, tx, s.access.Installation, id, secrets.ProviderCredential, []byte(secret), nil); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Server) networkCredentials(r *http.Request, principal access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err := visibleProvider(r.Context(), s.access.Pool, principal, id); err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',id,'version',version,'created_at',created_at,'revoked_at',revoked_at) FROM olp.provider_network_credentials WHERE provider_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReply(items, page), nil
}

type networkCredentialRequest struct {
	Credential string `json:"credential"`
}

func (s *Server) createNetworkCredential(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input networkCredentialRequest
	if err := access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	return access.Mutate(s.access, r, input, changeable(r.Context(), id), func(tx pgx.Tx, principal access.Principal, provider *record) (access.Reply, error) {
		if err := access.Match(r, provider.ETag); err != nil {
			return access.Reply{}, err
		}
		created, err := s.createNetworkCredentialVersion(r.Context(), tx, id, input.Credential)
		if err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(r.Context(), tx, r, principal.Actor(), "provider.network_credential.create", "provider", id, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusCreated, ETag: created.ETag, Body: map[string]any{"provider_id": id, "credential_id": created.ID, "etag": created.ETag}}, nil
	})
}

func (s *Server) revokeNetworkCredential(r *http.Request, _ access.Principal) (access.Reply, error) {
	return s.mutation(r, "provider.network_credential.revoke", nil, func(ctx context.Context, tx pgx.Tx, principal access.Principal, current *record) (access.Reply, error) {
		id, err := access.IDParam(r, "credential_id")
		if err != nil {
			return access.Reply{}, err
		}
		revoked, err := s.revokeNetworkCredentialVersion(ctx, tx, current, id)
		if err != nil {
			return access.Reply{}, err
		}
		return access.Detail(map[string]any{"provider_id": current.ID, "credential_id": id, "etag": revoked.ETag, "runtime_generation": revoked.Generation}, revoked.ETag), nil
	})
}

func validateNetworkReference(ctx context.Context, q access.Queryer, providerID string, network *egress.ConnectionOptions) error {
	if network == nil || network.CredentialID == "" {
		return nil
	}
	var valid bool
	err := q.QueryRow(ctx, "SELECT revoked_at IS NULL FROM olp.provider_network_credentials WHERE id=$1 AND provider_id=$2", network.CredentialID, providerID).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !valid {
		return access.Invalid("configuration.options.network.credential_id", "Select an unrevoked network credential owned by this provider.")
	}
	return err
}

type networkSecrets struct{ server *access.Server }

func (n networkSecrets) NetworkSecret(ctx context.Context, providerID, credentialID string) ([]byte, error) {
	tx, err := n.server.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := validateNetworkReference(ctx, tx, providerID, &egress.ConnectionOptions{CredentialID: credentialID}); err != nil {
		return nil, err
	}
	return n.server.Keys.Read(ctx, tx, n.server.Installation, credentialID, secrets.ProviderCredential)
}

func (s *Server) revokeNetworkCredentialVersion(ctx context.Context, tx pgx.Tx, current *record, id string) (revokedCredential, error) {
	var version int
	if err := tx.QueryRow(ctx, "UPDATE olp.provider_network_credentials SET revoked_at=now() WHERE id=$1 AND provider_id=$2 AND revoked_at IS NULL RETURNING version", id, current.ID).Scan(&version); err != nil {
		return revokedCredential{}, err
	}
	generation, err := access.AdvanceAuthority(ctx, tx)
	if err != nil {
		return revokedCredential{}, err
	}
	etag, err := touch(ctx, tx, current.ID)
	if err != nil {
		return revokedCredential{}, err
	}
	return revokedCredential{Version: version, Generation: generation, ETag: etag}, nil
}

type createdNetworkCredential struct {
	ID   string
	ETag string
}

func (s *Server) createNetworkCredentialVersion(ctx context.Context, tx pgx.Tx, providerID, credential string) (createdNetworkCredential, error) {
	id, err := s.StoreNetworkCredential(ctx, tx, providerID, credential)
	if err != nil {
		return createdNetworkCredential{}, err
	}
	etag, err := touch(ctx, tx, providerID)
	if err != nil {
		return createdNetworkCredential{}, err
	}
	return createdNetworkCredential{ID: id, ETag: etag}, nil
}
