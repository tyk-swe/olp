package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Usable returns the manifest and module of an installed plugin whose
// declared origins an owner approved. A plugin awaiting approval can't be
// used, so everything that runs a plugin obtains it here, and everything that
// pins one obtains its profile from Profile.
func Usable(ctx context.Context, q access.Queryer, digest string) (abi.Manifest, []byte, error) {
	var module []byte
	manifest, err := usable(ctx, q, digest, &module)
	return manifest, module, err
}

// Profile returns the profile id that a usable plugin declares, for a
// provider to pin with the plugin's digest.
func Profile(ctx context.Context, q access.Queryer, digest, id string) (*connectors.PluginProfile, error) {
	manifest, err := usable(ctx, q, digest, nil)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(manifest.Profiles, func(p abi.Profile) bool { return p.ID == id }) {
		return nil, refuse(CodeProfileUnknown, "The plugin declares no profile with this ID.")
	}
	return connectors.NewPluginProfile(digest, manifest, id)
}

// Profiles returns the profiles of every usable plugin, which providers may
// pin, newest build of each plugin first.
func Profiles(ctx context.Context, q access.Queryer) ([]connectors.Profile, error) {
	rows, err := q.Query(ctx, "SELECT digest, manifest FROM olp.plugins WHERE approved_at IS NOT NULL ORDER BY manifest->>'name', installed_at DESC, digest")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := []connectors.Profile{}
	for rows.Next() {
		var digest string
		var manifest abi.Manifest
		if err = rows.Scan(&digest, &manifest); err != nil {
			return nil, err
		}
		for _, declared := range manifest.Profiles {
			profile, err := connectors.NewPluginProfile(digest, manifest, declared.ID)
			if err != nil {
				return nil, err
			}
			profiles = append(profiles, profile.Profile())
		}
	}
	return profiles, rows.Err()
}

// usable reads the manifest of a plugin that Usable admits, and its module
// when module is not nil.
func usable(ctx context.Context, q access.Queryer, digest string, module *[]byte) (abi.Manifest, error) {
	var manifest abi.Manifest
	var data, unread []byte
	var approved bool
	withModule := module != nil
	if !withModule {
		module = &unread
	}
	err := q.QueryRow(ctx, "SELECT manifest, approved_at IS NOT NULL, CASE WHEN $2 THEN module END FROM olp.plugins WHERE digest=$1", digest, withModule).Scan(&data, &approved, module)
	if errors.Is(err, pgx.ErrNoRows) {
		return manifest, refuse(CodeNotInstalled, "No plugin with this digest is installed.")
	}
	if err != nil {
		return manifest, err
	}
	if !approved {
		return manifest, refuse(CodeNotApproved, "An owner has not approved the origins this plugin declares.")
	}
	return manifest, json.Unmarshal(data, &manifest)
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
