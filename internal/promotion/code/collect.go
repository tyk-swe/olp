package code

import (
	"cmp"
	"maps"
	"slices"

	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/promotion"
)

func byID[T any](items []T, id func(T) string) []T {
	return slices.SortedFunc(slices.Values(items), func(a, b T) int { return cmp.Compare(id(a), id(b)) })
}

func present[T any](value nullable.Nullable[[]T]) []T {
	items, _ := promotion.Present(value)
	return items
}

func sortedKeys[V any](values map[string]V) []string {
	return append(make([]string, 0, len(values)), slices.Sorted(maps.Keys(values))...)
}

func orEmpty[K comparable, V any](values *map[K]V) map[K]V {
	if values == nil {
		return map[K]V{}
	}
	return *values
}

func mappedValues[V, W any](values map[string]V, convert func(V) W) map[string]W {
	return maps.Collect(func(yield func(string, W) bool) {
		for key, item := range values {
			if !yield(key, convert(item)) {
				return
			}
		}
	})
}

func keyed[V any](keys []string, values []V) map[string]V {
	return maps.Collect(func(yield func(string, V) bool) {
		for i, key := range keys {
			if !yield(key, values[i]) {
				return
			}
		}
	})
}
