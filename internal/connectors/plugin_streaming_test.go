package connectors

import (
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// streamingManifest declares a Responses profile whose upstream serves only
// streaming requests.
func streamingManifest() abi.Manifest {
	manifest := pluginManifest()
	manifest.Profiles[0].Dialect = "openai-responses"
	manifest.Profiles[0].Hosting.ForceStreaming = true
	return manifest
}
