package authority

type ValidationError struct {
	Field  string
	Detail string
}

func (e *ValidationError) Error() string { return "validation_failed" }

func invalid(field, detail string) error {
	return &ValidationError{Field: field, Detail: detail}
}
