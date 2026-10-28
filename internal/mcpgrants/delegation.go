package mcpgrants

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

type delegatedClaims struct {
	jwt.RegisteredClaims
	ServerID  string `json:"olp_server_id"`
	ProjectID string `json:"olp_project_id"`
}

func (s *Store) Delegate(ctx context.Context, authority access.Authority, tool routing.MCPToolsetTool) ([]byte, error) {
	bound, bindErr := Bind(authority, tool)
	if bindErr != nil || tool.Delegation == nil {
		return nil, ErrRequired
	}
	var current routing.MCPDelegation
	var keyID string
	queryErr := s.Pool.QueryRow(ctx, `SELECT v.delegation,s.delegation_key_id::text FROM olp.mcp_servers s JOIN olp.mcp_server_revisions v ON v.id=s.latest_revision_id WHERE s.id=$1 AND s.project_id=$2 AND s.enabled AND s.retired_at IS NULL AND v.id=$3`, bound.ServerID, bound.ProjectID, tool.ServerRevisionID).Scan(&current, &keyID)
	if queryErr != nil || !reflect.DeepEqual(&current, tool.Delegation) {
		return nil, ErrRequired
	}
	material, readErr := s.Keys.Read(ctx, s.Pool, s.Installation, keyID, secrets.MCPDelegationSigning)
	if readErr != nil {
		return nil, ErrRequired
	}
	defer clear(material)
	key, keyErr := secrets.MCPPrivateKey(material)
	if keyErr != nil {
		return nil, ErrRequired
	}
	defer clear(key)
	fingerprint := sha256.Sum256(key.Public().(ed25519.PublicKey))
	now := time.Now()
	claims := delegatedClaims{RegisteredClaims: jwt.RegisteredClaims{Issuer: current.Issuer, Subject: string(bound.Identity.Kind) + ":" + bound.Identity.ID, Audience: jwt.ClaimStrings{current.Audience}, IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now.Add(-5 * time.Second)), ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Minute))}, ServerID: bound.ServerID, ProjectID: bound.ProjectID}
	token := jwt.Token{Method: jwt.SigningMethodEdDSA, Claims: claims, Header: map[string]any{"typ": "JWT", "alg": jwt.SigningMethodEdDSA.Alg(), "kid": hex.EncodeToString(fingerprint[:])}}
	encoded, signErr := token.SignedString(key)
	if signErr != nil {
		return nil, ErrRequired
	}
	return []byte(encoded), nil
}

func (s *Store) CheckGrant(ctx context.Context, authority access.Authority, tool routing.MCPToolsetTool) error {
	bound, bindErr := Bind(authority, tool)
	if bindErr != nil {
		return bindErr
	}
	if _, liveErr := liveBinding(ctx, s.Pool, bound); liveErr != nil {
		return liveErr
	}
	grant, queryErr := readGrant(ctx, s.Pool, bound)
	if queryErr != nil {
		return queryErr
	}
	if !grant.Expires.After(time.Now()) || grant.AttemptID != nil {
		return ErrRequired
	}
	return nil
}
