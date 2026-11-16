package connectors

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/tyk-swe/olp/plugin/abi"
)

const pluginTransport = "plugin"

type Carrier interface {
	Carry(ctx context.Context, digest string, provider abi.Provider, req *http.Request, secrets []string) (*http.Response, error)
}

var ErrNotSent = errors.New("the request was not sent")

func (c Config) CarriedByPlugin() bool {
	return c.Plugin != nil && c.Plugin.declared.CarriesTraffic
}

func (c Config) CarrierClient(carrier Carrier, secrets []string) *http.Client {
	return &http.Client{
		Transport: carried{carrier: carrier, digest: c.Plugin.profile.Revision, provider: abi.Provider{Profile: c.Plugin.profile.ID, Options: c.PluginOptions}, secrets: secrets},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

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
