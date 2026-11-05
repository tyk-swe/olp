package registration

import (
	"context"
	"reflect"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/routing"
)

type serverSummary struct {
	Delegation           *routing.MCPDelegation   `json:"delegation"`
	Stdio                *routing.MCPStdioBinding `json:"stdio,omitempty"`
	OAuth                *routing.MCPOAuth        `json:"oauth"`
	ID                   string                   `json:"id"`
	ProjectID            string                   `json:"project_id"`
	Name                 string                   `json:"name"`
	Transport            string                   `json:"transport"`
	Endpoint             string                   `json:"endpoint"`
	Enabled              bool                     `json:"enabled"`
	HasCredential        bool                     `json:"has_credential"`
	ETag                 string                   `json:"etag"`
	RevisionID           string                   `json:"revision_id"`
	Revision             int64                    `json:"revision"`
	CertifiedAt          time.Time                `json:"certified_at"`
	RetiredAt            *time.Time               `json:"retired_at"`
	UnitPrice            *string                  `json:"unit_price"`
	ToolPrices           map[string]string        `json:"tool_prices"`
	CallsPerMinute       int64                    `json:"calls_per_minute"`
	MaxConcurrency       int64                    `json:"max_concurrency"`
	PerKeyCallsPerMinute int64                    `json:"per_key_calls_per_minute"`
	PerKeyMaxConcurrency int64                    `json:"per_key_max_concurrency"`
}

func summarize(d Definition) serverSummary {
	return serverSummary{
		Delegation: d.Delegation, Stdio: d.Stdio, OAuth: d.OAuth,
		ID: d.ID, ProjectID: d.ProjectID, Name: d.Name, Transport: d.Transport,
		Endpoint: d.Endpoint, Enabled: d.Enabled, HasCredential: d.HasCredential,
		ETag: d.ETag, RevisionID: d.RevisionID, Revision: d.Revision,
		CertifiedAt: d.CertifiedAt, RetiredAt: d.RetiredAt,
		UnitPrice: d.UnitPrice, ToolPrices: d.ToolPrices,
		CallsPerMinute: d.CallsPerMinute, MaxConcurrency: d.MaxConcurrency,
		PerKeyCallsPerMinute: d.PerKeyCallsPerMinute, PerKeyMaxConcurrency: d.PerKeyMaxConcurrency,
	}
}

func prepareRevision(current *Definition, candidate Definition) (Definition, error) {
	if candidate.Catalog == nil {
		return Definition{}, access.Invalid("catalog", "Certify a bounded tool catalog first.")
	}
	for name, price := range candidate.ToolPrices {
		if !slices.ContainsFunc(candidate.Catalog.Tools, func(tool routing.MCPTool) bool { return tool.Name == name }) || !routing.ValidToolPrice(price) {
			return Definition{}, access.Invalid("tool_prices", "Use certified tool names with non-negative decimal prices.")
		}
	}
	if candidate.UnitPrice != nil && !routing.ValidToolPrice(*candidate.UnitPrice) {
		return Definition{}, access.Invalid("unit_price", "Use a non-negative decimal price with at most 12 integer and 12 fractional digits, or null for no price.")
	}

	effective := routing.EffectiveToolLimits(candidate.CallsPerMinute, candidate.MaxConcurrency, candidate.PerKeyCallsPerMinute, candidate.PerKeyMaxConcurrency)
	if !effective.WithinInstallation() {
		return Definition{}, access.Invalid("calls_per_minute", "Stay inside the installation tool call limits.")
	}
	if current != nil && (current.Transport != candidate.Transport || !reflect.DeepEqual(current.Stdio, candidate.Stdio)) {
		return Definition{}, access.Invalid("transport", "The registered transport and executable binding are immutable.")
	}
	if current != nil && (current.ProjectID != candidate.ProjectID || current.RetiredAt != nil) {
		return Definition{}, access.Invalid("project_id", "The registered project is immutable.")
	}
	id, revision := func() (string, int64) {
		if current != nil {
			return current.ID, current.Revision + 1
		}
		return access.NewID(), 1
	}()
	return Definition{
		ID: id, ETag: access.NewID(), RevisionID: access.NewID(), Revision: revision,
		ProjectID: candidate.ProjectID, Name: candidate.Name, Endpoint: candidate.Endpoint, Transport: candidate.Transport,
		Enabled: candidate.Enabled, HasCredential: candidate.CredentialID != nil, CredentialID: candidate.CredentialID,
		Catalog: candidate.Catalog, CertifiedAt: candidate.CertifiedAt, RetiredAt: candidate.RetiredAt,
		Stdio: candidate.Stdio.Clone(), OAuth: candidate.OAuth.Clone(), Delegation: candidate.Delegation.Clone(),
		EntraClientSecretID: candidate.EntraClientSecretID, DelegationKeyID: candidate.DelegationKeyID,
		UnitPrice: clonePrice(candidate.UnitPrice), ToolPrices: cloneToolPrices(candidate.ToolPrices),
		CallsPerMinute: effective.CallsPerMinute, MaxConcurrency: effective.MaxConcurrency,
		PerKeyCallsPerMinute: effective.PerKeyCallsPerMinute, PerKeyMaxConcurrency: effective.PerKeyMaxConcurrency,
	}, nil
}

func Save(ctx context.Context, tx pgx.Tx, actor string, current *Definition, candidate Definition) (Definition, error) {
	next, err := prepareRevision(current, candidate)
	if err != nil {
		return Definition{}, err
	}
	if err := persistServer(ctx, tx, actor, current, next); err != nil {
		return Definition{}, err
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.mcp_server_revisions(id,server_id,revision,endpoint,protocol_version,tools,digest,created_by,unit_price,tool_prices,oauth,delegation,stdio) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::numeric,$10,$11,$12,$13)", next.RevisionID, next.ID, next.Revision, next.Endpoint, next.Catalog.Protocol, next.Catalog.Tools, next.Catalog.Digest, actor, next.UnitPrice, next.ToolPrices, next.OAuth, next.Delegation, next.Stdio)
	if err != nil {
		return Definition{}, err
	}
	return next, nil
}

func persistServer(ctx context.Context, tx pgx.Tx, actor string, current *Definition, next Definition) error {
	if current != nil {
		_, err := tx.Exec(ctx, "UPDATE olp.mcp_servers SET name=$2,endpoint=$3,enabled=$4,credential_id=$5,latest_revision_id=$6,etag=$7,calls_per_minute=$8,max_concurrency=$9,per_key_calls_per_minute=$10,per_key_max_concurrency=$11,delegation_key_id=$12,entra_client_secret_id=$13 WHERE id=$1", next.ID, next.Name, next.Endpoint, next.Enabled, next.CredentialID, next.RevisionID, next.ETag, next.CallsPerMinute, next.MaxConcurrency, next.PerKeyCallsPerMinute, next.PerKeyMaxConcurrency, next.DelegationKeyID, next.EntraClientSecretID)
		return err
	}
	var active int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM olp.mcp_servers WHERE retired_at IS NULL").Scan(&active); err != nil {
		return err
	}
	if active >= 128 {
		return access.Fail(409, "mcp_server_limit", "This installation already has 128 active MCP servers.")
	}
	_, err := tx.Exec(ctx, "INSERT INTO olp.mcp_servers(id,project_id,name,transport,endpoint,enabled,credential_id,latest_revision_id,etag,created_by,calls_per_minute,max_concurrency,per_key_calls_per_minute,per_key_max_concurrency,delegation_key_id,entra_client_secret_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)", next.ID, next.ProjectID, next.Name, next.Transport, next.Endpoint, next.Enabled, next.CredentialID, next.RevisionID, next.ETag, actor, next.CallsPerMinute, next.MaxConcurrency, next.PerKeyCallsPerMinute, next.PerKeyMaxConcurrency, next.DelegationKeyID, next.EntraClientSecretID)
	return err
}

const columns = "s.id::text,s.project_id::text,s.name,s.transport,s.endpoint,s.enabled,s.credential_id,s.etag::text,s.retired_at,v.id::text,v.revision,v.protocol_version,v.tools,v.digest,v.certified_at,v.unit_price::text,v.tool_prices,s.calls_per_minute,s.max_concurrency,s.per_key_calls_per_minute,s.per_key_max_concurrency,v.oauth,v.stdio,v.delegation,s.delegation_key_id::text,s.entra_client_secret_id::text"
const joined = " FROM olp.mcp_servers s JOIN olp.mcp_server_revisions v ON v.id=s.latest_revision_id"

func scan(row pgx.Row) (*Definition, error) {
	d := &Definition{Catalog: &upstream.Catalog{}}
	err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Transport, &d.Endpoint, &d.Enabled, &d.CredentialID, &d.ETag, &d.RetiredAt, &d.RevisionID, &d.Revision, &d.Catalog.Protocol, &d.Catalog.Tools, &d.Catalog.Digest, &d.CertifiedAt, &d.UnitPrice, &d.ToolPrices, &d.CallsPerMinute, &d.MaxConcurrency, &d.PerKeyCallsPerMinute, &d.PerKeyMaxConcurrency, &d.OAuth, &d.Stdio, &d.Delegation, &d.DelegationKeyID, &d.EntraClientSecretID)
	d.HasCredential = d.CredentialID != nil
	if d.ToolPrices == nil {
		d.ToolPrices = map[string]string{}
	}
	return d, err
}

func Load(ctx context.Context, q access.Queryer, id string, lock bool) (*Definition, error) {
	query := "SELECT " + columns + joined + " WHERE s.id=$1"
	if lock {
		query += " FOR UPDATE OF s"
	}
	return scan(q.QueryRow(ctx, query, id))
}

func FindActive(ctx context.Context, q access.Queryer, project, name string) (*Definition, error) {
	return scan(q.QueryRow(ctx, "SELECT "+columns+joined+" WHERE s.project_id=$1 AND lower(s.name)=lower($2) AND s.retired_at IS NULL", project, name))
}

func UpdateLimits(ctx context.Context, tx pgx.Tx, id string, limits routing.ToolLimits, etag string) error {
	if !limits.WithinInstallation() {
		return access.Invalid("calls_per_minute", "Stay inside the installation tool call limits.")
	}
	_, err := tx.Exec(ctx, "UPDATE olp.mcp_servers SET calls_per_minute=$2,max_concurrency=$3,per_key_calls_per_minute=$4,per_key_max_concurrency=$5,etag=$6 WHERE id=$1", id, limits.CallsPerMinute, limits.MaxConcurrency, limits.PerKeyCallsPerMinute, limits.PerKeyMaxConcurrency, etag)
	return err
}
