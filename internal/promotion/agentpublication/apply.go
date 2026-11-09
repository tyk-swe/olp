package agentpublication

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents/publications"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Deps struct {
	Keys             *secrets.KeyRing
	Installation     string
	Egress           *egress.Policy
	RuntimePublisher func(context.Context, pgx.Tx, string) error
}

type binder struct {
	tx       pgx.Tx
	deps     Deps
	project  string
	bindings promotion.Bindings
}

func Applier(desired []Publication, bindings promotion.Bindings, deps Deps) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		if len(desired) == 0 {
			return nil
		}
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		for _, publication := range slices.SortedStableFunc(slices.Values(desired), toolsetsFirst) {
			if err := apply(ctx, tx, p, directory, bindings, deps, publication); err != nil {
				return err
			}
		}
		if deps.RuntimePublisher == nil {
			return nil
		}
		return deps.RuntimePublisher(ctx, tx, p.UserID())
	}
}

func toolsetsFirst(a, b Publication) int {
	_, before := a.Kind.(Toolset)
	_, after := b.Kind.(Toolset)
	switch {
	case before != after && before:
		return -1
	case before != after:
		return 1
	case a.Kind.name() != b.Kind.name():
		return strings.Compare(a.Kind.name(), b.Kind.name())
	}
	return byKey(a, b)
}

func apply(ctx context.Context, tx pgx.Tx, p access.Principal, directory project.Directory, bindings promotion.Bindings, deps Deps, publication Publication) error {
	projectID, ok := directory.ID(publication.Project)
	if !ok {
		return access.Fail(http.StatusConflict, "project_unknown", "Agent publication project "+publication.Project+" is not installed.")
	}
	if err := p.Project(&projectID, access.Change); err != nil {
		return err
	}
	current, err := find(ctx, tx, projectID, publication, true)
	if err != nil {
		return err
	}
	previous, err := publishedDefinition(ctx, tx, current)
	if err != nil {
		return err
	}
	b := binder{tx: tx, deps: deps, project: projectID, bindings: bindings}
	draft, err := b.bind(ctx, publication.Draft, publication.Kind.credentials().Credential, draftOf(current))
	if err != nil {
		return err
	}
	if err := publications.AuthorizeTrustChange(p, current, draft); err != nil {
		return err
	}
	next, err := publications.SaveDraft(ctx, tx, p.UserID(), current, publication.Kind.name(), draft, deps.Egress)
	if err != nil {
		return err
	}
	published, err := publishDeclared(ctx, tx, p, b, publication, next, current, previous, draft)
	if err != nil {
		return err
	}
	if err := applyLabels(ctx, tx, p, next, publication, projectID); err != nil {
		return err
	}
	if err := publications.SetLatestRevision(ctx, tx, next.ID, published); err != nil {
		return err
	}
	_, err = access.AdvanceAuthority(ctx, tx)
	return err
}

func publishDeclared(ctx context.Context, tx pgx.Tx, p access.Principal, b binder, publication Publication, next agentcontract.Publication, current *agentcontract.Publication, previous *agentcontract.Draft, draft agentcontract.Draft) (*string, error) {
	if publication.Published == nil {
		return nil, nil
	}
	definition, err := b.bindPublished(ctx, *publication.Published, publication.Kind.credentials(), draft, previous)
	if err != nil {
		return nil, err
	}
	if err := publications.AuthorizeTrustChange(p, priorOf(current, previous), definition); err != nil {
		return nil, err
	}
	revision, err := publishDefinition(ctx, tx, p.UserID(), next, definition)
	return revision.RevisionID, err
}

func applyLabels(ctx context.Context, tx pgx.Tx, p access.Principal, next agentcontract.Publication, publication Publication, projectID string) error {
	prompt, ok := publication.Kind.(Prompt)
	if !ok {
		return nil
	}
	if err := publications.ClearLabels(ctx, tx, next.ID); err != nil {
		return err
	}
	for _, label := range labelNames {
		definition, declared := prompt.Labels[label]
		if !declared {
			continue
		}
		labeled, err := publishDefinition(ctx, tx, p.UserID(), next, placed(definition, projectID))
		if err != nil {
			return err
		}
		if err := publications.SetLabel(ctx, tx, labeled, label, *labeled.RevisionID, access.NewID()); err != nil {
			return err
		}
	}
	return nil
}

func publishDefinition(ctx context.Context, tx pgx.Tx, actor string, p agentcontract.Publication, definition agentcontract.Draft) (agentcontract.Publication, error) {
	draft := agentcontract.NormalizeDraft(definition)
	id, found, err := publications.RevisionIDByDigest(ctx, tx, p.ID, agentcontract.DraftDigest(draft))
	if err != nil {
		return agentcontract.Publication{}, err
	}
	if found {
		return agentcontract.Publication{ID: p.ID, Kind: p.Kind, ProjectID: p.ProjectID, Name: p.Name, ETag: p.ETag, Draft: draft, RevisionID: &id, Retired: p.Retired}, nil
	}
	return publications.Publish(ctx, tx, actor, agentcontract.Publication{ID: p.ID, Kind: p.Kind, ProjectID: p.ProjectID, Name: p.Name, ETag: p.ETag, Draft: draft, RevisionID: p.RevisionID, Retired: p.Retired})
}

func priorOf(current *agentcontract.Publication, previous *agentcontract.Draft) *agentcontract.Publication {
	if previous == nil {
		return current
	}
	return &agentcontract.Publication{Draft: *previous}
}

func (b binder) bindPublished(ctx context.Context, definition agentcontract.Draft, agent Agent, draft agentcontract.Draft, prior *agentcontract.Draft) (agentcontract.Draft, error) {
	if agent.PublishedCredential != nil && agent.Credential != nil && *agent.PublishedCredential == *agent.Credential {
		return publications.WithCredential(placed(definition, b.project), credentialID(&draft)), nil
	}
	return b.bind(ctx, definition, agent.PublishedCredential, prior)
}

func (b binder) bind(ctx context.Context, definition agentcontract.Draft, ref *promotion.Ref, prior *agentcontract.Draft) (agentcontract.Draft, error) {
	base := placed(publications.WithCredential(definition, nil), b.project)
	if base.Agent == nil || ref == nil {
		return base, nil
	}
	reusable := reusableCredential(prior, &base)
	sealed, bound := b.bindings[*ref].(promotion.Sealed)
	if !bound && !reusable {
		return base, access.Invalid("agent_publications.credential_ref", "Supply the required destination credential; endpoint changes cannot inherit it.")
	}
	if !bound {
		return publications.WithCredential(base, prior.Agent.CredentialID), nil
	}
	material := []byte(sealed.Secret)
	defer clear(material)
	if reusable {
		matched, err := publications.CredentialMatches(ctx, b.tx, b.deps.Keys, b.deps.Installation, b.project, *prior.Agent.CredentialID, material)
		if err != nil || matched {
			return publications.WithCredential(base, prior.Agent.CredentialID), err
		}
	}
	id, err := b.store(ctx, base, material)
	return publications.WithCredential(base, &id), err
}

func (b binder) store(ctx context.Context, draft agentcontract.Draft, material []byte) (string, error) {
	if draft.Agent.OAuth != nil && draft.Agent.OAuth.EntraOBO != nil {
		return publications.StoreEntraCredential(ctx, b.tx, b.deps.Keys, b.deps.Installation, b.project, material)
	}
	return publications.StoreCredential(ctx, b.tx, b.deps.Keys, b.deps.Installation, b.project, material)
}
