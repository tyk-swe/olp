package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// loopbackClient is the provider network path of a test: the egress policy
// admits the loopback servers the tests start.
func loopbackClient(t *testing.T) *http.Client {
	t.Helper()
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	client, err := policy.ConnectionClient(nil, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fetch(t *testing.T, grant *HTTP, request abi.HTTPRequest) (abi.HTTPResponse, *abi.Error) {
	t.Helper()
	params, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var response abi.HTTPResponse
	data, failure := serveHTTP(context.WithValue(t.Context(), httpKey{}, grant), params)
	if failure == nil {
		if err = json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
	}
	return response, failure
}

// A redirect comes back to the plugin rather than leading it elsewhere.

// The provider's network path carries the egress policy, which here
// admits no loopback address.

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// Another sign-in's value is refused before the plugin reaches out.

// Without the HTTP capability, the plugin can't reach its authority.

// Out of band, the operator pastes code#state.

// The token response names no regional API, so the account uses the
// shared one.

// The authority spends a code once.

// Until the operator approves, the authority says to keep polling, and
// to slow down when polled again within its interval.

func reported(err error, code string) bool {
	reported, ok := errors.AsType[*abi.Error](err)
	return ok && reported.Code == code
}
