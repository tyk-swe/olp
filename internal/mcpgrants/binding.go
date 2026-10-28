package mcpgrants

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

type IdentityKind string

const (
	KeyIdentity     IdentityKind = "api_key"
	EndUserIdentity IdentityKind = "end_user"
)

var ErrRequired = errors.New("a usable personal MCP grant is required")

type Identity struct {
	Kind             IdentityKind `json:"kind"`
	ID               string       `json:"id"`
	WorkloadIssuerID string       `json:"workload_issuer_id,omitempty"`
}

type Binding struct {
	ServerID  string           `json:"server_id"`
	ProjectID string           `json:"project_id"`
	Endpoint  string           `json:"endpoint"`
	OAuth     routing.MCPOAuth `json:"oauth"`
	Identity  Identity         `json:"identity"`
}

func identityOf(authority access.Authority) (Identity, error) {
	if authority.WorkloadIssuerID != nil && authority.EndUserDigest != "" {
		return Identity{Kind: EndUserIdentity, ID: authority.EndUserDigest, WorkloadIssuerID: *authority.WorkloadIssuerID}, nil
	}
	if authority.ID == "" {
		return Identity{}, ErrRequired
	}
	return Identity{Kind: KeyIdentity, ID: authority.ID}, nil
}

func Bind(authority access.Authority, tool routing.MCPToolsetTool) (Binding, error) {
	if authority.ProjectID == nil || tool.OAuth == nil || !tool.OAuth.Valid() {
		return Binding{}, ErrRequired
	}
	identity, err := identityOf(authority)
	if err != nil {
		return Binding{}, err
	}
	return binding(tool.ServerID, *authority.ProjectID, tool.Endpoint, *tool.OAuth, identity), nil
}

func binding(serverID, projectID, endpoint string, config routing.MCPOAuth, identity Identity) Binding {
	return Binding{ServerID: serverID, ProjectID: projectID, Endpoint: endpoint, Identity: identity,
		OAuth: routing.MCPOAuth{ClientID: config.ClientID, Audience: config.Audience,
			Scopes:                slices.Compact(slices.Sorted(slices.Values(config.Scopes))),
			AuthorizationEndpoint: config.AuthorizationEndpoint, TokenEndpoint: config.TokenEndpoint,
			DeviceAuthorizationEndpoint: config.DeviceAuthorizationEndpoint}}
}

func (b Binding) Digest() string {
	encoded, _ := json.Marshal(b)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
