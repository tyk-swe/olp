// Package surface classifies public request paths. Admission, the gateway's
// error shapes and credential locations, and the console fallback all read
// this one table, so an inference path can neither be admitted as management
// traffic nor answered by the console.
package surface

import (
	"slices"
	"strings"
)

// Surface is the API a public request speaks.
type Surface struct {
	// Name selects the error envelope and credential locations: openai,
	// anthropic, gemini, bedrock, or management.
	Name string
	// Inference is true for model traffic, which takes the inference pool.
	Inference bool
}

// Management is every public request that is not inference: the management
// API, the console, and paths nothing serves.
var Management = Surface{Name: "management"}

// Prefix is a public path prefix the console must never answer.
type Prefix struct {
	Path    string
	Surface Surface
	// GatewayCatchAll marks prefixes whose unknown paths the gateway answers
	// itself wherever it is mounted.
	GatewayCatchAll bool
}

var prefixes = []Prefix{
	{Path: "/v1/", Surface: Surface{Name: "openai", Inference: true}, GatewayCatchAll: true},
	{Path: "/native/", Surface: Surface{Name: "openai", Inference: true}},
	{Path: "/anthropic/", Surface: Surface{Name: "anthropic", Inference: true}},
	{Path: "/gemini/", Surface: Surface{Name: "gemini", Inference: true}},
	// Gemini Live clients connect to the root WebSocket path.
	{Path: "/ws/", Surface: Surface{Name: "gemini", Inference: true}},
	{Path: "/bedrock/", Surface: Surface{Name: "bedrock", Inference: true}, GatewayCatchAll: true},
	{Path: "/api/", Surface: Management},
	{Path: "/v1beta/", Surface: Management},
	{Path: "/openai/", Surface: Management},
	{Path: "/health/", Surface: Management},
	{Path: "/metrics", Surface: Management},
}

// Of classifies a request path.
func Of(path string) Surface {
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix.Path) {
			return prefix.Surface
		}
	}
	return Management
}

// Reserved lists every prefix the console must never answer.
func Reserved() []Prefix { return slices.Clone(prefixes) }
