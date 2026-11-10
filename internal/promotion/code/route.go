package code

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	domain "github.com/tyk-swe/olp/internal/code"
	codeaccount "github.com/tyk-swe/olp/internal/code/account"
	codepool "github.com/tyk-swe/olp/internal/code/pool"
	coderoute "github.com/tyk-swe/olp/internal/code/route"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/provider"
)

type Route struct {
	ID        string
	Project   string
	Slug      string
	Draft     Policy
	Published *Publication
}

type scope struct {
	directory promotion.Directory
	bindings  bindings
	deps      provider.Deps
	accounts  []resolved[codeaccount.Input]
	pools     []resolved[codepool.Input]
}

type routeInput struct {
	draft     coderoute.Input
	published *frozen
}

func parseRoute(e contract.ConfigurationCodeRoute, prior []contract.ConfigurationCodeRoute, accounts []Account, pools []Pool, names promotion.Names, deps provider.Deps) (Route, error) {
	if slices.ContainsFunc(prior, func(other contract.ConfigurationCodeRoute) bool { return other.Slug == e.Slug }) {
		return Route{}, access.Invalid("code.routes.slug", "Use distinct route slugs.")
	}
	draft := Route{ID: e.Id.String(), Project: e.Project, Slug: e.Slug, Draft: policyOf(e.Draft)}
	if err := draft.validPolicy(draft.Draft, accounts, pools); err != nil {
		return Route{}, err
	}
	if e.Published == nil {
		return draft, nil
	}
	publication, err := parsePublication(*e.Published, names, deps)
	if err != nil {
		return Route{}, err
	}
	r := Route{ID: draft.ID, Project: draft.Project, Slug: draft.Slug, Draft: draft.Draft, Published: &publication}
	if err := r.validPaid(); err != nil {
		return Route{}, err
	}
	if err := r.validPolicy(publication.Policy, accounts, pools); err != nil {
		return Route{}, err
	}
	return r, r.validConnections(accounts, pools)
}

func (r Route) validPolicy(policy Policy, accounts []Account, pools []Pool) error {
	if access.ValidText("code", r.Project, 100) != nil || !slices.ContainsFunc(pools, func(p Pool) bool { return p.ID == policy.Pool && strings.EqualFold(p.Project, r.Project) }) {
		return access.Invalid("code.routes.pool", "Every pool must resolve within the route project.")
	}
	if policy.Tools != nil && !slices.ContainsFunc(accounts, func(a Account) bool { return a.ID == policy.Tools.AccountID && strings.EqualFold(a.Project, r.Project) }) {
		return access.Invalid("code.routes.tools", "Research accounts must resolve within the route project.")
	}
	for model, target := range policy.paidTargets() {
		if !slices.Contains(policy.Models, model) || access.ValidText("code", target.Provider, 100) != nil || access.ValidText("code", target.Slot, 100) != nil || access.ValidText("code", target.CredentialRef, 300) != nil {
			return access.Invalid("code.paid_targets", "Use declared models and named provider, slot and explicit credential references.")
		}
	}
	placeholder := func(targets map[string]PaidTarget) map[string]domain.PaidTarget {
		return promotion.MapValues(targets, func(PaidTarget) domain.PaidTarget { return domain.PaidTarget{ProviderID: r.ID, SlotID: r.ID} })
	}
	return policy.input(r.ID, r.Slug, policy.Pool, policy.Tools, placeholder(policy.PaidTargets), placeholder(policy.PaidFallbackTargets)).Validate()
}

func (r Route) validConnections(accounts []Account, pools []Pool) error {
	policy := r.Published.Policy
	if len(r.Published.Connections) == 0 && len(policy.PaidTargets)+len(policy.PaidFallbackTargets) == 0 {
		return access.Invalid("code.routes.published.connections", "Published routes require frozen provider connections.")
	}
	at := slices.IndexFunc(pools, func(p Pool) bool { return p.ID == policy.Pool })
	if at < 0 {
		return access.Invalid("code.routes.pool", "Every pool must resolve within the route project.")
	}
	members := pools[at].Accounts
	frozen := func(name string) bool {
		return slices.ContainsFunc(slices.Collect(maps.Keys(r.Published.Connections)), func(frozen string) bool { return strings.EqualFold(frozen, name) })
	}
	for name := range r.Published.Connections {
		if !slices.ContainsFunc(accounts, func(a Account) bool { _, member := members[a.ID]; return member && strings.EqualFold(a.Provider, name) }) {
			return access.Invalid("code.routes.published.connections", "Frozen providers must resolve through published pool accounts.")
		}
	}
	for ref := range members {
		if !slices.ContainsFunc(accounts, func(a Account) bool { return a.ID == ref && frozen(a.Provider) }) {
			return access.Invalid("code.routes.published.connections", "Freeze every published pool provider.")
		}
	}
	return nil
}

func (r Route) credentialRefs() []string {
	targets := func(policy Policy) []string {
		return slices.Collect(func(yield func(string) bool) {
			for _, target := range policy.paidTargets() {
				if !yield(target.CredentialRef) {
					return
				}
			}
		})
	}
	if r.Published == nil {
		return targets(r.Draft)
	}
	return slices.Concat(targets(r.Draft), targets(r.Published.Policy), slices.Concat(promotion.Map(slices.Collect(maps.Values(r.Published.PaidConnections)), PaidConnection.credentialRefs)...))
}

func (r Route) keyRefs() []string {
	if r.Published == nil {
		return []string{}
	}
	return slices.Concat(promotion.Map(slices.Collect(maps.Values(r.Published.PaidConnections)), PaidConnection.keyRefs)...)
}

func renderRoute(r Route) (contract.ConfigurationCodeRoute, error) {
	id, err := uuid.Parse(r.ID)
	if err != nil {
		return contract.ConfigurationCodeRoute{}, err
	}
	draft, err := renderPolicy(r.Draft)
	if err != nil {
		return contract.ConfigurationCodeRoute{}, err
	}
	published, err := renderPublication(r.Published)
	return contract.ConfigurationCodeRoute{Id: id, Project: r.Project, Slug: r.Slug, Draft: draft, Published: published}, err
}

func readRoutes(ctx context.Context, q access.Queryer) ([]Route, error) {
	stored, err := coderoute.ReadStored(ctx, q)
	if err != nil {
		return nil, err
	}
	slots, err := readSlotNames(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.TryMap(stored, func(s coderoute.Stored) (Route, error) { return portableRoute(ctx, q, s, slots) })
}

func portableRoute(ctx context.Context, q access.Queryer, s coderoute.Stored, slots map[string]PaidTarget) (Route, error) {
	draft, err := portablePolicy(s.Draft, slots)
	if err != nil {
		return Route{}, err
	}
	published, err := portablePublication(ctx, q, s.Published, s.Connections, s.Paid, slots)
	return Route{ID: s.ID, Project: s.Project, Slug: s.Slug, Draft: draft, Published: published}, err
}

func resolveRoute(ctx context.Context, q access.Queryer, r Route, in scope) (resolved[routeInput], error) {
	projectID, err := projectOf(in.directory, r.Project)
	if err != nil {
		return resolved[routeInput]{}, err
	}
	taken, err := coderoute.SlugTaken(ctx, q, r.Slug, projectID)
	if err != nil {
		return resolved[routeInput]{}, err
	}
	if taken {
		return resolved[routeInput]{}, access.Invalid("code.routes.slug", "Choose an unused slug or a code route in the destination project.")
	}
	ids, err := coderoute.IDs(ctx, q, r.Slug, projectID)
	if err != nil {
		return resolved[routeInput]{}, err
	}
	to, err := lookup(access.NewID(), ids)
	if err != nil {
		return resolved[routeInput]{}, err
	}
	published, err := r.frozen(ctx, q, projectID, in)
	if err != nil {
		return resolved[routeInput]{}, err
	}
	draft, err := r.Draft.resolve(ctx, q, r.Slug, projectID, in)
	return resolved[routeInput]{ref: r.ID, to: to, input: routeInput{draft: draft, published: published}}, err
}

func (r Route) frozen(ctx context.Context, q access.Queryer, projectID string, in scope) (*frozen, error) {
	if r.Published == nil {
		return nil, nil
	}
	return r.Published.resolve(ctx, q, r.Slug, projectID, in)
}

func routeChange(ctx context.Context, q access.Queryer, r resolved[routeInput]) (change, error) {
	action, republished, err := routeAction(ctx, q, r)
	return change{
		item: promotion.Item{Kind: contract.ConfigurationPlanItemKindCodeRoute, Key: r.ref, Action: action},
		write: func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
			return saveRoute(ctx, tx, p, r, republished)
		},
	}, err
}

func routeAction(ctx context.Context, q access.Queryer, r resolved[routeInput]) (contract.ConfigurationPlanItemAction, bool, error) {
	if !r.to.existing {
		return contract.ConfigurationPlanItemActionCreate, r.input.published != nil, nil
	}
	current, err := coderoute.ReadState(ctx, q, r.to.id)
	if err != nil {
		return "", false, err
	}
	sameDraft, err := same(inputOf(current.Draft), r.input.draft)
	if err != nil {
		return "", false, err
	}
	samePublication, err := r.input.published.same(current.Published, current.Connections, current.Paid)
	return replacement(!sameDraft || !samePublication), !samePublication, err
}

func saveRoute(ctx context.Context, tx pgx.Tx, p access.Principal, r resolved[routeInput], republished bool) error {
	published := r.input.published
	if republished && published != nil {
		pubDraft, err := coderoute.ParseDraft(published.Policy)
		if err != nil {
			return err
		}
		saved, err := coderoute.Save(ctx, tx, p, r.to.id, access.NewID(), pubDraft, r.to.existing)
		if err != nil {
			return err
		}
		if _, err := coderoute.SavePublication(ctx, tx, p, nil, r.to.id, saved.Route, published.Connections, published.Paid); err != nil {
			return err
		}
	}
	draft, err := coderoute.ParseDraft(r.input.draft)
	if err != nil {
		return err
	}
	if _, err := coderoute.Save(ctx, tx, p, r.to.id, access.NewID(), draft, r.to.existing || republished && published != nil); err != nil {
		return err
	}
	if republished && published == nil {
		return coderoute.Unpublish(ctx, tx, r.to.id)
	}
	return nil
}
