//go:build release

package signing

const Channel = "release"

func trustedKeys() []encodedKey { return releaseKeys() }
