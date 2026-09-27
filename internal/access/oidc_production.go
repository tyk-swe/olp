//go:build !oidctest

package access

import "net/netip"

const oidcTestBuild = false

// oidcTestNetworks is empty outside the oidctest build: identity egress has no
// exceptions to the public-address rule.
var oidcTestNetworks []netip.Prefix
