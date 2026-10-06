package plugins

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/pluginindex"
)

// index lists the reviewed plugins with whether each reviewed digest is
// installed and approved here. It installs nothing: the digest of a module an
// owner uploads is what ties it to an index entry.
func (s *Management) index(r *http.Request, _ access.Principal) (access.Reply, error) {
	if s.Index == nil {
		return access.Reply{}, access.Fail(503, "plugin_index_unavailable", "This process holds no verified plugin index.")
	}
	var digests []string
	for _, plugin := range s.Index.Index.Plugins {
		for _, release := range plugin.Releases {
			digests = append(digests, release.Digest)
		}
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT digest, approved_at IS NOT NULL FROM olp.plugins WHERE digest = ANY($1)", digests)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	// approved holds each installed digest, true once its origins are approved.
	approved := map[string]bool{}
	for rows.Next() {
		var digest string
		var isApproved bool
		if err := rows.Scan(&digest, &isApproved); err != nil {
			return access.Reply{}, err
		}
		approved[digest] = isApproved
	}
	if err := rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.OK(indexResponse(s.Index, approved)), nil
}

func indexResponse(signed *pluginindex.Signed, approved map[string]bool) contract.PluginIndexResponse {
	response := contract.PluginIndexResponse{PublishedAt: signed.Index.PublishedAt, Sha256: signed.SHA256, KeyId: signed.KeyID, Items: []contract.PluginIndexEntry{}}
	for _, plugin := range signed.Index.Plugins {
		entry := contract.PluginIndexEntry{Name: plugin.Name, Description: plugin.Description, Maintainer: plugin.Maintainer, DocumentationUrl: plugin.DocumentationURL,
			Repository: plugin.Repository, Path: plugin.Path, Releases: []contract.PluginIndexRelease{}}
		for _, release := range plugin.Releases {
			isApproved, installed := approved[release.Digest]
			entry.Releases = append(entry.Releases, contract.PluginIndexRelease{Version: release.Version, Digest: release.Digest, AbiVersion: int32(release.ABIVersion), SizeBytes: release.SizeBytes,
				Origins: release.Origins, Profiles: release.Profiles, Commit: release.Commit, ReviewedAt: release.ReviewedAt, Installed: installed, Approved: isApproved})
		}
		response.Items = append(response.Items, entry)
	}
	return response
}
