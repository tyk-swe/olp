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

// An Installed plugin is one Usable admits, with its code.
type Installed struct {
	Manifest abi.Manifest
	// Module is a confined plugin's WebAssembly module.
	Module []byte
	// Executable names an unconfined plugin's executable in the unconfined
	// plugin directory.
	Executable string
}

// Usable returns an installed plugin that may be used: a confined plugin
// whose declared origins an owner approved or, where the deployment enables
// the unconfined tier, an unconfined plugin an owner permitted. unconfined is
// the tier, or nil. Everything that runs a plugin obtains it here, and
// everything that pins one obtains its profile from Profile.
func Usable(ctx context.Context, q access.Queryer, unconfined *Unconfined, digest string) (Installed, error) {
	return usable(ctx, q, unconfined, digest, true)
}

// Profile returns the profile id that a usable plugin declares, for a
// provider to pin with the plugin's digest.
func Profile(ctx context.Context, q access.Queryer, unconfined *Unconfined, digest, id string) (*connectors.PluginProfile, error) {
	installed, err := usable(ctx, q, unconfined, digest, false)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(installed.Manifest.Profiles, func(p abi.Profile) bool { return p.ID == id }) {
		return nil, refuse(CodeProfileUnknown, "The plugin declares no profile with this ID.")
	}
	return pluginProfile(digest, installed.Manifest, installed.Executable != "", id)
}

// Profiles returns the profiles of every usable plugin, which providers may
// pin, newest build of each plugin first.
func Profiles(ctx context.Context, q access.Queryer, unconfined *Unconfined) ([]connectors.Profile, error) {
	rows, err := q.Query(ctx, "SELECT digest, manifest, executable IS NOT NULL FROM olp.plugins WHERE approved_at IS NOT NULL AND ($1 OR executable IS NULL) ORDER BY manifest->>'name', installed_at DESC, digest", unconfined != nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := []connectors.Profile{}
	for rows.Next() {
		var digest string
		var manifest abi.Manifest
		var isUnconfined bool
		if err = rows.Scan(&digest, &manifest, &isUnconfined); err != nil {
			return nil, err
		}
		for _, declared := range manifest.Profiles {
			profile, err := pluginProfile(digest, manifest, isUnconfined, declared.ID)
			if err != nil {
				return nil, err
			}
			profiles = append(profiles, profile.Profile())
		}
	}
	return profiles, rows.Err()
}

func pluginProfile(digest string, manifest abi.Manifest, unconfined bool, id string) (*connectors.PluginProfile, error) {
	if unconfined {
		return connectors.NewUnconfinedPluginProfile(digest, manifest, id)
	}
	return connectors.NewPluginProfile(digest, manifest, id)
}

// usable reads a plugin that Usable admits, with a confined plugin's module
// only when withModule is set.
func usable(ctx context.Context, q access.Queryer, unconfined *Unconfined, digest string, withModule bool) (Installed, error) {
	var installed Installed
	var manifest []byte
	var approved bool
	var executable *string
	err := q.QueryRow(ctx, "SELECT manifest, approved_at IS NOT NULL, executable, CASE WHEN $2 THEN module END FROM olp.plugins WHERE digest=$1", digest, withModule).
		Scan(&manifest, &approved, &executable, &installed.Module)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return installed, refuse(CodeNotInstalled, "No plugin with this digest is installed.")
	case err != nil:
		return installed, err
	case executable != nil && unconfined == nil:
		return installed, refuse(CodeUnconfinedDisabled, "The plugin is unconfined, and this deployment does not enable unconfined plugins.")
	case !approved:
		return installed, refuse(CodeNotApproved, "An owner has not approved the origins this plugin declares.")
	}
	if executable != nil {
		installed.Executable = *executable
	}
	return installed, json.Unmarshal(manifest, &installed.Manifest)
}

const selectPlugin = `SELECT p.digest, p.abi_version, p.size_bytes, p.executable, p.manifest, p.etag::text,
	p.installed_by::text, i.email, p.installed_at, p.approved_by::text, a.email, p.approved_at
	FROM olp.plugins p JOIN olp.users i ON i.id = p.installed_by LEFT JOIN olp.users a ON a.id = p.approved_by`

func loadPlugin(ctx context.Context, q access.Queryer, digest string) (contract.Plugin, error) {
	return scanPlugin(q.QueryRow(ctx, selectPlugin+" WHERE p.digest=$1", digest))
}

func scanPlugin(row pgx.Row) (contract.Plugin, error) {
	var p contract.Plugin
	var manifest []byte
	var etag, installedBy string
	var executable, approvedBy, approvedByEmail *string
	var approvedAt *time.Time
	err := row.Scan(&p.Digest, &p.AbiVersion, &p.SizeBytes, &executable, &manifest, &etag, &installedBy, &p.InstalledByEmail, &p.InstalledAt, &approvedBy, &approvedByEmail, &approvedAt)
	if err != nil {
		return p, err
	}
	p.Executable = nullable.NewNullNullable[string]()
	if executable != nil {
		p.Executable.Set(*executable)
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
