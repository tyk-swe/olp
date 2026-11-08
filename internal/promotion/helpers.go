package promotion

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
)

const MaxCredentialRef = 200

const MaxSecretBytes = 64 << 10

func NetworkRef(name string) string { return name + "/network" }

func BillingRef(name string) string { return name + "/billing" }

func OrEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func ProviderNameMap(ctx context.Context, q access.Queryer) (map[string]string, error) {
	rows, err := q.Query(ctx, "SELECT id::text,name FROM olp.providers")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		names[id] = name
	}
	return names, rows.Err()
}

func (s *Server) BindingMatches(ctx context.Context, q access.Queryer, credentialID *string, binding CredentialBinding) (bool, error) {
	if credentialID == nil {
		return false, nil
	}
	if binding.Reference != nil {
		var data []byte
		if err := q.QueryRow(ctx, "SELECT external_reference FROM olp.provider_credentials WHERE id=$1 AND revoked_at IS NULL", *credentialID).Scan(&data); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		var current secretstore.Reference
		return len(data) > 0 && json.Unmarshal(data, &current) == nil && current == *binding.Reference, nil
	}
	if s.Access == nil || s.Access.Keys == nil {
		return false, errors.New("credential comparison unavailable")
	}
	current, err := s.Access.Keys.Read(ctx, q, s.Access.Installation, *credentialID, secrets.ProviderCredential)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer clear(current)
	return sha256.Sum256(current) == sha256.Sum256([]byte(binding.Secret)), nil
}

const GrantBindingRefused = "A grant backs this credential slot: enroll a grant for it after applying instead of binding a secret."

func MapCode[A, B any](values []A, project func(A) B) []B {
	return append([]B{}, slices.Collect(func(yield func(B) bool) {
		for _, value := range values {
			if !yield(project(value)) {
				return
			}
		}
	})...)
}
