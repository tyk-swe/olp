package operationregistry

import (
	"slices"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
	"github.com/tyk-swe/olp/internal/domain/operations/classification"
	"github.com/tyk-swe/olp/internal/domain/operations/decision"
	"github.com/tyk-swe/olp/internal/domain/operations/embeddings"
	"github.com/tyk-swe/olp/internal/domain/operations/ocr"
	"github.com/tyk-swe/olp/internal/domain/operations/rerank"
	"github.com/tyk-swe/olp/internal/domain/operations/search"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization"
)

var catalog, catalogError = builtins()

func Catalog() operations.Registry { return catalog }

func Validate() error { return catalogError }

func builtins() (operations.Registry, error) {
	dialects := slices.Concat(embeddings.Definitions(), rerank.Definitions(), classification.Definitions(), tokenization.Definitions(), decision.Definitions(), ocr.Definitions(), search.Definitions())
	return operations.NewRegistry(dialects, slices.Concat(mappings(), decision.Mappings(), search.Mappings(), ocr.Mappings()))
}

type Policy struct {
	TransformedUnary bool
	CacheEligible    bool
	BoundedWork      bool
}

func PolicyFor(operation string) Policy {
	switch operation {
	case "decision", "ocr":
		return Policy{TransformedUnary: true}
	case "search":
		return Policy{TransformedUnary: true, BoundedWork: true}
	default:
		return Policy{CacheEligible: true, BoundedWork: true}
	}
}

func Lookup(id string) (operations.Dialect, bool) { return catalog.Lookup(id, operations.Revision) }
func identity(id string) oif.Identity             { return oif.Identity{ID: id, Revision: operations.Revision} }
