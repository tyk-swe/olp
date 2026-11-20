package guardrail

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/guardrails/credential"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

type Vault struct {
	Keys         *secrets.KeyRing
	Installation string
}

func CredentialApplier(desired Section, bindings promotion.Bindings, vault Vault) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p authz.Principal) error {
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		return promotion.Every(promotion.Filter(desired.Guardrails, func(g Guardrail) bool { return len(g.credentials) > 0 }), func(g Guardrail) error {
			project, err := projectID(directory, g.Project)
			if err != nil {
				return err
			}
			if err := p.Project(project, authz.Change); err != nil {
				return err
			}
			return promotion.Every(g.credentials, func(r reference) error { return ensureCredential(ctx, tx, p, vault, bindings[r.ref], project, r.ref) })
		})
	}
}

func ensureCredential(ctx context.Context, tx pgx.Tx, p authz.Principal, vault Vault, binding promotion.Credential, project *string, ref promotion.Ref) error {
	_, found, err := credential.FindID(ctx, tx, project, string(ref))
	if err != nil || found {
		return err
	}
	sealed, ok := binding.(promotion.Sealed)
	if !ok {
		return httpkit.Invalid("secret_bindings", unboundCredential)
	}
	_, err = credential.Create(ctx, tx, vault.Keys, vault.Installation, credential.New{ProjectID: project, Name: string(ref), Secret: []byte(sealed.Secret), CreatedBy: p.UserID()})
	return err
}

type resolver func(string) (string, error)

func credentialID(ctx context.Context, q database.Queryer, project *string) resolver {
	return func(name string) (string, error) {
		id, found, err := credential.FindID(ctx, q, project, name)
		if err == nil && !found {
			return "", httpkit.Invalid("secret_bindings", unboundCredential)
		}
		return id, err
	}
}

func credentialName(ctx context.Context, q database.Queryer) resolver {
	return func(id string) (string, error) { return credential.Name(ctx, q, id) }
}
