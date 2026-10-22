package main

import (
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type adapter struct{ codeplans.Adapter }

func (a adapter) Manifest() abi.Manifest {
	manifest := a.Adapter.Manifest()
	manifest.Version = "0.2.0"
	profile := manifest.Profiles[0]
	profile.ID, profile.Label = "zai-next", "Z.ai migration fixture"
	manifest.Profiles = append(manifest.Profiles, profile)
	return manifest
}

func init() { plugin.Register(adapter{codeplans.ZAI()}) }

func main() { plugin.Serve() }
