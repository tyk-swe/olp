package configuration

import (
	"context"
	"errors"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers"
)

// A plugin provider's configuration pins its plugin profile by the plugin's
// digest, so an artifact references the plugin build without carrying it, and
// never carries grant material: importing needs that build installed and a new
// grant enrollment for each credential slot a grant backs.

// pinPlugin pins the plugin profile a plugin provider's configuration names,
// which gives the provider its address. A plugin this installation can't use
// yet blocks the plan rather than refusing the artifact: unavailable is the
// refusal's code, and the operator installs and approves that build, then
// plans again.
func pinPlugin(ctx context.Context, q access.Queryer, cfg *providers.Configuration) (unavailable string, err error) {
	err = cfg.Pin(ctx, q)
	if refusal, ok := errors.AsType[*access.Problem](err); ok && (refusal.Code == plugins.CodeNotInstalled || refusal.Code == plugins.CodeNotApproved) {
		return refusal.Code, nil
	}
	return "", err
}

// referenceGrants gives each credential slot of a provider that authenticates
// with a grant a credential reference, whether a grant backs it yet or not.
// The grant stays behind, so importing marks each such slot for grant
// enrollment instead of binding a secret.
func referenceGrants(entry *ProviderEntry) {
	if !entry.Configuration.Grant() {
		return
	}
	for i := range entry.Slots {
		ref := CredentialRef(entry.Name, entry.Slots[i].Name)
		entry.Slots[i].CredentialRef = &ref
	}
}

// grantBindingRefused refuses a secret binding for the credential of a slot a
// grant backs, which only grant enrollment supplies.
const grantBindingRefused = "A grant backs this credential slot: enroll a grant for it after applying instead of binding a secret."
