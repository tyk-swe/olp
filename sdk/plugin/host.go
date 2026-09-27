//go:build !wasip1

package plugin

import "github.com/tyk-swe/olp/sdk/plugin/abi"

// hostCall answers every capability request as unavailable outside OLP's
// WASM runtime, so a plugin's own tests can run natively.
func hostCall([]byte) []byte {
	return respond(nil, &abi.Error{Code: "unavailable", Message: "OLP capabilities are available only inside OLP."})
}
