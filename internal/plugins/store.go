package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Usable returns the manifest and module of an installed plugin whose
// declared origins an owner approved. A plugin awaiting approval can't be
// used, so everything that pins or runs a plugin obtains it here.
func Usable(ctx context.Context, q access.Queryer, digest string) (abi.Manifest, []byte, error) {
	var manifest abi.Manifest
	var data, module []byte
	var approved bool
	err := q.QueryRow(ctx, "SELECT manifest, module, approved_at IS NOT NULL FROM olp.plugins WHERE digest=$1", digest).Scan(&data, &module, &approved)
	if errors.Is(err, pgx.ErrNoRows) {
		return manifest, nil, refuse(CodeNotInstalled, "No plugin with this digest is installed.")
	}
	if err != nil {
		return manifest, nil, err
	}
	if !approved {
		return manifest, nil, refuse(CodeNotApproved, "An owner has not approved the origins this plugin declares.")
	}
	return manifest, module, json.Unmarshal(data, &manifest)
}

const selectPlugin = `SELECT p.digest, p.abi_version, octet_length(p.module), p.manifest, p.etag::text,
	p.installed_by::text, i.email, p.installed_at, p.approved_by::text, a.email, p.approved_at
	FROM olp.plugins p JOIN olp.users i ON i.id = p.installed_by LEFT JOIN olp.users a ON a.id = p.approved_by`

func loadPlugin(ctx context.Context, q access.Queryer, digest string) (contract.Plugin, error) {
	return scanPlugin(q.QueryRow(ctx, selectPlugin+" WHERE p.digest=$1", digest))
}

func scanPlugin(row pgx.Row) (contract.Plugin, error) {
	var p contract.Plugin
	var manifest []byte
	var etag, installedBy string
	var approvedBy, approvedByEmail *string
	var approvedAt *time.Time
	err := row.Scan(&p.Digest, &p.AbiVersion, &p.SizeBytes, &manifest, &etag, &installedBy, &p.InstalledByEmail, &p.InstalledAt, &approvedBy, &approvedByEmail, &approvedAt)
	if err != nil {
		return p, err
	}
	p.Etag, p.InstalledBy = uuid.MustParse(etag), uuid.MustParse(installedBy)
	p.ApprovedBy, p.ApprovedByEmail, p.ApprovedAt = nullable.NewNullNullable[uuid.UUID](), nullable.NewNullNullable[string](), nullable.NewNullNullable[time.Time]()
	if approvedAt != nil {
		p.ApprovedBy.Set(uuid.MustParse(*approvedBy))
		p.ApprovedByEmail.Set(*approvedByEmail)
		p.ApprovedAt.Set(*approvedAt)
	}
	return p, json.Unmarshal(manifest, &p.Manifest)
}
