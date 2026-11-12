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
	"github.com/tyk-swe/olp/internal/providers/codeaccounts"
)

type Account struct {
	ID            string
	Project       string
	Provider      string
	CredentialRef string
	codeaccounts.Settings
}

func parseAccount(e contract.ConfigurationCodeAccount) (Account, error) {
	if access.ValidText("code", e.Project, 100) != nil || access.ValidText("code", e.Provider, 100) != nil || access.ValidText("code", e.CredentialRef, 200) != nil {
		return Account{}, access.Invalid("code.accounts", "Name the project, provider and destination credential reference.")
	}
	account := Account{ID: e.Id.String(), Project: e.Project, Provider: e.Provider, CredentialRef: e.CredentialRef, Settings: codeaccounts.Settings{Name: e.Name, Enabled: e.Enabled, Models: promotion.NonNil(slices.Clone(e.Models)), ExtraUsageEnabled: promotion.Pointer(e.ExtraUsageEnabled), CycleAnchor: anchor(promotion.Pointer(e.CycleAnchor))}}
	return account, account.Settings.Validate()
}

func anchor(date *openapi_types.Date) *string {
	if date == nil {
		return nil
	}
	value := date.String()
	return &value
}

func (a Account) input(project, provider, credential string) codeaccounts.Input {
	return codeaccounts.Input{ProjectID: project, ProviderID: provider, CredentialID: credential, Settings: a.Settings}
}

func readAccounts(ctx context.Context, q access.Queryer) ([]Account, error) {
	accounts, err := codeaccounts.ReadAccounts(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(accounts, func(a codeaccounts.Account) Account {
		return Account{ID: a.ID, Project: a.Project, Provider: a.Provider, CredentialRef: codeRef("account", a.ID), Settings: a.Settings}
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

func resolveAccount(ctx context.Context, q access.Queryer, a Account, directory promotion.Directory, credentials map[string]string) (resolved[codeaccounts.Input], error) {
	projectID, err := projectOf(directory, a.Project)
	if err != nil {
		return resolved[codeaccounts.Input]{}, err
	}
	credential, err := bound(a.CredentialRef, credentials)
	if err != nil {
		return resolved[codeaccounts.Input]{}, err
	}
	granted, found, err := codeaccounts.ReadGranted(ctx, q, a.Provider, projectID, credential)
	if err != nil {
		return resolved[codeaccounts.Input]{}, err
	}
	if !found {
		return resolved[codeaccounts.Input]{}, access.Invalid("code_bindings.credentials", "Choose a current enrolled credential belonging to the destination provider and project.")
	}
	ids, err := codeaccounts.AccountIDs(ctx, q, projectID, granted.ProviderID, granted.Principal)
	if err != nil {
		return resolved[codeaccounts.Input]{}, err
	}
	to, err := lookup(access.NewID(), ids)
	return resolved[codeaccounts.Input]{ref: a.ID, to: to, identity: promotion.TupleKey(projectID, granted.ProviderID, granted.Principal), input: a.input(projectID, granted.ProviderID, credential)}, err
}

func accountChange(ctx context.Context, q access.Queryer, a resolved[codeaccounts.Input]) (change, error) {
	action, err := compared(ctx, q, a, codeaccounts.ReadInput)
	return change{
		item: promotion.Item{Kind: contract.ConfigurationPlanItemKindCodeAccount, Key: a.ref, Action: action},
		write: func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
			_, err := codeaccounts.Save(ctx, tx, p, a.to.id, access.NewID(), a.input, a.to.existing)
			return err
		},
	}, err
}
