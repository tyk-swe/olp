package protocols

import (
	"encoding/json"
	"github.com/tyk-swe/olp/internal/oif"
)

// Capture parsing refuses duplicate keys, invalid Unicode and malformed native
// field types instead of silently omitting an uninspectable selected field.
func validCaptureJSON(raw []byte) bool {
	_, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 8 << 20, MaxDepth: 64, MaxNodes: 65536})
	return err == nil
}

type captureReader struct{ invalid bool }

func (r *captureReader) object(raw json.RawMessage) (Object, error) {
	value, err := object(raw)
	if err != nil && present(raw) {
		r.invalid = true
	}
	return value, err
}
func (r *captureReader) array(raw json.RawMessage) []json.RawMessage {
	var value []json.RawMessage
	if present(raw) && json.Unmarshal(raw, &value) != nil {
		r.invalid = true
	}
	return value
}
func (r *captureReader) str(raw json.RawMessage) string {
	var value string
	if present(raw) && json.Unmarshal(raw, &value) != nil {
		r.invalid = true
	}
	return value
}
