package ids

import (
	"slices"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/problem"
)

func New() string { return uuid.Must(uuid.NewV7()).String() }

func Canonical(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}

func CanonicalOrNil(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id.String() == raw
}

func Parse(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return "", problem.Invalid("id", "Use a valid UUID.")
	}
	return id.String(), nil
}

func ParseAll(values []string) ([]string, error) {
	type parsedUUID struct {
		value string
		err   error
	}
	parsed := slices.Collect(func(yield func(parsedUUID) bool) {
		for _, raw := range values {
			value, err := Parse(raw)
			if !yield(parsedUUID{value, err}) {
				return
			}
		}
	})
	for _, value := range parsed {
		if value.err != nil {
			return nil, value.err
		}
	}
	if values == nil {
		return nil, nil
	}
	return append([]string{}, slices.Collect(func(yield func(string) bool) {
		for _, value := range parsed {
			if !yield(value.value) {
				return
			}
		}
	})...), nil
}

func ParseOptional(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	canonical, err := Parse(*value)
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

// ParseProject canonicalises an optional project_id, so project lookups,
// ownership comparisons and idempotency fingerprints see one spelling.
func ParseProject(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	id, err := uuid.Parse(*value)
	if err != nil {
		return nil, problem.Invalid("project_id", "Use a valid UUID.")
	}
	canonical := id.String()
	return &canonical, nil
}
