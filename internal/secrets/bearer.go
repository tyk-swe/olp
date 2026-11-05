package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

func ParseBearer(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || len(value) < 1 || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return nil, errors.New("Supply bounded bearer material, or null to remove it.")
	}
	return []byte(value), nil
}
