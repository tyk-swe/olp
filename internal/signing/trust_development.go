//go:build !release

package signing

const Channel = "development"

func trustedKeys() []encodedKey { return compiledKeys() }
