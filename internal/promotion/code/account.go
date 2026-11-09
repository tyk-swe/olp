package code

import (
	"context"
	"slices"
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
	account := Account{ID: e.Id.String(), Project: e.Project, Provider: e.Provider, CredentialRef: e.CredentialRef, Name: e.Name, Enabled: e.Enabled, Models: promotion.NonNil(slices.Clone(e.Models)), ExtraUsageEnabled: promotion.Pointer(e.ExtraUsageEnabled), CycleAnchor: anchor(promotion.Pointer(e.CycleAnchor))}
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
	accounts, err := providers.ReadCodeAccounts(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(accounts, func(a providers.CodeAccount) Account {
		return Account{ID: a.ID, Project: a.Project, Provider: a.Provider, CredentialRef: codeRef("account", a.ID), Name: a.Name, Enabled: a.Enabled, Models: a.Models, ExtraUsageEnabled: a.ExtraUsageEnabled, CycleAnchor: a.CycleAnchor}
	}), nil
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
	return contract.ConfigurationCodeAccount{Id: id, Project: a.Project, Provider: a.Provider, CredentialRef: a.CredentialRef, Name: a.Name, Enabled: a.Enabled, Models: promotion.NonNil(slices.Clone(a.Models)), ExtraUsageEnabled: promotion.Nullable(a.ExtraUsageEnabled), CycleAnchor: promotion.Nullable(cycle)}, nil
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
	granted, err := providers.ReadGrantedCredential(ctx, q, a.Provider, projectID, credential)
	if err != nil {
		return resolved[providers.CodeAccountInput]{}, err
	}
	if granted == nil {
		return resolved[providers.CodeAccountInput]{}, access.Invalid("code_bindings.credentials", "Choose a current enrolled credential belonging to the destination provider and project.")
	}
	ids, err := providers.CodeAccountIDs(ctx, q, projectID, granted.ProviderID, granted.Principal)
	if err != nil {
		return resolved[providers.CodeAccountInput]{}, err
	}
	to, err := lookup(access.NewID(), ids)
	return resolved[providers.CodeAccountInput]{ref: a.ID, to: to, identity: promotion.TupleKey(projectID, granted.ProviderID, granted.Principal), input: a.input(projectID, granted.ProviderID, credential)}, err
}

func accountChange(ctx context.Context, q access.Queryer, a resolved[providers.CodeAccountInput]) (change, error) {
	action, err := compared(ctx, q, a, providers.ReadCodeAccountInput)
	return change{
		item: promotion.Item{Kind: contract.ConfigurationPlanItemKindCodeAccount, Key: a.ref, Action: action},
		write: func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
			_, err := providers.SaveCodeAccount(ctx, tx, p, a.to.id, access.NewID(), a.input, a.to.existing)
			return err
		},
	}, err
}
