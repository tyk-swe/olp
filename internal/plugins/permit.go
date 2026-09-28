package plugins

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// permitPurpose is the recent authentication permitting an unconfined plugin
// takes.
const permitPurpose = "plugin_permit"

// unconfinedTier returns the deployment's unconfined tier, or the problem
// every unconfined plugin path answers where the deployment enables none.
func (s *Management) unconfinedTier() (*Unconfined, error) {
	if s.Unconfined == nil {
		return nil, access.Fail(http.StatusNotFound, "unconfined_plugins_disabled", "This deployment does not enable unconfined plugins. Only a deployment setting enables them.")
	}
	return s.Unconfined, nil
}

// executables lists the executables in the unconfined plugin directory, which
// only an owner sees.
func (s *Management) executables(r *http.Request, _ access.Principal) (access.Reply, error) {
	tier, err := s.unconfinedTier()
	if err != nil {
		return access.Reply{}, err
	}
	files, err := tier.Executables()
	if err != nil {
		return access.Reply{}, access.Fail(http.StatusServiceUnavailable, "unconfined_plugin_dir_unreadable", "OLP can't read the unconfined plugin directory the deployment names: "+err.Error())
	}
	items := make([]contract.UnconfinedExecutable, 0, len(files))
	for _, file := range files {
		permitted, err := permitted(r.Context(), s.Access.Pool, file.Digest)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, contract.UnconfinedExecutable{Name: file.Name, Digest: file.Digest, SizeBytes: file.Size, Permitted: permitted})
	}
	return access.OK(contract.UnconfinedExecutableListResponse{Items: items}), nil
}

// review runs an executable to read its manifest, for an owner to review.
func (s *Management) review(r *http.Request, _ access.Principal) (access.Reply, error) {
	tier, err := s.unconfinedTier()
	if err != nil {
		return access.Reply{}, err
	}
	file, manifest, err := tier.Inspect(r.Context(), r.PathValue("executable"))
	if err != nil {
		return access.Reply{}, problem(err)
	}
	permitted, err := permitted(r.Context(), s.Access.Pool, file.Digest)
	if err != nil {
		return access.Reply{}, err
	}
	var declared contract.PluginManifest
	if err = remarshal(manifest, &declared); err != nil {
		return access.Reply{}, err
	}
	return access.OK(contract.UnconfinedExecutableReview{Name: file.Name, Digest: file.Digest, SizeBytes: file.Size, Permitted: permitted, AbiVersion: abi.Version, Manifest: declared}), nil
}

// permit makes an executable an unconfined plugin, pinnable by its digest.
// It runs with native privileges, so the owner acknowledges that and has
// reauthenticated for it.
func (s *Management) permit(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input contract.UnconfinedPluginPermitRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	// The route authorizes before running anything.
	tier, err := s.unconfinedTier()
	if err != nil {
		return access.Reply{}, err
	}
	if !digestPattern.MatchString(input.Digest) {
		return access.Reply{}, access.Invalid("digest", "Name the executable by the lowercase hexadecimal SHA-256 digest you reviewed.")
	}
	if !input.AcknowledgeRisk {
		return access.Reply{}, access.Invalid("acknowledge_risk", "Acknowledge that an unconfined plugin runs with the privileges of OLP's processes, outside every confinement.")
	}
	file, manifest, err := tier.Inspect(r.Context(), r.PathValue("executable"))
	if err != nil {
		return access.Reply{}, problem(err)
	}
	if file.Digest != input.Digest {
		return access.Reply{}, access.Fail(http.StatusConflict, CodeExecutableChanged, "The executable changed since you reviewed it. Review it again.")
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.ConsumeRecent(r, tx, p, permitPurpose, ""); err != nil {
		return access.Reply{}, err
	}
	var installed, count int
	if err = tx.QueryRow(r.Context(), "SELECT count(*) FILTER (WHERE digest=$1), count(*) FROM olp.plugins", file.Digest).Scan(&installed, &count); err != nil {
		return access.Reply{}, err
	}
	if installed > 0 {
		return access.Reply{}, access.Fail(http.StatusConflict, "plugin_permitted", "An owner already permitted this build of the executable.")
	}
	if count >= maxInstalled {
		return access.Reply{}, access.Fail(http.StatusUnprocessableEntity, "plugin_limit", "An installation holds at most 64 plugin digests. Uninstall one first.")
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp.plugins(digest,abi_version,manifest,executable,size_bytes,etag,installed_by,approved_by,approved_at) VALUES($1,$2,$3,$4,$5,$6,$7,$7,now())",
		file.Digest, abi.Version, data, file.Name, file.Size, access.NewID(), p.ID); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "plugin.permit", "plugin", file.Digest, "success"); err != nil {
		return access.Reply{}, err
	}
	plugin, err := loadPlugin(r.Context(), tx, file.Digest)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: http.StatusCreated, Body: plugin, ETag: plugin.Etag.String(), Location: "/api/v1/plugins/" + file.Digest})
}

// permitted reports whether an owner permitted the executable with digest.
func permitted(ctx context.Context, q access.Queryer, digest string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.plugins WHERE digest=$1 AND executable IS NOT NULL)", digest).Scan(&exists)
	return exists, err
}

// remarshal converts a value to its contract type through JSON.
func remarshal(value, into any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}
