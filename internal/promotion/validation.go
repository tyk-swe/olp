package promotion

import (
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/secrets"
)

func ValidateBindings(doc *Document, bindings BindingSet) error {
	refs, grants := map[string]bool{}, map[string]bool{}
	guardrailRefs, guardrailNetworkRefs := map[string]bool{}, map[string]bool{}
	for _, entry := range doc.Guardrails {
		if strings.HasPrefix(entry.Type, "builtin.") {
			continue
		}
		var config guardrails.ExternalConfig
		if json.Unmarshal(entry.Configuration, &config) != nil {
			return access.Invalid("guardrails", "Invalid guardrail configuration.")
		}
		if config.CredentialRef != "" {
			refs[config.CredentialRef], guardrailRefs[config.CredentialRef] = true, true
		}
		if config.Network != nil && config.Network.CredentialID != "" {
			name := config.Network.CredentialID
			refs[name], guardrailRefs[name], guardrailNetworkRefs[name] = true, true, true
		}
	}
	bearerRefs := map[string]bool{}
	for _, e := range doc.MCPServers {
		if e.CredentialRef != nil {
			refs[*e.CredentialRef] = true
			bearerRefs[*e.CredentialRef] = true
		}
	}
	for _, e := range doc.AgentPublications {
		for _, ref := range []*string{e.CredentialRef, e.PublishedCredentialRef} {
			if ref != nil {
				refs[*ref], bearerRefs[*ref] = true, true
			}
		}
	}
	sinkRefs := map[string]bool{}
	for _, entry := range doc.Sinks {
		if entry.CredentialRef != nil {
			refs[*entry.CredentialRef] = true
			sinkRefs[*entry.CredentialRef] = true
		}
	}
	billingRefs := map[string]string{}
	for _, p := range doc.Providers {
		if p.NetworkCredentialRef != nil {
			refs[*p.NetworkCredentialRef] = true
		}
		if p.BillingImport != nil {
			refs[p.BillingImport.CredentialRef] = true
			source, err := billing.SourceFor(p.Configuration.Kind)
			if err != nil {
				return err
			}
			billingRefs[p.BillingImport.CredentialRef] = source
		}
		for _, slot := range p.Slots {
			if slot.CredentialRef != nil {
				refs[*slot.CredentialRef] = true
				grants[*slot.CredentialRef] = p.Configuration.Grant()
			}
		}
	}
	for name, secret := range bindings {
		if guardrailRefs[name] {
			if secret.Reference != nil {
				return access.Invalid("external_credential_bindings", "Guardrail credentials require sealed secret bindings.")
			}
			if err := guardrails.ValidateCredential([]byte(secret.Secret)); err != nil || len(secret.Secret) > MaxSecretBytes {
				return access.Invalid("secret_bindings."+name, "Supply a bounded guardrail credential object.")
			}
			if guardrailNetworkRefs[name] && egress.ValidateConnectionSecret([]byte(secret.Secret)) != nil {
				return access.Invalid("secret_bindings."+name, "Supply a valid network credential object.")
			}
			continue
		}
		if bearerRefs[name] {
			if secret.Reference != nil {
				return access.Invalid("external_credential_bindings", "Static bearer material uses sealed bindings.")
			}
			encoded, _ := json.Marshal(secret.Secret)
			if _, err := secrets.ParseBearer(encoded); err != nil {
				return access.Invalid("credential", err.Error())
			}
			continue
		}
		if sinkRefs[name] {
			if secret.Reference != nil || len(secret.Secret) < 1 || len(secret.Secret) > 4096 {
				return access.Invalid("secret_bindings", "Sink signing credentials require 1–4096 sealed bytes.")
			}
			continue
		}
		if source := billingRefs[name]; source != "" {
			if secret.Reference != nil {
				return access.Invalid("external_credential_bindings", "Billing credentials require sealed secret bindings.")
			}
			if err := billing.ValidateCredential(source, secret.Secret); err != nil {
				return err
			}
			continue
		}
		if grants[name] {
			return access.Invalid("secret_bindings."+name, GrantBindingRefused)
		}
		if secret.Reference != nil {
			if !refs[name] || secret.Reference.Validate() != nil {
				return access.Invalid("external_credential_bindings", "Use a document reference and an immutable store version.")
			}
			for _, provider := range doc.Providers {
				if provider.NetworkCredentialRef != nil && *provider.NetworkCredentialRef == name {
					return access.Invalid("external_credential_bindings", "Network identities require sealed secret bindings.")
				}
			}
			continue
		}
		for _, provider := range doc.Providers {
			if provider.NetworkCredentialRef != nil && *provider.NetworkCredentialRef == name {
				if err := egress.ValidateConnectionSecret([]byte(secret.Secret)); err != nil {
					return access.Invalid("secret_bindings."+name, err.Error())
				}
			}
		}
		if !refs[name] {
			return access.Invalid("secret_bindings", "Secret binding "+name+" is not referenced by the document.")
		}
		if err := providers.ValidCredential(secret.Secret); err != nil {
			return access.Invalid("secret_bindings."+name, "Use a credential of 1–65536 bytes.")
		}
		if len(secret.Secret) > MaxSecretBytes {
			return access.Invalid("secret_bindings."+name, "Secrets must fit within 64 KiB.")
		}
	}
	return nil
}
