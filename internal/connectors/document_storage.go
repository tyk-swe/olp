package connectors

import (
	"errors"
	"slices"
)

func NormalizeDocumentURIPrefixes(kind string, prefixes []string) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	if kind != "vertex_ai" || len(prefixes) > 32 {
		return nil, errors.New("use at most 32 document URI prefixes on a Vertex connection")
	}
	out := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		normalized, err := normalizeBatchStoragePrefix(prefix, "gs")
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, normalized) {
			out = append(out, normalized)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (c Config) AllowsDocumentURI(uri string) bool {
	for _, prefix := range c.DocumentURIPrefixes {
		if (BatchStorage{}).Contains(prefix, uri) {
			return true
		}
	}
	return false
}
