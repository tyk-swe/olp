package code

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/providers"
)

type Account struct {
	ID                string
	Project           string
	Provider          string
	CredentialRef     string
	Name              string
	Enabled           bool
	Models            []string
	ExtraUsageEnabled *bool
	CycleAnchor       *string
}

func parseAccount(e contract.ConfigurationCodeAccount) (Account, error) {
	if access.ValidText("code", e.Project, 100) != nil || access.ValidText("code", e.Provider, 100) != nil || access.ValidText("code", e.CredentialRef, 200) != nil {
		return Account{}, access.Invalid("code.accounts", "Name the project, provider and destination credential reference.")
	}
	account := Account{ID: e.Id.String(), Project: e.Project, Provider: e.Provider, CredentialRef: e.CredentialRef, Name: e.Name, Enabled: e.Enabled, Models: append(make([]string, 0, len(e.Models)), e.Models...), ExtraUsageEnabled: promotion.Pointer(e.ExtraUsageEnabled), CycleAnchor: anchor(promotion.Pointer(e.CycleAnchor))}
	return account, account.input(account.ID, account.ID, account.ID).Validate()
}

func anchor(date *openapi_types.Date) *string {
	if date == nil {
		return nil
	}
	value := date.String()
	return &value
}

func (a Account) input(project, provider, credential string) providers.CodeAccountInput {
	return providers.CodeAccountInput{ProjectID: project, ProviderID: provider, CredentialID: credential, Name: a.Name, Enabled: a.Enabled, Models: a.Models, ExtraUsageEnabled: a.ExtraUsageEnabled, CycleAnchor: a.CycleAnchor}
}

func readAccounts(ctx context.Context, q access.Queryer) ([]Account, error) {
	rows, err := q.Query(ctx, `SELECT a.id::text,p.name,v.name,'code/account/'||a.id,a.name,a.enabled,a.models,a.extra_usage_enabled,a.cycle_anchor::text FROM olp.code_accounts a JOIN olp.projects p ON p.id=a.project_id JOIN olp.providers v ON v.id=a.provider_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Account])
}

func renderAccount(a Account) (contract.ConfigurationCodeAccount, error) {
	id, err := uuid.Parse(a.ID)
	if err != nil {
		return contract.ConfigurationCodeAccount{}, err
	}
	cycle, err := date(a.CycleAnchor)
	if err != nil {
		return contract.ConfigurationCodeAccount{}, err
	}
	return contract.ConfigurationCodeAccount{Id: id, Project: a.Project, Provider: a.Provider, CredentialRef: a.CredentialRef, Name: a.Name, Enabled: a.Enabled, Models: append(make([]string, 0, len(a.Models)), a.Models...), ExtraUsageEnabled: promotion.Nullable(a.ExtraUsageEnabled), CycleAnchor: promotion.Nullable(cycle)}, nil
}

func date(value *string) (*openapi_types.Date, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := time.Parse(openapi_types.DateFormat, *value)
	if err != nil {
		return nil, err
	}
	return &openapi_types.Date{Time: parsed}, nil
}

func resolveAccount(ctx context.Context, q access.Queryer, a Account, directory project.Directory, credentials map[string]string) (resolved[providers.CodeAccountInput], error) {
	projectID, err := projectOf(directory, a.Project)
	if err != nil {
		return resolved[providers.CodeAccountInput]{}, err
	}
	credential, err := bound(a.CredentialRef, credentials)
	if err != nil {
		return resolved[providers.CodeAccountInput]{}, err
	}
	var provider, principal string
	switch err := q.QueryRow(ctx, `SELECT p.id::text,c.principal FROM olp.providers p JOIN olp.provider_credentials c ON c.provider_id=p.id JOIN olp.provider_grants g ON g.credential_id=c.id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND c.id=$3 AND c.principal<>'' AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())`, a.Provider, projectID, credential).Scan(&provider, &principal); {
	case errors.Is(err, pgx.ErrNoRows):
		return resolved[providers.CodeAccountInput]{}, access.Invalid("code_bindings.credentials", "Choose a current enrolled credential belonging to the destination provider and project.")
	case err != nil:
		return resolved[providers.CodeAccountInput]{}, err
	}
	to, err := lookup(ctx, q, access.NewID(), `SELECT id::text FROM olp.code_accounts WHERE project_id=$1 AND provider_id=$2 AND principal=$3`, projectID, provider, principal)
	return resolved[providers.CodeAccountInput]{ref: a.ID, to: to, identity: strings.Join([]string{projectID, provider, principal}, "\x00"), input: a.input(projectID, provider, credential)}, err
}

func accountChange(ctx context.Context, q access.Queryer, a resolved[providers.CodeAccountInput]) (change, error) {
	action, err := compared(ctx, q, a, `SELECT jsonb_build_object('project_id',project_id,'provider_id',provider_id,'credential_id',credential_id,'name',name,'enabled',enabled,'models',models,'extra_usage_enabled',extra_usage_enabled,'cycle_anchor',cycle_anchor) FROM olp.code_accounts WHERE id=$1`)
	return change{
		item: promotion.Item{Kind: contract.ConfigurationPlanItemKindCodeAccount, Key: a.ref, Action: action},
		write: func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
			_, err := providers.SaveCodeAccount(ctx, tx, p, a.to.id, access.NewID(), a.input, a.to.existing)
			return err
		},
	}, err
}
