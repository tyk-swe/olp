package guardrail

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Vault struct {
	Keys         *secrets.KeyRing
	Installation string
}

func CredentialApplier(desired Section, bindings promotion.Bindings, vault Vault) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		return promotion.Every(promotion.Filter(desired.Guardrails, func(g Guardrail) bool { return len(g.credentials) > 0 }), func(g Guardrail) error {
			project, err := projectID(directory, g.Project)
			if err != nil {
				return err
			}
			if err := p.Project(project, access.Change); err != nil {
				return err
			}
			return promotion.Every(g.credentials, func(r reference) error { return ensureCredential(ctx, tx, p, vault, bindings[r.ref], project, r.ref) })
		})
	}
}

func ensureCredential(ctx context.Context, tx pgx.Tx, p access.Principal, vault Vault, binding promotion.Credential, project *string, ref promotion.Ref) error {
	_, found, err := guardrails.FindCredentialID(ctx, tx, project, string(ref))
	if err != nil || found {
		return err
	}
	sealed, ok := binding.(promotion.Sealed)
	if !ok {
		return access.Invalid("secret_bindings", unboundCredential)
	}
	_, err = guardrails.CreateCredential(ctx, tx, vault.Keys, vault.Installation, guardrails.NewCredential{ProjectID: project, Name: string(ref), Secret: []byte(sealed.Secret), CreatedBy: p.UserID()})
	return err
}

type resolver func(string) (string, error)

func credentialID(ctx context.Context, q access.Queryer, project *string) resolver {
	return func(name string) (string, error) {
		id, found, err := guardrails.FindCredentialID(ctx, q, project, name)
		if err == nil && !found {
			return "", access.Invalid("secret_bindings", unboundCredential)
		}
		return id, err
	}
}

func credentialName(ctx context.Context, q access.Queryer) resolver {
	return func(id string) (string, error) { return guardrails.CredentialName(ctx, q, id) }
}

type network struct {
	*egress.ConnectionOptions
	CredentialID string `json:"credential_id,omitempty"`
}

func rewriteCredentials(kind guardrails.Kind, configuration json.RawMessage, resolve resolver) (json.RawMessage, error) {
	if kind.Builtin() {
		return configuration, nil
	}
	var c guardrails.ExternalConfig
	if err := json.Unmarshal(configuration, &c); err != nil {
		return nil, access.Invalid("guardrails", "Invalid published configuration.")
	}
	credential, err := resolved(c.CredentialRef, resolve)
	if err != nil {
		return nil, err
	}
	connection, err := rewriteNetwork(c.Network, resolve)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		guardrails.ExternalConfig
		CredentialRef string   `json:"credential_ref,omitempty"`
		Network       *network `json:"network,omitempty"`
	}{c, credential, connection})
}

func rewriteNetwork(options *egress.ConnectionOptions, resolve resolver) (*network, error) {
	if options == nil {
		return nil, nil
	}
	credential, err := resolved(options.CredentialID, resolve)
	if err != nil {
		return nil, err
	}
	return &network{options, credential}, nil
}

func resolved(value string, resolve resolver) (string, error) {
	if value == "" {
		return "", nil
	}
	return resolve(value)
}
