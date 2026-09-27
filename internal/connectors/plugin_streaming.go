package connectors

import (
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// A plugin profile that forces streaming places its dialect at an upstream
// that serves only streaming requests. OLP sends every request of the profile
// as a streaming one, and aggregates the stream into the dialect's
// non-streaming result for a caller that did not ask to stream
// (openai.Aggregate), so the caller, codecs and usage accounting see an
// ordinary non-streaming exchange.

// forcedStreaming is the provenance of the streaming a plugin profile forces
// on a prepared request.
const forcedStreaming oif.Origin = "hosting_streaming"

// parseForceStreaming reads a profile's forced streaming, which OLP can serve
// only for a dialect whose streams it aggregates.
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

// aggregated reports whether OLP aggregates the dialect's streams for a
// caller that did not ask to stream.
func aggregated(dialect string) bool {
	family, ok := generationFamily(dialect)
	return ok && openai.Aggregates(family)
}

// ForcesStreaming reports whether every request reaches the upstream as a
// streaming one: the connector's plugin profile forces streaming, so a
// non-streaming caller's result is aggregated from the upstream's stream.
func (c Config) ForcesStreaming() bool {
	return c.Plugin != nil && c.Plugin.hosting.forceStreaming
}

// StreamRequest returns a prepared dialect request as a plugin profile that
// forces streaming sends it: a request that does not ask for a stream does,
// and its provenance records the change. Every dialect OLP aggregates asks for
// a stream with its /stream member. Other requests are returned as they are.
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
