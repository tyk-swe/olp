package mcpservers

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

func validateDelegation(in *Input) error {
	if !in.Delegation.Valid() {
		return access.Invalid("delegation", "Supply an HTTPS issuer and bounded audience.")
	}
	if in.Delegation != nil && in.OAuth == nil {
		return access.Invalid("delegation", "Delegation requires personal OAuth grants.")
	}
	if in.DelegationPrivateKey == nil {
		return nil
	}
	if in.Delegation == nil {
		return access.Invalid("delegation_private_key", "Configure delegation before supplying its signing key.")
	}
	key, err := secrets.MCPPrivateKey([]byte(*in.DelegationPrivateKey))
	if err != nil {
		return access.Invalid("delegation_private_key", "Supply a PKCS8 Ed25519 private key.")
	}
	clear(key)
	return nil
}

func (s *Server) saveDelegationKey(ctx context.Context, tx pgx.Tx, in Input, previous *Definition) (*string, error) {
	if in.Delegation == nil {
		return nil, nil
	}
	if in.DelegationPrivateKey != nil {
		id := access.NewID()
		material := []byte(*in.DelegationPrivateKey)
		defer clear(material)
		if err := s.Access.Keys.Store(ctx, tx, s.Access.Installation, id, secrets.MCPDelegationSigning, material, nil); err != nil {
			return nil, err
		}
		return &id, nil
	}
	if previous != nil && previous.DelegationKeyID != nil {
		return previous.DelegationKeyID, nil
	}
	return nil, access.Invalid("delegation_private_key", "Supply this installation's signing key before enabling delegation.")
}
