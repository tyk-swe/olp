package signing

// DevelopmentKeyID names the development key. Its seed is public, in
// devkey/, so local builds and tests can sign documents after an edit.
const DevelopmentKeyID = "dev-2026a"

// developmentKey verifies documents signed with the development seed. Only
// builds without the release tag trust it.
var developmentKey = Key{ID: DevelopmentKeyID, Public: mustPublicKey("8FPiQcGvJqYX0/Kb56ly8UK67gpnU0DERDlQfuFIqtw=")}

// releaseKeys verify documents the release workflow signed with the key only
// CI holds. Rotation adds the incoming key here a release before documents
// are signed with it alone; see docs/catalog.md#signing-keys.
var releaseKeys = []Key{
	{ID: "release-2026a", Public: mustPublicKey("DhzCvhBBGXWpg+mNX7Z1fE4hJBUSVi0Llmxe6U7A+Nk=")},
}

// Trusted is the keyring this build trusts: the release keys, and in a
// development build the development key.
func Trusted() Keyring { return trusted }

func mustPublicKey(encoded string) []byte {
	key, err := ParsePublicKey(encoded)
	if err != nil {
		panic(err)
	}
	return key
}

// Known returns a key compiled into the binary, trusted by this build or not,
// so a signer can refuse a seed that is not the key it names.
func Known(id string) (Key, bool) {
	for _, key := range append([]Key{developmentKey}, releaseKeys...) {
		if key.ID == id {
			return key, true
		}
	}
	return Key{}, false
}
