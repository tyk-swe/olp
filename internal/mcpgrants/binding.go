package mcpgrants

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
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
	ServerID          string           `json:"server_id,omitempty"`
	AgentID           string           `json:"agent_id,omitempty"`
	EntraCredentialID string           `json:"entra_credential_id,omitempty"`
	ProjectID         string           `json:"project_id"`
	Endpoint          string           `json:"endpoint"`
	OAuth             routing.MCPOAuth `json:"oauth"`
	Identity          Identity         `json:"identity"`
}

func BindAgent(authority access.Authority, publicationID string, agent agentcontract.Definition) (Binding, error) {
	if authority.ProjectID == nil || publicationID == "" || agent.OAuth == nil || !agent.OAuth.Valid() || (agent.OAuth.EntraOBO == nil && agent.CredentialID != nil) || (agent.OAuth.EntraOBO != nil && agent.CredentialID == nil) {
		return Binding{}, ErrRequired
	}
	identity, err := identityOf(authority)
	if err != nil {
		return Binding{}, err
	}
	return agentBinding(publicationID, *authority.ProjectID, agent, identity), nil
}

func agentBinding(publicationID, projectID string, agent agentcontract.Definition, identity Identity) Binding {
	bound := binding("", projectID, agent.Endpoint, *agent.OAuth, identity)
	credential := func() string {
		if agent.OAuth.EntraOBO == nil || agent.CredentialID == nil {
			return ""
		}
		return *agent.CredentialID
	}()
	return Binding{ServerID: bound.ServerID, AgentID: publicationID, EntraCredentialID: credential, ProjectID: bound.ProjectID, Endpoint: bound.Endpoint, OAuth: bound.OAuth, Identity: bound.Identity}
}

func (b Binding) accessPurpose() secrets.SealPurpose {
	if b.AgentID != "" {
		return secrets.AgentCredential
	}
	return secrets.MCPGrantAccess
}

func (b Binding) refreshPurpose() secrets.SealPurpose {
	if b.AgentID != "" {
		return secrets.AgentCredential
	}
	return secrets.MCPGrantRefresh
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
		OAuth: routing.MCPOAuth{EntraOBO: config.EntraOBO.Clone(), ClientID: config.ClientID, Audience: config.Audience,
			Scopes:                slices.Compact(slices.Sorted(slices.Values(config.Scopes))),
			AuthorizationEndpoint: config.AuthorizationEndpoint, TokenEndpoint: config.TokenEndpoint,
			DeviceAuthorizationEndpoint: config.DeviceAuthorizationEndpoint}}
}

func (b Binding) Digest() string {
	encoded, _ := json.Marshal(b)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
