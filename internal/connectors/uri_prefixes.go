package connectors

import (
	"errors"
	"slices"
)

func NormalizeTuningDatasetPrefixes(kind string, prefixes []string) ([]string, error) {
	return normalizeVertexPrefixes(kind, prefixes, "dataset")
}

func NormalizeDocumentURIPrefixes(kind string, prefixes []string) ([]string, error) {
	return normalizeVertexPrefixes(kind, prefixes, "document")
}

func normalizeVertexPrefixes(kind string, prefixes []string, subject string) ([]string, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	if kind != KindVertexAI || len(prefixes) > 32 {
		return nil, errors.New("use at most 32 " + subject + " URI prefixes on a Vertex connection")
	}
	normalized := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		var err error
		if normalized[i], err = normalizeBatchStoragePrefix(prefix, "gs"); err != nil {
			return nil, err
		}
	}
	slices.Sort(normalized)
	return slices.Compact(normalized), nil
}

func (c Config) AllowsTuningDataset(uri string) bool {
	return underAnyPrefix(c.TuningDatasetURIPrefixes, uri)
}

func (c Config) AllowsDocumentURI(uri string) bool {
	return underAnyPrefix(c.DocumentURIPrefixes, uri)
}

func underAnyPrefix(prefixes []string, uri string) bool {
	return slices.ContainsFunc(prefixes, func(prefix string) bool { return (BatchStorage{}).Contains(prefix, uri) })
}
