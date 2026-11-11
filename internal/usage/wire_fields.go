package usage

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
)

func missingField(field string) error {
	return fmt.Errorf("request metadata field %s is missing", field)
}

func requiredField[T any](field string, value *T) (T, error) {
	if value == nil {
		var zero T
		return zero, missingField(field)
	}
	return *value, nil
}

func requiredUUID(field string, value *string) (string, error) {
	text, err := requiredField(field, value)
	if err != nil {
		return "", err
	}
	parsed, err := uuid.Parse(text)
	if err != nil {
		return "", fmt.Errorf("request metadata field %s is not a uuid", field)
	}
	return parsed.String(), nil
}

func optionalUUID(field string, value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := requiredUUID(field, value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func requiredLabel(field string, value *string, allowed map[string]struct{}) (string, error) {
	label, err := requiredField(field, value)
	if err != nil {
		return "", err
	}
	if _, ok := allowed[label]; !ok {
		return "", fmt.Errorf("request metadata field %s has unknown value", field)
	}
	return label, nil
}

func optionalStatus(value *uint16) *int {
	if value == nil {
		return nil
	}
	status := int(*value)
	return &status
}

func requiredMilliseconds(field string, value *uint64) (int64, error) {
	raw, err := requiredField(field, value)
	if err != nil {
		return 0, err
	}
	if raw > math.MaxInt64 {
		return 0, fmt.Errorf("request metadata field %s is out of range", field)
	}
	return int64(raw), nil
}

func optionalMilliseconds(field string, value *uint64) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	milliseconds, err := requiredMilliseconds(field, value)
	if err != nil {
		return nil, err
	}
	return &milliseconds, nil
}

func optionalDecimal(field string, value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	if _, err := parseWireDecimal(*value); err != nil {
		return nil, fmt.Errorf("request metadata field %s is not a decimal: %w", field, err)
	}
	return value, nil
}

// parseWireDecimal accepts the canonical decimal text media units travel as and
// reports whether it is below zero. The bounds are those of the numeric(24,6)
// column the value lands in: a wider value would abort the persisting
// transaction and the delivery would then be retried forever.
func parseWireDecimal(value string) (bool, error) {
	digits := value
	negative := false
	if strings.HasPrefix(digits, "+") || strings.HasPrefix(digits, "-") {
		negative = digits[0] == '-'
		digits = digits[1:]
	}
	integer, fraction, hasFraction := strings.Cut(digits, ".")
	if integer == "" || (hasFraction && fraction == "") {
		return false, errors.New("empty decimal")
	}
	if len(integer) > 18 || len(fraction) > 6 {
		return false, errors.New("decimal is out of range")
	}
	nonZero := false
	for _, part := range [2]string{integer, fraction} {
		for index := 0; index < len(part); index++ {
			if part[index] < '0' || part[index] > '9' {
				return false, errors.New("decimal has a non-digit")
			}
			if part[index] != '0' {
				nonZero = true
			}
		}
	}
	return negative && nonZero, nil
}
