// Package operationregistry is the trusted composition root for unary operation
// contracts. Runtime requests can select registrations, never install codecs.
package operationregistry

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/classification"
	"github.com/tyk-swe/olp/internal/operations/embeddings"
	"github.com/tyk-swe/olp/internal/operations/rerank"
	"github.com/tyk-swe/olp/internal/operations/tokenization"
)

var Default = builtins()

func builtins() *operations.Registry {
	r := operations.NewRegistry()
	for _, definitions := range [][]operations.Dialect{embeddings.Definitions(), rerank.Definitions(), classification.Definitions(), tokenization.Definitions()} {
		for _, d := range definitions {
			if err := r.Register(d); err != nil {
				panic(err)
			}
		}
	}
	for _, m := range mappings() {
		if err := r.RegisterMapping(m); err != nil {
			panic(err)
		}
	}
	return r
}

// Lookup preserves already published profile dialect labels while resolving
// their codec identity. A hosting label never substitutes a generation codec.
func Lookup(id string) (operations.Dialect, bool) {
	switch id {
	case "direct-openai/moderation", "direct-compatible/moderation", "azure-deployment/moderation", "azure-v1/moderation", "azure-responses-legacy/moderation":
		id = "openai-moderation"
	}
	return Default.Lookup(id, operations.Revision)
}
func Identity(id string) oif.Identity { return oif.Identity{ID: id, Revision: operations.Revision} }
