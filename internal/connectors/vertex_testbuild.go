//go:build oidctest

package connectors

import (
	"net"
	"net/url"
)

// Identity integration fixtures run only in the oidctest build. Production
// never permits loopback destinations for native Vertex credentials.
func vertexTestDestination(u *url.URL) bool {
	return u.User == nil && u.Opaque == "" && (u.Scheme == "http" || u.Scheme == "https") && net.ParseIP(u.Hostname()).IsLoopback()
}
