package ptr

func Or[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

func Value[T any](p *T) T {
	if p == nil {
		return *new(T)
	}
	return *p
}

func Clone[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

func NonNil[S ~[]E, E any](values S) S {
	if values == nil {
		return S{}
	}
	return values
}

func NonNilMap[M ~map[K]V, K comparable, V any](values M) M {
	if values == nil {
		return M{}
	}
	return values
}
