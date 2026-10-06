//go:build release

package signing

// Channel names the trust a build applies to signed documents.
const Channel = "release"

// A release build trusts only release keys: a document signed with the public
// development seed does not verify.
var trusted = MustKeyring(releaseKeys...)
