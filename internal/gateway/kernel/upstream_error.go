package kernel

import "net/http"

// inBandStatus is the status an in-band stream failure presents to the
// client, as if the upstream had answered with it.
func inBandStatus(class string) int {
	switch class {
	case classContextWindow, classUpstreamClient, classContentFilter:
		return http.StatusBadRequest
	case ClassCredential:
		return http.StatusUnauthorized
	case classRateLimit:
		return http.StatusTooManyRequests
	case ClassTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}
