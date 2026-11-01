package agents

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/secrets"
)

// StoreCredential stores only purpose-bound sealed material and project
// ownership. Published definitions contain its UUID, never its bearer value.
func StoreCredential(ctx context.Context, tx pgx.Tx, keys *secrets.KeyRing, installation, project string, material []byte) (string, error) {
	if keys == nil {
		return "", access.Fail(503, "keys_unavailable", "Credential storage is unavailable.")
	}
	id := access.NewID()
	if err := keys.Store(ctx, tx, installation, id, secrets.AgentCredential, material, nil); err != nil {
		return "", err
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.agent_credentials(id,project_id) VALUES($1,$2)`, id, project)
	return id, err
}

func StoreEntraCredential(ctx context.Context, tx pgx.Tx, keys *secrets.KeyRing, installation, project string, material []byte) (string, error) {
	id, err := StoreCredential(ctx, tx, keys, installation, project, material)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.agent_credentials SET entra_client=true WHERE id=$1`, id); err != nil {
		return "", err
	}
	return id, nil
}

// CredentialMatches lets declarative promotion retain an unchanged credential
// version. It never exposes material or compares credentials from another project.
func CredentialMatches(ctx context.Context, q access.Queryer, keys *secrets.KeyRing, installation, project, id string, material []byte) (bool, error) {
	if keys == nil {
		return false, access.Fail(503, "keys_unavailable", "Credential storage is unavailable.")
	}
	var owned bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.agent_credentials WHERE id=$1 AND project_id=$2)`, id, project).Scan(&owned); err != nil {
		return false, err
	}
	if !owned {
		return false, nil
	}
	current, err := keys.Read(ctx, q, installation, id, secrets.AgentCredential)
	if err != nil {
		return false, err
	}
	defer clear(current)
	return subtle.ConstantTimeCompare(current, material) == 1, nil
}

func (s *Server) credential(ctx context.Context, tx pgx.Tx, current *Publication, d Draft, raw json.RawMessage, entraSecret *string) (*string, error) {
	if d.Agent == nil {
		if len(raw) != 0 || entraSecret != nil {
			return nil, access.Invalid("credential", "Only agents accept credentials.")
		}
		return nil, nil
	}
	if d.Agent.OAuth != nil && d.Agent.OAuth.EntraOBO != nil {
		if len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, access.Invalid("credential", "Entra authorization does not use a static bearer credential.")
		}
		if entraSecret != nil {
			if len(*entraSecret) == 0 || len(*entraSecret) > 16384 {
				return nil, access.Invalid("entra_client_secret", "Supply a nonempty bounded client secret.")
			}
			material := []byte(*entraSecret)
			defer clear(material)
			id, err := StoreEntraCredential(ctx, tx, s.Access.Keys, s.Access.Installation, d.ProjectID, material)
			if err != nil {
				return nil, err
			}
			return &id, nil
		}
		if current != nil && current.Draft.Agent != nil && current.Draft.Agent.OAuth != nil && current.Draft.Agent.OAuth.EntraOBO != nil && current.Draft.Agent.CredentialID != nil && current.Draft.Agent.OAuth.ClientID == d.Agent.OAuth.ClientID && current.Draft.Agent.OAuth.TokenEndpoint == d.Agent.OAuth.TokenEndpoint && current.Draft.Agent.OAuth.EntraOBO.Issuer == d.Agent.OAuth.EntraOBO.Issuer {
			return current.Draft.Agent.CredentialID, nil
		}
		return nil, access.Invalid("entra_client_secret", "Supply this client's secret when enabling Entra or changing its issuer, client, or token endpoint.")
	}
	if entraSecret != nil {
		return nil, access.Invalid("entra_client_secret", "Configure Entra authorization before supplying a client secret.")
	}
	if len(raw) == 0 {
		if d.Agent.CredentialID == nil && current != nil && current.Draft.Agent != nil && (current.Draft.Agent.OAuth == nil || current.Draft.Agent.OAuth.EntraOBO == nil) {
			return current.Draft.Agent.CredentialID, nil
		}
		return d.Agent.CredentialID, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	material, err := mcpservers.Credential(raw)
	if err != nil {
		return nil, err
	}
	defer clear(material)
	id, err := StoreCredential(ctx, tx, s.Access.Keys, s.Access.Installation, d.ProjectID, material)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func draftWithCredential(d Draft, id *string) Draft {
	if d.Agent == nil {
		return d
	}
	agent := d.Agent
	return Draft{ProjectID: d.ProjectID, Name: d.Name, Toolset: d.Toolset, Prompt: d.Prompt, Agent: &agentcontract.Definition{
		Protocol: agent.Protocol, Endpoint: agent.Endpoint, CardDigest: agent.CardDigest, OAuth: agent.OAuth, CredentialID: id,
		ToolsetRevisions: agent.ToolsetRevisions, RouteRevisions: agent.RouteRevisions, PricingRevisionID: agent.PricingRevisionID,
		MaxCost: agent.MaxCost, MessagePrice: agent.MessagePrice, RemoteWorkBoundSeconds: agent.RemoteWorkBoundSeconds,
		MaxTaskContinuations: agent.MaxTaskContinuations, TaskDeadlineSeconds: agent.TaskDeadlineSeconds,
	}}
}
