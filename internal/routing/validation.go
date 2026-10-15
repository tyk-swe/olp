package routing

// ValidationError reports an invalid routing control. HTTP boundaries choose its representation.
type ValidationError struct{ Field, Message string }

func (e *ValidationError) Error() string { return e.Message }
func invalidValue(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

func (e *ValidationError) RoutingValidation() (string, string) { return e.Field, e.Message }
