package connectors

import (
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/plugin/abi"
)

const forcedStreaming oif.Origin = "hosting_streaming"

func parseForceStreaming(declared abi.Profile) (bool, error) {
	if !declared.Hosting.ForceStreaming {
		return false, nil
	}
	if !aggregated(declared.Dialect) {
		dialects := slices.DeleteFunc(PluginDialects(), func(dialect string) bool { return !aggregated(dialect) })
		return false, &ProfileError{Field: "hosting.force_streaming", Message: "Force streaming only in a dialect whose streams OLP aggregates: " + strings.Join(dialects, ", ") + "."}
	}
	return true, nil
}

func aggregated(dialect string) bool {
	family, ok := generationFamily(dialect)
	return ok && openai.Aggregates(family)
}

func (c Config) ForcesStreaming() bool {
	return c.Plugin != nil && c.Plugin.hosting.forceStreaming
}

func (c Config) StreamRequest(prepared oif.Prepared) (oif.Prepared, error) {
	if stream, _ := prepared.Document().Lookup("/stream"); !c.ForcesStreaming() || stream.Raw() == "true" {
		return prepared, nil
	}
	change := oif.Change{Pointer: "/stream", Value: "true", Origin: forcedStreaming, Reason: "forced upstream streaming of the plugin profile's hosting adaptation"}
	document, err := oif.Apply(prepared.Document(), []oif.Change{change})
	if err != nil {
		return oif.Prepared{}, err
	}
	streamed, err := oif.PrepareDestination(prepared.Request(), prepared.Descriptor(), document, forcedStreaming, "forced upstream streaming of plugin profile "+c.ProfileID)
	if err != nil {
		return oif.Prepared{}, err
	}
	return streamed.WithProvenance(prepared.Provenance()...).WithProvenance(oif.Provenance{Pointer: change.Pointer, Origin: change.Origin, Reason: change.Reason}), nil
}
