package provider

import (
	"maps"
	"strconv"
)

type path string

func (p path) field(name string) string {
	if p == "" {
		return name
	}
	return string(p) + "." + name
}

func (p path) index(i int) path { return path(string(p) + "." + strconv.Itoa(i)) }

func (p path) at(name string, i int) path { return path(p.field(name) + "." + strconv.Itoa(i)) }

func firstIndex[T any, K comparable](items []T, key func(T) K) map[K]int {
	return maps.Collect(func(yield func(K, int) bool) {
		for i := len(items) - 1; i >= 0; i-- {
			if !yield(key(items[i]), i) {
				return
			}
		}
	})
}

func firstRepeat[T any, K comparable](items []T, key func(T) K) (T, bool) {
	first := firstIndex(items, key)
	for i, item := range items {
		if first[key(item)] != i {
			return item, true
		}
	}
	var none T
	return none, false
}

func each[T, U any](items []T, convert func(int, T) (U, error)) ([]U, error) {
	converted := make([]U, 0, len(items))
	for i, item := range items {
		value, err := convert(i, item)
		if err != nil {
			return nil, err
		}
		converted = append(converted, value)
	}
	return converted, nil
}

func mapSlice[T, U any](items []T, convert func(int, T) U) []U {
	converted := make([]U, 0, len(items))
	for i, item := range items {
		converted = append(converted, convert(i, item))
	}
	return converted
}

func sum[T any](items []T, measure func(T) int) int {
	total := 0
	for _, item := range items {
		total += measure(item)
	}
	return total
}

func when[T any](condition bool, value T) []T {
	if !condition {
		return nil
	}
	return []T{value}
}

func optional[T any](value T, present bool) *T {
	if !present {
		return nil
	}
	return &value
}

func orEmpty[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
