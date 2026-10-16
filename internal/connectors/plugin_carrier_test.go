package connectors

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// recordingCarrier answers every carried request with a 418 response, recording
// what it was asked to carry.
type recordingCarrier struct {
	digest   string
	provider abi.Provider
	url      string
	secrets  []string
}

func (c *recordingCarrier) Carry(_ context.Context, digest string, provider abi.Provider, req *http.Request, secrets []string) (*http.Response, error) {
	c.digest, c.provider, c.url, c.secrets = digest, provider, req.URL.String(), secrets
	return &http.Response{StatusCode: http.StatusTeapot, Header: http.Header{}, Body: http.NoBody}, nil
}

// A process that runs no plugins sends nothing.
