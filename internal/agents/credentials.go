package agents

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
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

func (s *Server) credential(ctx context.Context, tx pgx.Tx, current *Publication, d *Draft, raw json.RawMessage) error {
	if d.Agent == nil {
		if len(raw) != 0 {
			return access.Invalid("credential", "Only agents accept credentials.")
		}
		return nil
	}
	if len(raw) == 0 {
		if d.Agent.CredentialID == nil && current != nil && current.Draft.Agent != nil {
			d.Agent.CredentialID = current.Draft.Agent.CredentialID
		}
		return nil
	}
	d.Agent.CredentialID = nil
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	material, err := mcpservers.Credential(raw)
	if err != nil {
		return err
	}
	defer clear(material)
	id, err := StoreCredential(ctx, tx, s.Access.Keys, s.Access.Installation, d.ProjectID, material)
	if err != nil {
		return err
	}
	d.Agent.CredentialID = &id
	return nil
}
