package providers

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

// A table of its own keeps API credential slots from selecting TLS keys.
func (s *credentialService) storeNetworkCredential(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error) {
	if len(secret) > maxCredentialBytes {
		return "", httpkit.Invalid("credential", "Network credentials must fit within 64 KiB.")
	}
	if err := egress.ValidateConnectionSecret([]byte(secret)); err != nil {
		return "", httpkit.Invalid("credential", err.Error())
	}
	id := ids.New()
	if _, err := tx.Exec(ctx, "INSERT INTO olp.provider_network_credentials(id,provider_id,version) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_network_credentials WHERE provider_id=$2))", id, providerID); err != nil {
		return "", err
	}
	if err := s.access.Keys.Store(ctx, tx, s.access.Installation, id, secrets.ProviderCredential, []byte(secret), nil); err != nil {
		return "", err
	}
	return id, nil
}

func (s *credentialService) networkCredentials(r *http.Request, principal authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := visibleProvider(r.Context(), s.access.Pool, principal, id); err != nil {
		return httpkit.Reply{}, err
	}
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT jsonb_build_object('id',id,'version',version,'created_at',created_at,'revoked_at',revoked_at) FROM olp.provider_network_credentials WHERE provider_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3", id, page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReply(items, page), nil
}

type networkCredentialRequest struct {
	Credential string `json:"credential"`
}

func (s *credentialService) createNetworkCredential(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input networkCredentialRequest
	if err := httpkit.DecodeUnique(r, &input, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.access, r, input, changeable(r.Context(), id), func(tx pgx.Tx, principal authz.Principal, provider *record) (httpkit.Reply, access.Event, error) {
		if err := httpkit.Match(r, provider.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		created, err := s.createNetworkCredentialVersion(r.Context(), tx, id, input.Credential)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: http.StatusCreated, ETag: created.ETag, Body: map[string]any{"provider_id": id, "credential_id": created.ID, "etag": created.ETag}}, access.Event{Action: "provider.network_credential.create", Resource: "provider", ID: id}, nil
	})
}

func (s *credentialService) revokeNetworkCredential(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return providerMutation(s.access, r, "provider.network_credential.revoke", nil, func(ctx context.Context, tx pgx.Tx, principal authz.Principal, current *record) (httpkit.Reply, error) {
		id, err := httpkit.IDParam(r, "credential_id")
		if err != nil {
			return httpkit.Reply{}, err
		}
		revoked, err := s.revokeNetworkCredentialVersion(ctx, tx, current, id)
		if err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.Detail(map[string]any{"provider_id": current.ID, "credential_id": id, "etag": revoked.ETag, "runtime_generation": revoked.Generation}, revoked.ETag), nil
	})
}

type networkSecrets struct{ server *access.Server }

func (n networkSecrets) NetworkSecret(ctx context.Context, providerID, credentialID string) ([]byte, error) {
	return readTx(ctx, n.server.Pool, func(tx pgx.Tx) ([]byte, error) {
		if err := validateNetworkReference(ctx, tx, providerID, &egress.ConnectionOptions{CredentialID: credentialID}); err != nil {
			return nil, err
		}
		return n.server.Keys.Read(ctx, tx, n.server.Installation, credentialID, secrets.ProviderCredential)
	})
}

func (s *credentialService) revokeNetworkCredentialVersion(ctx context.Context, tx pgx.Tx, current *record, id string) (revokedCredential, error) {
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

func (s *credentialService) createNetworkCredentialVersion(ctx context.Context, tx pgx.Tx, providerID, credential string) (createdNetworkCredential, error) {
	id, err := s.storeNetworkCredential(ctx, tx, providerID, credential)
	if err != nil {
		return createdNetworkCredential{}, err
	}
	etag, err := touch(ctx, tx, providerID)
	if err != nil {
		return createdNetworkCredential{}, err
	}
	return createdNetworkCredential{ID: id, ETag: etag}, nil
}
