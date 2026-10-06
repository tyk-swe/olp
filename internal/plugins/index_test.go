package plugins

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/pluginindex"
	"github.com/tyk-swe/olp/internal/testutil"
)

// TestIndexedPluginsDeclareTheirListedManifests builds each plugin the
// reviewed index lists from this repository and checks its newest release
// names the manifest the source declares. Digests depend on the toolchain, so
// the release workflow checks them; this keeps the review honest per change.
func TestIndexedPluginsDeclareTheirListedManifests(t *testing.T) {
	t.Parallel()
	signed, err := pluginindex.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	r := newTestRuntime(t, DefaultLimits, nil)
	for _, listed := range signed.Index.Plugins {
		manifest, err := r.Inspect(t.Context(), testutil.BuildPlugin(t, "./"+listed.Path))
		if err != nil {
			t.Fatalf("%s: %v", listed.Path, err)
		}
		newest := listed.Releases[0]
		var profiles []string
		for _, profile := range manifest.Profiles {
			profiles = append(profiles, profile.ID)
		}
		slices.Sort(profiles)
		if manifest.Name != listed.Name || manifest.Version != newest.Version || !slices.Equal(slices.Sorted(slices.Values(manifest.Origins)), newest.Origins) || !slices.Equal(profiles, newest.Profiles) {
			t.Fatalf("%s declares %s %s %v %v; the index lists %s %s %v %v", listed.Path, manifest.Name, manifest.Version, manifest.Origins, profiles, listed.Name, newest.Version, newest.Origins, newest.Profiles)
		}
	}
}
