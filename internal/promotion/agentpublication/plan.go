package agentpublication

import (
	"context"
	"errors"
	"reflect"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents/publications"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

const planDetail = "Promote reviewed immutable configuration only; execution metadata and secrets remain local."

type slot struct {
	ref            *promotion.Ref
	desired, prior *agentcontract.Draft
}

func Planner(desired []Publication, names promotion.Names, bindings promotion.Bindings) promotion.Planner {
	if len(desired) == 0 {
		return promotion.Planner{}
	}
	return promotion.Planner{
		Claims: slices.Collect(func(yield func(promotion.Claim) bool) {
			for _, publication := range desired {
				for _, ref := range publication.refs() {
					if !yield(promotion.Claim{Ref: ref, Accept: promotion.AcceptBearer}) {
						return
					}
				}
			}
		}),
		Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
			directory, err := project.ReadDirectory(ctx, q)
			if err != nil {
				return nil, err
			}
			items, err := promotion.TryMap(desired, func(publication Publication) ([]promotion.Item, error) {
				return planPublication(ctx, q, names, directory, bindings, publication)
			})
			return slices.Concat(items...), err
		},
	}
}

func planPublication(ctx context.Context, q access.Queryer, names promotion.Names, directory project.Directory, bindings promotion.Bindings, publication Publication) ([]promotion.Item, error) {
	key := publication.key()
	projectID, present := directory.ID(publication.Project)
	if !present && !names.Project(publication.Project) {
		return []promotion.Item{promotion.Conflict(kind, key, "The owning project is not declared or present.")}, nil
	}
	current, err := findPresent(ctx, q, projectID, present, publication)
	if err != nil {
		return nil, err
	}
	previous, err := publishedDefinition(ctx, q, current)
	if err != nil {
		return nil, err
	}
	agent, _ := publication.Kind.(Agent)
	unbound := slices.Collect(func(yield func(promotion.Item) bool) {
		for _, slot := range []slot{{agent.Credential, &publication.Draft, draftOf(current)}, {agent.PublishedCredential, publication.Published, previous}} {
			if slot.ref != nil && bindings[*slot.ref] == nil && !reusableCredential(slot.prior, slot.desired) && !yield(promotion.Blocker(kind, key, "Supply the destination agent bearer credential binding for the declared definition.")) {
				return
			}
		}
	})
	blocked, err := referencesBlocked(ctx, q, publication, current, referenceProject(projectID, present))
	if err != nil {
		return nil, err
	}
	return slices.Concat(unbound, when(blocked, promotion.Blocker(kind, key, "Rebind missing immutable destination references explicitly.")), []promotion.Item{
		{Kind: kind, Key: key, Action: actionOf(current, publication, bindings), Detail: planDetail},
	}), nil
}

func referencesBlocked(ctx context.Context, q access.Queryer, publication Publication, current *agentcontract.Publication, projectID string) (bool, error) {
	retained, err := retainedDigests(ctx, q, current)
	if err != nil {
		return false, err
	}
	for index, definition := range publication.definitions() {
		digest := exportedDigest(definition)
		if (index == 0 && current != nil && digest == exportedDigest(current.Draft)) || (index > 0 && slices.Contains(retained, digest)) {
			continue
		}
		err := publications.ValidateReferences(ctx, q, publication.Kind.name(), placed(definition, projectID))
		if _, expected := errors.AsType[*access.Problem](err); expected {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	return false, nil
}

func actionOf(current *agentcontract.Publication, publication Publication, bindings promotion.Bindings) contract.ConfigurationPlanItemAction {
	if current == nil {
		return contract.ConfigurationPlanItemActionCreate
	}
	rebinds := slices.ContainsFunc(publication.refs(), func(ref promotion.Ref) bool { return bindings[ref] != nil })
	if rebinds || publication.Published != nil || current.RevisionID != nil || len(publication.labels()) != 0 || !reflect.DeepEqual(exportDraft(current.Draft), publication.Draft) {
		return contract.ConfigurationPlanItemActionReplace
	}
	return contract.ConfigurationPlanItemActionReuse
}

func reusableCredential(prior, next *agentcontract.Draft) bool {
	if prior == nil || next == nil || prior.Agent == nil || next.Agent == nil || prior.Agent.Endpoint != next.Agent.Endpoint || prior.Agent.CredentialID == nil {
		return false
	}
	before, after := prior.Agent.OAuth, next.Agent.OAuth
	priorEntra := before != nil && before.EntraOBO != nil
	if priorEntra != (after != nil && after.EntraOBO != nil) {
		return false
	}
	return !priorEntra || (before.ClientID == after.ClientID && before.TokenEndpoint == after.TokenEndpoint && before.EntraOBO.Issuer == after.EntraOBO.Issuer)
}

func findPresent(ctx context.Context, q access.Queryer, projectID string, present bool, publication Publication) (*agentcontract.Publication, error) {
	if !present {
		return nil, nil
	}
	return find(ctx, q, projectID, publication, false)
}

func find(ctx context.Context, q access.Queryer, projectID string, publication Publication, lock bool) (*agentcontract.Publication, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM olp.agent_publications WHERE NOT retired AND project_id=$1 AND kind=$2 AND lower(name)=lower($3)`, projectID, publication.Kind.name(), publication.Draft.Name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return publications.Load(ctx, q, id, publication.Kind.name(), lock)
}

func publishedDefinition(ctx context.Context, q access.Queryer, current *agentcontract.Publication) (*agentcontract.Draft, error) {
	if current == nil || current.RevisionID == nil {
		return nil, nil
	}
	revision, err := publications.LoadRevision(ctx, q, current.ID, *current.RevisionID)
	if err != nil {
		return nil, err
	}
	return &revision.Definition, nil
}

func retainedDigests(ctx context.Context, q access.Queryer, current *agentcontract.Publication) ([]string, error) {
	if current == nil {
		return nil, nil
	}
	rows, err := q.Query(ctx, `SELECT definition FROM olp.agent_publication_revisions WHERE publication_id=$1`, current.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		definition, err := pgx.RowTo[agentcontract.Draft](row)
		return exportedDigest(definition), err
	})
}

func draftOf(current *agentcontract.Publication) *agentcontract.Draft {
	if current == nil {
		return nil
	}
	return &current.Draft
}

func referenceProject(projectID string, present bool) string {
	if !present {
		return placeholderProject
	}
	return projectID
}

func when(condition bool, item promotion.Item) []promotion.Item {
	if !condition {
		return nil
	}
	return []promotion.Item{item}
}
