// Package operationregistry is the trusted composition root for unary operation
// contracts. Runtime requests can select registrations, never install codecs.
package operationregistry

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/classification"
	"github.com/tyk-swe/olp/internal/operations/decision"
	"github.com/tyk-swe/olp/internal/operations/embeddings"
	"github.com/tyk-swe/olp/internal/operations/ocr"
	"github.com/tyk-swe/olp/internal/operations/rerank"
	"github.com/tyk-swe/olp/internal/operations/search"
	"github.com/tyk-swe/olp/internal/operations/tokenization"
)

var Default = builtins()

func builtins() *operations.Registry {
	r := operations.NewRegistry()
	for _, definitions := range [][]operations.Dialect{embeddings.Definitions(), rerank.Definitions(), classification.Definitions(), tokenization.Definitions(), decision.Definitions(), ocr.Definitions(), search.Definitions()} {
		for _, d := range definitions {
			if err := r.Register(d); err != nil {
				panic(err)
			}
		}
	}
	for _, m := range append(append(mappings(), decision.Mappings()...), search.Mappings()...) {
		if err := r.RegisterMapping(m); err != nil {
			panic(err)
		}
	}
	return r
}

// TransformedServing reports whether the operation serves on strict and
// transformed routes, with no caching and unary inspector parsing.
func TransformedServing(op string) bool { return op == "decision" || op == "ocr" || op == "search" }

// UnboundedWork reports whether the operation cannot bound its provider work
// before dispatch, so token and non-exempt cost caps refuse it.
func UnboundedWork(op string) bool { return op == "decision" || op == "ocr" }

// Lookup resolves the canonical dialect declared by a provider profile.
func Lookup(id string) (operations.Dialect, bool) { return Default.Lookup(id, operations.Revision) }
func Identity(id string) oif.Identity             { return oif.Identity{ID: id, Revision: operations.Revision} }
