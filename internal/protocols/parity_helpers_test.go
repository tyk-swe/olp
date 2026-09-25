package protocols_test

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/protocols"
)

// object and raw mirror the package-internal fixtures helpers; external parity
// tests keep the same call shape while staying outside the package boundary.
func object(data []byte) (protocols.Object, error) {
	var f protocols.Object
	err := json.Unmarshal(data, &f)
	return f, err
}

func raw(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
