package promotion

import (
	"context"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func ValueOf[T any](pointer *T) T {
	if pointer == nil {
		var zero T
		return zero
	}
	return *pointer
}

func NonZero[T comparable](value T) *T {
	var zero T
	if value == zero {
		return nil
	}
	return &value
}

func OptionalMap[K comparable, V any](values map[K]V) *map[K]V {
	if len(values) == 0 {
		return nil
	}
	return &values
}

func NonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

func When[T any](condition bool, value T) []T {
	if !condition {
		return []T{}
	}
	return []T{value}
}

func Map[T, U any](values []T, convert func(T) U) []U {
	return slices.AppendSeq(make([]U, 0, len(values)), func(yield func(U) bool) {
		for _, value := range values {
			if !yield(convert(value)) {
				return
			}
		}
	})
}

func MapIndex[T, U any](values []T, convert func(int, T) U) []U {
	return slices.AppendSeq(make([]U, 0, len(values)), func(yield func(U) bool) {
		for i, value := range values {
			if !yield(convert(i, value)) {
				return
			}
		}
	})
}

func Filter[T any](values []T, keep func(T) bool) []T {
	return slices.AppendSeq([]T{}, func(yield func(T) bool) {
		for _, value := range values {
			if keep(value) && !yield(value) {
				return
			}
		}
	})
}

func PointerIf[T any](value T, present bool) *T {
	if !present {
		return nil
	}
	return &value
}

func Optional[T any](values []T) *[]T {
	if len(values) == 0 {
		return nil
	}
	return &values
}

func TryMap[T, U any](values []T, convert func(T) (U, error)) ([]U, error) {
	return TryMapIndex(values, func(_ int, value T) (U, error) { return convert(value) })
}

func TryMapIndex[T, U any](values []T, convert func(int, T) (U, error)) ([]U, error) {
	type outcome struct {
		value U
		err   error
	}
	outcomes := slices.AppendSeq(make([]outcome, 0, len(values)), func(yield func(outcome) bool) {
		for i, value := range values {
			converted, err := convert(i, value)
			if !yield(outcome{value: converted, err: err}) || err != nil {
				return
			}
		}
	})
	if last := len(outcomes) - 1; last >= 0 && outcomes[last].err != nil {
		return nil, outcomes[last].err
	}
	return Map(outcomes, func(o outcome) U { return o.value }), nil
}

func Every[T any](values []T, check func(T) error) error {
	return EveryIndex(values, func(_ int, value T) error { return check(value) })
}

func EveryIndex[T any](values []T, check func(int, T) error) error {
	for i, value := range values {
		if err := check(i, value); err != nil {
			return err
		}
	}
	return nil
}

func Index[T any, K comparable](values []T, key func(T) K) map[K]T {
	return Associate(values, func(value T) (K, T) { return key(value), value })
}

func Associate[T any, K comparable, V any](values []T, entry func(T) (K, V)) map[K]V {
	return maps.Collect(func(yield func(K, V) bool) {
		for _, value := range values {
			if !yield(entry(value)) {
				return
			}
		}
	})
}

func FirstIndex[T any, K comparable](values []T, key func(T) K) map[K]int {
	return maps.Collect(func(yield func(K, int) bool) {
		for i, value := range slices.Backward(values) {
			if !yield(key(value), i) {
				return
			}
		}
	})
}

func FirstRepeat[T any, K comparable](values []T, key func(T) K) (T, bool) {
	first := FirstIndex(values, key)
	for i, value := range values {
		if first[key(value)] != i {
			return value, true
		}
	}
	return *new(T), false
}

func Rows[T any](ctx context.Context, q access.Queryer, scan pgx.RowToFunc[T], sql string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scan)
}
