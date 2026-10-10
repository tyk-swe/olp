//go:build !oidctest

package connectors

import "net/url"

func vertexTestDestination(*url.URL) bool { return false }
