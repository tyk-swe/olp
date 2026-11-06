package signing

import (
	"fmt"
	"slices"
	"sync"
)

const DevelopmentKeyID = "dev-2026a"

type encodedKey struct{ id, public string }

func developmentKey() encodedKey {
	return encodedKey{id: DevelopmentKeyID, public: "8FPiQcGvJqYX0/Kb56ly8UK67gpnU0DERDlQfuFIqtw="}
}

func releaseKeys() []encodedKey {
	return []encodedKey{
		{id: "release-2026a", public: "DhzCvhBBGXWpg+mNX7Z1fE4hJBUSVi0Llmxe6U7A+Nk="},
	}
}

func compiledKeys() []encodedKey {
	return slices.Concat([]encodedKey{developmentKey()}, releaseKeys())
}

var trusted = sync.OnceValues(func() (Keyring, error) {
	return NewKeyring(slices.Collect(mapped(trustedKeys(), encodedKey.decode))...)
})

func Trusted() (Keyring, error) { return trusted() }

func Known(id string) (Key, error) {
	known := compiledKeys()
	i := slices.IndexFunc(known, func(k encodedKey) bool { return k.id == id })
	if i < 0 {
		return Key{}, fmt.Errorf("key %q is not compiled into this build; add its public key to internal/signing/keys.go first", id)
	}
	key := known[i].decode()
	if malformedKey(key) {
		return Key{}, fmt.Errorf("compiled signing key %q is malformed", id)
	}
	return key, nil
}

func (k encodedKey) decode() Key {
	public, err := ParsePublicKey(k.public)
	if err != nil {
		return Key{ID: k.id}
	}
	return Key{ID: k.id, Public: public}
}
