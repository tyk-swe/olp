//go:build oidctest

package connectors

import (
	"net/url"
	"testing"
)

func TestVertexFixtureExceptionOnlyAllowsNumericLoopback(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"http://127.0.0.1:8080/v1":             true,
		"https://[::1]:443/v1":                 true,
		"http://localhost:8080/v1":             false,
		"http://127.0.0.1.attacker.example/v1": false,
		"http://user@127.0.0.1/v1":             false,
		"http://10.0.0.1/v1":                   false,
	} {
		u, err := url.Parse(endpoint)
		if err != nil || vertexTestDestination(u) != want {
			t.Fatalf("%s: allowed=%t, want %t (%v)", endpoint, vertexTestDestination(u), want, err)
		}
	}
}
