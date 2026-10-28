package mcpservers

import (
	"context"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

// Save stores a certified catalog in an immutable revision under the caller's
// serialized transaction. No upstream IO occurs while holding these locks.
func Save(ctx context.Context, tx pgx.Tx, actor string, current, next *Definition) error {
	if next.Catalog == nil {
		return access.Invalid("catalog", "Certify a bounded tool catalog first.")
	}
	names := make(map[string]bool, len(next.Catalog.Tools))
	for _, tool := range next.Catalog.Tools {
		names[tool.Name] = true
	}
	for name, price := range next.ToolPrices {
		if !names[name] || !routing.ValidToolPrice(price) {
			return access.Invalid("tool_prices", "Use certified tool names with non-negative decimal prices.")
		}
	}
	if next.UnitPrice != nil && !routing.ValidToolPrice(*next.UnitPrice) {
		return access.Invalid("unit_price", "Use a non-negative decimal price with at most 12 integer and 12 fractional digits, or null for no price.")
	}
	if next.ToolPrices == nil {
		next.ToolPrices = map[string]string{}
	}
	effective := routing.EffectiveToolLimits(next.CallsPerMinute, next.MaxConcurrency, next.PerKeyCallsPerMinute, next.PerKeyMaxConcurrency)
	next.CallsPerMinute, next.MaxConcurrency = effective.CallsPerMinute, effective.MaxConcurrency
	next.PerKeyCallsPerMinute, next.PerKeyMaxConcurrency = effective.PerKeyCallsPerMinute, effective.PerKeyMaxConcurrency
	if !effective.WithinInstallation() {
		return access.Invalid("calls_per_minute", "Stay inside the installation tool call limits.")
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
		_, err = tx.Exec(ctx, "INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,credential_id,latest_revision_id,etag,created_by,calls_per_minute,max_concurrency,per_key_calls_per_minute,per_key_max_concurrency,delegation_key_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)", next.ID, next.ProjectID, next.Name, next.Transport, next.Endpoint, next.Enabled, next.CredentialID, next.RevisionID, next.ETag, actor, next.CallsPerMinute, next.MaxConcurrency, next.PerKeyCallsPerMinute, next.PerKeyMaxConcurrency, next.DelegationKeyID)
	} else {
		if current.Transport != next.Transport || !reflect.DeepEqual(current.Stdio, next.Stdio) {
			return access.Invalid("transport", "The registered transport and executable binding are immutable.")
		}
		if current.ProjectID != next.ProjectID || current.RetiredAt != nil {
			return access.Invalid("project_id", "The registered project is immutable.")
		}
		next.ID, next.Revision = current.ID, current.Revision+1
		_, err = tx.Exec(ctx, "UPDATE olp.mcp_servers SET name=$2,endpoint=$3,enabled=$4,credential_id=$5,latest_revision_id=$6,etag=$7,calls_per_minute=$8,max_concurrency=$9,per_key_calls_per_minute=$10,per_key_max_concurrency=$11,delegation_key_id=$12 WHERE id=$1", next.ID, next.Name, next.Endpoint, next.Enabled, next.CredentialID, next.RevisionID, next.ETag, next.CallsPerMinute, next.MaxConcurrency, next.PerKeyCallsPerMinute, next.PerKeyMaxConcurrency, next.DelegationKeyID)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,created_by,unit_price,tool_prices,oauth,delegation,stdio) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::numeric,$10,$11,$12,$13)", next.RevisionID, next.ID, next.Revision, next.Endpoint, next.Catalog.Protocol, next.Catalog.Tools, next.Catalog.Digest, actor, next.UnitPrice, next.ToolPrices, next.OAuth, next.Delegation, next.Stdio)
	return err
}

func UpdateLimits(ctx context.Context, tx pgx.Tx, id string, limits routing.ToolLimits, etag string) error {
	if !limits.WithinInstallation() {
		return access.Invalid("calls_per_minute", "Stay inside the installation tool call limits.")
	}
	_, err := tx.Exec(ctx, "UPDATE olp.mcp_servers SET calls_per_minute=$2,max_concurrency=$3,per_key_calls_per_minute=$4,per_key_max_concurrency=$5,etag=$6 WHERE id=$1", id, limits.CallsPerMinute, limits.MaxConcurrency, limits.PerKeyCallsPerMinute, limits.PerKeyMaxConcurrency, etag)
	return err
}
