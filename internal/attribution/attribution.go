package attribution

import (
	"encoding/json"
	"regexp"
)

const (
	Header            = "X-OLP-Attribution"
	HeaderBytes       = 4096
	MaxKeys           = 4
	MaxBytes          = 1024
	KeyMax            = 8
	DelegationPairKey = "delegation_pair"
)

var (
	keyPattern   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,31}$`)
	valuePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

func ValidKey(key string) bool     { return keyPattern.MatchString(key) }
func ValidValue(value string) bool { return valuePattern.MatchString(value) }

type Error struct{ Reason string }

func (e *Error) Error() string { return e.Reason }

func Validate(attribution map[string]string) error {
	if len(attribution) > MaxKeys {
		return &Error{Reason: "too_many_keys"}
	}
	for key, value := range attribution {
		if key == DelegationPairKey || !ValidKey(key) {
			return &Error{Reason: "invalid_key"}
		}
		if !ValidValue(value) {
			return &Error{Reason: "invalid_value"}
		}
	}
	data, err := json.Marshal(attribution)
	if err != nil {
		return &Error{Reason: "invalid_encoding"}
	}
	if len(data) > MaxBytes {
		return &Error{Reason: "too_large"}
	}
	return nil
}

func JSON(attribution map[string]string) []byte {
	if len(attribution) == 0 {
		return []byte("{}")
	}
	data, err := json.Marshal(attribution)
	if err != nil {
		return []byte("{}")
	}
	return data
}
