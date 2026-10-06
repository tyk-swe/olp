//go:build !release

package signing

// Channel names the trust a build applies to signed documents.
const Channel = "development"

var trusted = MustKeyring(append([]Key{developmentKey}, releaseKeys...)...)
