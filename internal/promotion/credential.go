package promotion

import (
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
)

const MaxSecret = 64 << 10

type Ref string

type Credential interface{ credential() }

type Sealed struct{ Secret string }

type External struct{ Reference secretstore.Reference }

func (Sealed) credential()   {}
func (External) credential() {}

func (Sealed) String() string { return "Sealed([REDACTED])" }

type Bindings map[Ref]Credential

type Claim struct {
	Ref    Ref
	Accept func(Credential) error
}

func ParseBindings(request contract.ConfigurationPromotionRequest) (Bindings, error) {
	sealed := ValueOf(request.SecretBindings)
	external := ValueOf(request.ExternalCredentialBindings)
	if slices.ContainsFunc(slices.Collect(maps.Keys(external)), func(ref string) bool { _, ok := sealed[ref]; return ok }) {
		return nil, access.Invalid("external_credential_bindings", "A reference cannot also have a plaintext binding.")
	}
	return maps.Collect(func(yield func(Ref, Credential) bool) {
		for ref, secret := range sealed {
			if !yield(Ref(ref), Sealed{Secret: secret}) {
				return
			}
		}
		for ref, reference := range external {
			if !yield(Ref(ref), External{Reference: storeReference(reference)}) {
				return
			}
		}
	}), nil
}

func (b Bindings) HasExternal() bool {
	return slices.ContainsFunc(slices.Collect(maps.Values(b)), func(credential Credential) bool {
		_, external := credential.(External)
		return external
	})
}

func Claimed(bindings Bindings, claims []Claim) error {
	for _, ref := range slices.Sorted(maps.Keys(bindings)) {
		if err := accept(ref, bindings[ref], acceptsFor(claims, ref)); err != nil {
			return err
		}
	}
	return nil
}

func acceptsFor(claims []Claim, ref Ref) []func(Credential) error {
	return slices.Collect(func(yield func(func(Credential) error) bool) {
		for _, claim := range claims {
			if claim.Ref == ref && !yield(claim.Accept) {
				return
			}
		}
	})
}

func accept(ref Ref, credential Credential, checks []func(Credential) error) error {
	if len(checks) == 0 {
		if _, external := credential.(External); external {
			return externalRefused()
		}
		return access.Invalid("secret_bindings", "Secret binding "+string(ref)+" is not referenced by the document.")
	}
	for _, check := range checks {
		if err := check(credential); err != nil {
			return err
		}
	}
	return nil
}

func externalRefused() error {
	return access.Invalid("external_credential_bindings", "Use a document reference and an immutable store version.")
}

func ValidateExternal(reference secretstore.Reference) error {
	if reference.Validate() != nil {
		return externalRefused()
	}
	return nil
}

func AcceptBearer(credential Credential) error {
	return SealedOnly("Static bearer material uses sealed bindings.", func(secret string) error {
		encoded, err := json.Marshal(secret)
		if err != nil {
			return err
		}
		if _, err := secrets.ParseBearer(encoded); err != nil {
			return access.Invalid("credential", err.Error())
		}
		return nil
	})(credential)
}

func SealedOnly(refusal string, validate func(secret string) error) func(Credential) error {
	return func(credential Credential) error {
		sealed, ok := credential.(Sealed)
		if !ok {
			return access.Invalid("external_credential_bindings", refusal)
		}
		return validate(sealed.Secret)
	}
}

func storeReference(reference contract.ExternalCredentialReference) secretstore.Reference {
	return secretstore.Reference{
		Store:    string(reference.Store),
		SecretID: reference.SecretId,
		Version:  reference.Version,
		Region:   ValueOf(reference.Region),
		Field:    ValueOf(reference.Field),
	}
}
