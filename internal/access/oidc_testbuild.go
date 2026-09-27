//go:build oidctest

package access

import "net/netip"

const oidcTestBuild = true

// oidcTestNetworks lets integration tests run a loopback identity provider.
var oidcTestNetworks = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
