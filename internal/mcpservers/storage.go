package mcpservers

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

// Save stores a certified catalog in an immutable revision under the caller's
// serialized transaction. No upstream IO occurs while holding these locks.
func Save(ctx context.Context, tx pgx.Tx, actor string, current, next *Definition) error {
	if next.Catalog == nil {
		return access.Invalid("catalog", "Certify a bounded tool catalog first.")
	}
	next.ETag, next.RevisionID, next.Revision = access.NewID(), access.NewID(), 1
	var err error
	if current == nil {
		var active int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM olp.mcp_servers WHERE retired_at IS NULL").Scan(&active); err != nil {
			return err
		}
		if active >= 128 {
			return access.Fail(409, "mcp_server_limit", "This installation already has 128 active MCP servers.")
		}
		next.ID = access.NewID()
		_, err = tx.Exec(ctx, "INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,credential_id,latest_revision_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", next.ID, next.ProjectID, next.Name, next.Transport, next.Endpoint, next.Enabled, next.CredentialID, next.RevisionID, next.ETag, actor)
	} else {
		if current.ProjectID != next.ProjectID || current.RetiredAt != nil {
			return access.Invalid("project_id", "The registered project is immutable.")
		}
		next.ID, next.Revision = current.ID, current.Revision+1
		_, err = tx.Exec(ctx, "UPDATE olp.mcp_servers SET name=$2,endpoint=$3,enabled=$4,credential_id=$5,latest_revision_id=$6,etag=$7 WHERE id=$1", next.ID, next.Name, next.Endpoint, next.Enabled, next.CredentialID, next.RevisionID, next.ETag)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", next.RevisionID, next.ID, next.Revision, next.Endpoint, next.Catalog.Protocol, next.Catalog.Tools, next.Catalog.Digest, actor)
	return err
}
