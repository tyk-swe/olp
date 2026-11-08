package sink

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/sinks"
)

const (
	kind   = contract.ConfigurationPlanItemKindSink
	detail = "Export content-free durable facts; queued events and delivery history remain local."
)

func Planner(desired []Sink, names promotion.Names, bindings promotion.Bindings, credentials sinks.Credentials) promotion.Planner {
	return promotion.Planner{
		Claims: slices.Collect(func(yield func(promotion.Claim) bool) {
			for _, s := range desired {
				if s.Signed && !yield(promotion.Claim{Ref: s.ref(), Accept: acceptCredential}) {
					return
				}
			}
		}),
		Authorize: func(_ context.Context, _ access.Queryer, principal access.Principal) error {
			for _, s := range desired {
				if err := principal.Authorize(operation(s.Owner)); err != nil {
					return err
				}
			}
			return nil
		},
		Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
			return plan(ctx, q, desired, names, bindings, credentials)
		},
	}
}

func acceptCredential(credential promotion.Credential) error {
	sealed, ok := credential.(promotion.Sealed)
	if !ok || len(sealed.Secret) < 1 || len(sealed.Secret) > 4096 {
		return access.Invalid("secret_bindings", "Sink signing credentials require 1–4096 sealed bytes.")
	}
	return nil
}

func operation(owner Owner) access.Operation {
	switch owner.(type) {
	case Project:
		return access.Keys
	default:
		return access.Settings
	}
}

type destination struct {
	stored   map[key]stored
	projects project.Directory
	names    promotion.Names
}

func (d destination) placeable(s Sink) bool {
	owner, scoped := s.Owner.(Project)
	if !scoped {
		return true
	}
	_, present := d.projects.ID(owner.Name)
	return present || d.names.Project(owner.Name)
}

func (d destination) fresh(s Sink) bool {
	_, found := d.stored[s.key()]
	return d.placeable(s) && !found
}

func plan(ctx context.Context, q access.Queryer, desired []Sink, names promotion.Names, bindings promotion.Bindings, credentials sinks.Credentials) ([]promotion.Item, error) {
	if len(desired) == 0 {
		return nil, nil
	}
	current, err := readStored(ctx, q)
	if err != nil {
		return nil, err
	}
	projects, projectErr := project.ReadDirectory(ctx, q)
	if projectErr != nil {
		return nil, projectErr
	}
	at := destination{stored: index(current), projects: projects, names: names}
	planned, err := promotion.TryMapIndex(desired, func(i int, s Sink) ([]promotion.Item, error) {
		active := len(current) + len(promotion.Filter(desired[:i+1], at.fresh))
		return planSink(ctx, q, s, at, active, sealedBinding(bindings, s), credentials)
	})
	if err != nil {
		return nil, err
	}
	return slices.Concat(planned...), nil
}

type binding struct {
	secret string
	bound  bool
}

func sealedBinding(bindings promotion.Bindings, s Sink) binding {
	sealed, ok := bindings[s.ref()].(promotion.Sealed)
	return binding{secret: sealed.Secret, bound: s.Signed && ok}
}

func planSink(ctx context.Context, q access.Queryer, s Sink, at destination, active int, credential binding, credentials sinks.Credentials) ([]promotion.Item, error) {
	if !at.placeable(s) {
		return []promotion.Item{promotion.Conflict(kind, s.Name, "The owning project is not declared or present at the destination.")}, nil
	}
	current, found := at.stored[s.key()]
	action, err := sinkAction(ctx, q, s, current, found, credential, credentials)
	if err != nil {
		return nil, err
	}
	return slices.Concat(
		promotion.When(s.Signed && !credential.bound && (!found || current.credential == nil), promotion.Blocker(kind, s.Name, "Supply the destination signing-credential binding.")),
		promotion.When(!found && active > maxSinks, promotion.Blocker(kind, s.Name, "The destination would exceed 64 active sinks.")),
		[]promotion.Item{{Kind: kind, Key: s.Name, Action: action, Detail: detail}},
	), nil
}

func sinkAction(ctx context.Context, q access.Queryer, s Sink, current stored, found bool, credential binding, credentials sinks.Credentials) (contract.ConfigurationPlanItemAction, error) {
	if !found {
		return contract.ConfigurationPlanItemActionCreate, nil
	}
	if !same(current.sink, s) {
		return contract.ConfigurationPlanItemActionReplace, nil
	}
	rotation, err := rotate(ctx, q, s, current, credential, credentials)
	if err != nil {
		return "", err
	}
	if rotation.needed {
		return contract.ConfigurationPlanItemActionReplace, nil
	}
	return contract.ConfigurationPlanItemActionReuse, nil
}

func same(a, b Sink) bool {
	return a.Name == b.Name && a.Destination == b.Destination && slices.Equal(a.Streams, b.Streams) && a.Enabled == b.Enabled && a.Signed == b.Signed
}

type rotation struct {
	needed bool
	secret *string
}

func rotate(ctx context.Context, q access.Queryer, s Sink, current stored, credential binding, credentials sinks.Credentials) (rotation, error) {
	if !s.Signed {
		return rotation{needed: current.credential != nil}, nil
	}
	if !credential.bound {
		return rotation{}, nil
	}
	if current.credential == nil {
		return rotation{needed: true, secret: &credential.secret}, nil
	}
	matches, err := credentials.Matches(ctx, q, *current.credential, credential.secret)
	if err != nil || matches {
		return rotation{}, err
	}
	return rotation{needed: true, secret: &credential.secret}, nil
}

func Applier(desired []Sink, bindings promotion.Bindings, credentials sinks.Credentials) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, principal access.Principal) error {
		if len(desired) == 0 {
			return nil
		}
		current, err := readStored(ctx, tx)
		if err != nil {
			return err
		}
		projects, projectErr := project.ReadDirectory(ctx, tx)
		if projectErr != nil {
			return projectErr
		}
		existing := index(current)
		for _, s := range desired {
			if err := applySink(ctx, tx, principal, s, existing, projects, sealedBinding(bindings, s), credentials); err != nil {
				return err
			}
		}
		return nil
	}
}

func applySink(ctx context.Context, tx pgx.Tx, principal access.Principal, s Sink, existing map[key]stored, projects project.Directory, credential binding, credentials sinks.Credentials) error {
	projectID, err := owningProject(s.Owner, projects, principal)
	if err != nil {
		return err
	}
	current, found := existing[s.key()]
	change, rotateErr := rotate(ctx, tx, s, current, credential, credentials)
	if rotateErr != nil {
		return rotateErr
	}
	if found && same(current.sink, s) && !change.needed {
		return nil
	}
	id, writeErr := write(ctx, tx, s, projectID, current, found, principal.UserID())
	if writeErr != nil || !change.needed {
		return writeErr
	}
	return credentials.Replace(ctx, tx, id, change.secret)
}

func owningProject(owner Owner, projects project.Directory, principal access.Principal) (*string, error) {
	scoped, ok := owner.(Project)
	if !ok {
		return nil, nil
	}
	id, present := projects.ID(scoped.Name)
	if !present {
		return nil, access.Invalid("sinks.project", "The owning project is not declared or present at the destination.")
	}
	if err := principal.Project(&id, access.Change); err != nil {
		return nil, err
	}
	return &id, nil
}

func write(ctx context.Context, tx pgx.Tx, s Sink, projectID *string, current stored, found bool, actor string) (string, error) {
	etag := access.NewID()
	if found {
		return current.id, sinks.Update(ctx, tx, current.id, s.spec(projectID), etag)
	}
	id := access.NewID()
	return id, sinks.Insert(ctx, tx, id, s.spec(projectID), etag, actor)
}
