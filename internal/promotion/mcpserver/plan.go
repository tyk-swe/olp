package mcpserver

import (
	"context"
	"crypto/subtle"
	"reflect"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	entraDetail = "Install this Entra issuer, client, and token endpoint with its replacement secret locally before applying the metadata."
	planDetail  = "Recertify changed registrations against the reviewed catalog digest; history and credentials remain local."
)

type Keys struct {
	Ring         *secrets.KeyRing
	Installation string
}

func (k Keys) read(ctx context.Context, q secrets.RowQuerier, id string) ([]byte, error) {
	return k.Ring.Read(ctx, q, k.Installation, id, secrets.MCPCredential)
}

func (k Keys) store(ctx context.Context, tx pgx.Tx, id, secret string) error {
	return k.Ring.Store(ctx, tx, k.Installation, id, secrets.MCPCredential, []byte(secret), nil)
}

type planned struct {
	items   []promotion.Item
	creates bool
}

func Planner(servers []Server, names promotion.Names, bindings promotion.Bindings, keys Keys) promotion.Planner {
	if len(servers) == 0 {
		return promotion.Planner{}
	}
	return promotion.Planner{
		Claims: slices.Collect(func(yield func(promotion.Claim) bool) {
			for _, server := range servers {
				bearer := flatten(server.Transport).bearer
				if bearer != nil && !yield(promotion.Claim{Ref: *bearer, Accept: promotion.AcceptBearer}) {
					return
				}
			}
		}),
		Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
			return plan(ctx, q, servers, names, bindings, keys)
		},
	}
}

func plan(ctx context.Context, q access.Queryer, servers []Server, names promotion.Names, bindings promotion.Bindings, keys Keys) ([]promotion.Item, error) {
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return nil, err
	}
	active, err := registration.ListActive(ctx, q)
	if err != nil {
		return nil, err
	}
	plans, err := promotion.TryMap(servers, func(server Server) (planned, error) {
		return planServer(ctx, q, names, directory, bindings, keys, current(active, directory, server), server)
	})
	if err != nil {
		return nil, err
	}
	creating := slices.Collect(func(yield func(string) bool) {
		for index, server := range servers {
			if plans[index].creates && !yield(server.Name) {
				return
			}
		}
	})
	overflow := slices.Collect(func(yield func(promotion.Item) bool) {
		for index, name := range creating {
			if len(active)+index+1 > maxServers && !yield(promotion.Blocker(kind, name, "The destination would exceed 128 active MCP servers.")) {
				return
			}
		}
	})
	return slices.Concat(slices.Collect(func(yield func(promotion.Item) bool) {
		for _, server := range plans {
			for _, item := range server.items {
				if !yield(item) {
					return
				}
			}
		}
	}), overflow), nil
}

func planServer(ctx context.Context, q access.Queryer, names promotion.Names, directory promotion.Directory, bindings promotion.Bindings, keys Keys, existing *registration.Definition, server Server) (planned, error) {
	if absent(names, directory, server.Project) {
		return planned{items: []promotion.Item{promotion.Conflict(kind, server.Name, "The owning project is not declared or present.")}}, nil
	}
	flat := flatten(server.Transport)
	entra := promotion.When(unretainedEntra(flat, existing), promotion.Conflict(kind, server.Name, entraDetail))
	if flat.delegation != nil && (existing == nil || existing.DelegationKeyID == nil) {
		return planned{items: slices.Concat(entra, []promotion.Item{promotion.Conflict(kind, server.Name, "Install this server's delegation signing key locally before applying its metadata.")})}, nil
	}
	unbound := flat.bearer != nil && bindings[*flat.bearer] == nil && (existing == nil || !existing.HasCredential)
	action, err := actionOf(ctx, q, bindings, keys, existing, server)
	if err != nil {
		return planned{}, err
	}
	return planned{
		items: slices.Concat(entra, promotion.When(unbound, promotion.Blocker(kind, server.Name, "Supply the destination MCP bearer binding.")), []promotion.Item{
			{Kind: kind, Key: server.Name, Action: action, Detail: planDetail},
		}),
		creates: existing == nil,
	}, nil
}

func absent(names promotion.Names, directory promotion.Directory, name string) bool {
	_, present := directory.ID(name)
	return !present && !names.Project(name)
}

func unretainedEntra(flat wire, existing *registration.Definition) bool {
	return flat.oauth != nil && flat.oauth.EntraOBO != nil && !registration.CanRetainEntraClientSecret(existing, flat.oauth)
}

func actionOf(ctx context.Context, q access.Queryer, bindings promotion.Bindings, keys Keys, existing *registration.Definition, server Server) (contract.ConfigurationPlanItemAction, error) {
	if existing == nil {
		return contract.ConfigurationPlanItemActionCreate, nil
	}
	equal, err := matches(ctx, q, bindings, keys, *existing, server)
	if err != nil || !equal {
		return contract.ConfigurationPlanItemActionReplace, err
	}
	return contract.ConfigurationPlanItemActionReuse, nil
}

func matches(ctx context.Context, q access.Queryer, bindings promotion.Bindings, keys Keys, existing registration.Definition, server Server) (bool, error) {
	if !reflect.DeepEqual(fromDefinition(existing, server.Project), server) {
		return false, nil
	}
	bearer := flatten(server.Transport).bearer
	if bearer == nil {
		return true, nil
	}
	sealed, bound := bindings[*bearer].(promotion.Sealed)
	if !bound {
		return true, nil
	}
	secret, err := keys.read(ctx, q, *existing.CredentialID)
	if err != nil {
		return false, err
	}
	defer clear(secret)
	return subtle.ConstantTimeCompare(secret, []byte(sealed.Secret)) == 1, nil
}
