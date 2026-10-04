//go:build oidctest

package connectors

import (
	"net"
	"net/url"
)

// The existing oidctest build runs local identity/cloud-token fixtures.
// Production builds never admit this credential destination exception.
func vertexTestDestination(u *url.URL) bool {
	return u.User == nil && (u.Scheme == "http" || u.Scheme == "https") && net.ParseIP(u.Hostname()).IsLoopback()
}
