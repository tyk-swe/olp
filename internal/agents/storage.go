package agents

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Draft = agentcontract.Draft
type Publication = agentcontract.Publication
type Revision = agentcontract.Revision

func Load(ctx context.Context, q access.Queryer, id, kind string, lock bool) (*Publication, error) {
	query := `SELECT id::text,kind,project_id::text,name,etag::text,draft,latest_revision_id::text,retired FROM olp.agent_publications WHERE id=$1 AND kind=$2`
	if lock {
		query += " FOR UPDATE"
	}
	p := &Publication{}
	err := q.QueryRow(ctx, query, id, kind).Scan(&p.ID, &p.Kind, &p.ProjectID, &p.Name, &p.ETag, &p.Draft, &p.RevisionID, &p.Retired)
	return p, err
}

func validateEndpoint(d Draft, policy *egress.Policy) error {
	if d.Agent == nil {
		return nil
	}
	if policy == nil {
		return access.Fail(503, "egress_policy_unavailable", "Agent registration requires an egress policy.")
	}
	u, err := policy.ValidateEndpoint(d.Agent.Endpoint)
	if err != nil || u.String() != d.Agent.Endpoint {
		return access.Invalid("endpoint", "Use a canonical endpoint permitted by provider egress policy.")
	}
	return nil
}

func SaveDraft(ctx context.Context, tx pgx.Tx, actor string, current *Publication, next *Publication, policy *egress.Policy) error {
	next.Draft = agentcontract.NormalizeDraft(next.Draft)
	if err := next.Draft.Validate(next.Kind); err != nil {
		return access.Invalid("draft", err.Error())
	}
	if err := validateEndpoint(next.Draft, policy); err != nil {
		return err
	}
	next.ProjectID, next.Name, next.ETag = next.Draft.ProjectID, next.Draft.Name, access.NewID()
	if current == nil {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM olp.agent_publications WHERE project_id=$1 AND NOT retired`, next.ProjectID).Scan(&count); err != nil {
			return err
		}
		if count >= 256 {
			return access.Invalid("name", "This project has reached its publication limit.")
		}
		next.ID = access.NewID()
		_, err := tx.Exec(ctx, `INSERT INTO olp.agent_publications(id,project_id,kind,name,draft,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)`, next.ID, next.ProjectID, next.Kind, next.Name, next.Draft, next.ETag, actor)
		return err
	}
	if current.ProjectID != next.ProjectID || current.Kind != next.Kind || current.Retired {
		return access.Fail(409, "publication_immutable", "Ownership and retired history cannot be changed.")
	}
	if current.Draft.Agent != nil && next.Draft.Agent != nil && current.Draft.Agent.Endpoint != next.Draft.Agent.Endpoint && current.Draft.Agent.CredentialID != nil && next.Draft.Agent.CredentialID != nil && *current.Draft.Agent.CredentialID == *next.Draft.Agent.CredentialID {
		return access.Invalid("credential_id", "Replace or remove the credential when changing the endpoint.")
	}
	next.ID, next.RevisionID = current.ID, current.RevisionID
	_, err := tx.Exec(ctx, `UPDATE olp.agent_publications SET name=$2,draft=$3,etag=$4 WHERE id=$1`, next.ID, next.Name, next.Draft, next.ETag)
	return err
}

// ValidateReferences rechecks immutable pins and live registration ownership
// inside the publication transaction. It never contacts a provider or MCP peer.
func ValidateReferences(ctx context.Context, tx access.Queryer, kind string, d Draft) error {
	if err := d.Validate(kind); err != nil {
		return access.Invalid("draft", err.Error())
	}
	if d.Toolset != nil {
		for _, tool := range d.Toolset.Tools {
			var tools []mcpservers.Tool
			var digest string
			err := tx.QueryRow(ctx, `SELECT v.tools,v.digest FROM olp.mcp_server_revisions v JOIN olp.mcp_servers s ON s.id=v.server_id WHERE v.id=$1 AND s.id=$2 AND s.project_id=$3 AND s.enabled AND s.retired_at IS NULL AND s.latest_revision_id=v.id FOR SHARE OF s`, tool.RevisionID, tool.ServerID, d.ProjectID).Scan(&tools, &digest)
			if err != nil || digest != tool.CatalogDigest {
				return access.Invalid("toolset", "A pinned server revision is unavailable in this project.")
			}
			found := false
			for _, t := range tools {
				if t.Name == tool.Name {
					found = true
					break
				}
			}
			if !found {
				return access.Invalid("toolset", "A selected tool is not in its pinned catalog.")
			}
		}
	}
	if d.Agent != nil {
		for _, id := range d.Agent.ToolsetRevisions {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.agent_publication_revisions v JOIN olp.agent_publications p ON p.id=v.publication_id WHERE v.id=$1 AND p.project_id=$2 AND v.kind='toolset' AND NOT p.retired)`, id, d.ProjectID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return access.Invalid("toolset_revisions", "Use published toolsets in this project.")
			}
		}
		for _, id := range d.Agent.RouteRevisions {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.route_revisions v JOIN olp.routes r ON r.id=v.route_id WHERE v.id=$1 AND r.project_id=$2 AND r.retired_at IS NULL)`, id, d.ProjectID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return access.Invalid("route_revisions", "Use published route revisions in this project.")
			}
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.pricing_revisions WHERE id=$1)`, d.Agent.PricingRevisionID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return access.Invalid("pricing_revision_id", "Use an installed immutable pricing revision.")
		}
		if d.Agent.CredentialID != nil {
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.secrets s JOIN olp.agent_credentials c ON c.id=s.id WHERE s.id=$1 AND s.purpose=$2 AND c.project_id=$3)`, *d.Agent.CredentialID, secrets.AgentCredential, d.ProjectID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return access.Invalid("credential_id", "The agent credential is unavailable.")
			}
		}
	}
	return nil
}

func Publish(ctx context.Context, tx pgx.Tx, actor string, p *Publication) error {
	p.Draft = agentcontract.NormalizeDraft(p.Draft)
	if p.Retired {
		return access.Fail(409, "publication_retired", "Retired publications cannot be promoted.")
	}
	if err := ValidateReferences(ctx, tx, p.Kind, p.Draft); err != nil {
		return err
	}
	if err := p.Draft.Validate(p.Kind); err != nil {
		return access.Invalid("draft", err.Error())
	}
	digest := agentcontract.DraftDigest(p.Draft)
	id := access.NewID()
	var existing string
	err := tx.QueryRow(ctx, `SELECT id::text FROM olp.agent_publication_revisions WHERE publication_id=$1 AND digest=$2`, p.ID, digest).Scan(&existing)
	if err == nil {
		p.RevisionID = &existing
		p.ETag = access.NewID()
		_, err = tx.Exec(ctx, `UPDATE olp.agent_publications SET latest_revision_id=$2,etag=$3 WHERE id=$1`, p.ID, existing, p.ETag)
		return err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM olp.agent_publication_revisions WHERE publication_id=$1`, p.ID).Scan(&count); err != nil {
		return err
	}
	if count >= 32 {
		return access.Invalid("revision", "This publication has reached its immutable revision limit.")
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.agent_publication_revisions(id,publication_id,project_id,kind,definition,digest,created_by) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(publication_id,digest) DO NOTHING RETURNING id::text`, id, p.ID, p.ProjectID, p.Kind, p.Draft, digest, actor).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id::text FROM olp.agent_publication_revisions WHERE publication_id=$1 AND digest=$2`, p.ID, digest).Scan(&id)
	}
	if err != nil {
		return err
	}
	p.RevisionID = &id
	p.ETag = access.NewID()
	_, err = tx.Exec(ctx, `UPDATE olp.agent_publications SET latest_revision_id=$2,etag=$3 WHERE id=$1`, p.ID, id, p.ETag)
	return err
}

func LoadRevision(ctx context.Context, q access.Queryer, publication, id string) (Revision, error) {
	var v Revision
	err := q.QueryRow(ctx, `SELECT v.id::text,v.publication_id::text,v.project_id::text,v.kind,v.digest,v.definition,COALESCE(p.latest_revision_id=v.id,false),COALESCE((SELECT array_agg(label ORDER BY label) FROM olp.prompt_labels WHERE publication_id=p.id AND revision_id=v.id),'{}'::text[]) FROM olp.agent_publication_revisions v JOIN olp.agent_publications p ON p.id=v.publication_id WHERE v.publication_id=$1 AND v.id=$2`, publication, id).Scan(&v.ID, &v.PublicationID, &v.ProjectID, &v.Kind, &v.Digest, &v.Definition, &v.Current, &v.Labels)
	return v, err
}

func SetLabel(ctx context.Context, tx pgx.Tx, p *Publication, label, revision, etag string) error {
	if p.Kind != "prompt" || p.Retired || (label != "staging" && label != "production") {
		return access.Invalid("label", "Use production or staging on an active prompt.")
	}
	if _, err := LoadRevision(ctx, tx, p.ID, revision); err != nil {
		return access.Invalid("revision_id", "Use a revision of this prompt.")
	}
	_, err := tx.Exec(ctx, `INSERT INTO olp.prompt_labels(publication_id,label,revision_id,etag) VALUES($1,$2,$3,$4) ON CONFLICT(publication_id,label) DO UPDATE SET revision_id=EXCLUDED.revision_id,etag=EXCLUDED.etag`, p.ID, label, revision, etag)
	return err
}

func mapConflict(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Fail(404, "not_found", "The requested resource was not found.")
	}
	return err
}
