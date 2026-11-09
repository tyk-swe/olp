package code

import (
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/promotion"
)

func mappedValues[V, W any](values map[string]V, convert func(V) W) map[string]W {
	return maps.Collect(func(yield func(string, W) bool) {
		for key, item := range values {
			if !yield(key, convert(item)) {
				return
			}
		}
	})
}

func tryMappedValues[V, W any](values map[string]V, convert func(string, V) (W, error)) (map[string]W, error) {
	keys := slices.Sorted(maps.Keys(values))
	converted, err := promotion.TryMap(keys, func(key string) (W, error) { return convert(key, values[key]) })
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, W) bool) {
		for i, key := range keys {
			if !yield(key, converted[i]) {
				return
			}
		}
	}), nil
}
