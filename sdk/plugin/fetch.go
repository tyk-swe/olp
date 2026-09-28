package plugin

import (
	"context"
	"encoding/json"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// HTTPRequest is a request a plugin sends with Fetch, or carries for OLP.
type HTTPRequest = abi.HTTPRequest

// HTTPResponse is the response Fetch returns.
type HTTPResponse = abi.HTTPResponse

// Fetch sends an HTTP request through OLP, the only way a confined plugin
// reaches the network. OLP grants it to grant enrollment steps and grant
// refresh, and sends the request only to the plugin's approved origins, over
// the provider's network path and egress policy, following no redirect. A
// request OLP refuses or cannot complete fails with an *Error, such as one
// with code abi.CodeOriginNotApproved; any response the upstream sent,
// whatever its status, is returned. Pass the context of the call the request
// serves: OLP grants the capability by call, and an unconfined plugin serves
// calls concurrently.
func Fetch(ctx context.Context, request HTTPRequest) (HTTPResponse, error) {
	var response HTTPResponse
	result, err := callHost(ctx, abi.CapabilityHTTP, request)
	if err != nil {
		return response, err
	}
	return response, json.Unmarshal(result, &response)
}
