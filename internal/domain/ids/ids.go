package ids

import "github.com/google/uuid"

func New() string { return uuid.Must(uuid.NewV7()).String() }

func Canonical(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}

func CanonicalOrNil(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id.String() == raw
}
