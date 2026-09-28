package connectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// An unconfined plugin may carry its profiles' upstream traffic itself: OLP
// places, authenticates and signs each request as for any plugin profile, then
// hands the finished request to the plugin instead of its own transport, and
// reads the response and its stream back. The plugin sees all caller content,
// so such a profile serves only transformed routes.

// pluginTransport is the transport of a plugin profile whose plugin carries
// its traffic.
const pluginTransport = "plugin"

// A Carrier runs the unconfined plugins that carry their profiles' upstream
// traffic, which it finds by the digest of the plugin's executable.
type Carrier interface {
	// Carry hands the plugin with digest one finished upstream request for
	// provider, which the plugin receives as its call's provider, and returns
	// the upstream's response as the plugin streams it back. A failure wraps
	// ErrNotSent when the request never reached the upstream; after any other
	// failure, the upstream's outcome is unknown. Ending ctx or closing the
	// response's body cancels the call. The plugin's output never reveals
	// secrets.
	Carry(ctx context.Context, digest string, provider abi.Provider, req *http.Request, secrets []string) (*http.Response, error)
}

// ErrNotSent marks a failed request that never reached the upstream, so
// another attempt can't repeat its work.
var ErrNotSent = errors.New("the request was not sent")

// CarriedByPlugin reports whether the connector's plugin carries its upstream
// traffic instead of OLP's transport.
func (c Config) CarriedByPlugin() bool {
	return c.Plugin != nil && c.Plugin.declared.CarriesTraffic
}

// CarrierClient returns, for a connector whose plugin carries its traffic, the
// client that sends its finished upstream requests through the plugin, which
// carrier runs, redacting secrets from what the plugin logs and reports. The
// client returns a redirect the upstream answers with rather than following
// it. Its failures wrap ErrNotSent as the carrier's do.
func (c Config) CarrierClient(carrier Carrier, secrets []string) *http.Client {
	return &http.Client{
		Transport: carried{carrier: carrier, digest: c.Plugin.profile.Revision, provider: abi.Provider{Profile: c.Plugin.profile.ID, Options: c.PluginOptions}, secrets: secrets},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// carried is the transport of requests a plugin carries.
type carried struct {
	carrier  Carrier
	digest   string
	provider abi.Provider
	secrets  []string
}

func (c carried) RoundTrip(req *http.Request) (*http.Response, error) {
	if c.carrier == nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, fmt.Errorf("%w: this process runs no plugins that carry traffic", ErrNotSent)
	}
	return c.carrier.Carry(req.Context(), c.digest, c.provider, req, c.secrets)
}
