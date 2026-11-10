package route

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/auth"
	"github.com/tyk-swe/olp/internal/code/session"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type View struct {
	code.Route
	Adapters []code.Adapter `json:"adapters"`
}

const selectRows = `SELECT COALESCE(v.document,x.draft), COALESCE(v.connections,'{}'::jsonb), x.etag::text
	FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id`

func scanView(row pgx.Row) (View, error) {
	var docJSON, connJSON []byte
	var etag string
	if err := row.Scan(&docJSON, &connJSON, &etag); err != nil {
		return View{}, err
	}
	var r code.Route
	if err := json.Unmarshal(docJSON, &r); err != nil {
		return View{}, err
	}
	r.ETag = etag
	var connections map[string]vendor.ConnectionIdentity
	if len(connJSON) > 0 {
		if err := json.Unmarshal(connJSON, &connections); err != nil {
			return View{}, err
		}
	}
	return View{Route: r, Adapters: vendor.AdaptersForConnections(connections)}, nil
}

func Load(ctx context.Context, q access.Queryer, id string) (View, error) {
	return scanView(q.QueryRow(ctx, selectRows+` WHERE x.id=$1`, id))
}

func Save(ctx context.Context, tx pgx.Tx, p access.Principal, id, etag string, d Draft, existing bool) (View, error) {
	in := d.input
	project := in.ProjectID
	if err := p.Project(&project, access.Change); err != nil {
		return View{}, err
	}
	var matches, conflict bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_pools WHERE id=$1 AND project_id=$2),
		EXISTS(SELECT 1 FROM olp.routes WHERE slug=$3) OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE slug=$3)`,
		in.PoolID, in.ProjectID, in.Slug).Scan(&matches, &conflict); err != nil {
		return View{}, err
	}
	if !matches {
		return View{}, access.NotFound
	}
	if conflict {
		return View{}, access.Fail(409, "slug_in_use", "That slug is already used by a route.")
	}
	candidate := in.toRoute()
	candidate.ID = id
	candidate.ETag = etag
	if in.Tools != nil {
		if _, err := session.ResearchAccount(ctx, tx, candidate); err != nil {
			if errors.Is(err, session.ErrResearchAccountUnavailable) {
				return View{}, access.Invalid("tools", "Choose an active enrolled Z.ai global account in this route's pool.")
			}
			return View{}, err
		}
	}
	if _, err := PaidProviders(ctx, tx, candidate); err != nil {
		return View{}, err
	}
	if existing {
		var oldSlug string
		var oldDoc []byte
		if err := tx.QueryRow(ctx, `SELECT slug, COALESCE((SELECT document FROM olp.code_route_revisions WHERE id=latest_revision_id), draft) FROM olp.code_routes WHERE id=$1`, id).Scan(&oldSlug, &oldDoc); err != nil {
			return View{}, err
		}
		if oldSlug != in.Slug {
			return View{}, access.Invalid("slug", "Route slugs are immutable.")
		}
		var previous code.Route
		if len(oldDoc) > 0 && json.Unmarshal(oldDoc, &previous) == nil {
			candidate.RevisionID = previous.RevisionID
			candidate.Revision = previous.Revision
			candidate.PublishedAt = previous.PublishedAt
		}
	}
	draftJSON, err := json.Marshal(candidate)
	if err != nil {
		return View{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.code_routes(id,project_id,slug,draft,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(id) DO UPDATE SET draft=excluded.draft,etag=excluded.etag`,
		id, in.ProjectID, in.Slug, draftJSON, etag, p.UserID()); err != nil {
		return View{}, err
	}
	return Load(ctx, tx, id)
}

func Publish(ctx context.Context, tx pgx.Tx, p access.Principal, host *plugins.Host, id string) (View, error) {
	var draftJSON []byte
	if err := tx.QueryRow(ctx, `SELECT draft FROM olp.code_routes WHERE id=$1`, id).Scan(&draftJSON); err != nil {
		return View{}, err
	}
	var r code.Route
	if err := json.Unmarshal(draftJSON, &r); err != nil {
		return View{}, err
	}
	return SavePublication(ctx, tx, p, host, id, r, nil, nil)
}

func SavePublication(
	ctx context.Context,
	tx pgx.Tx,
	p access.Principal,
	host *plugins.Host,
	id string,
	in code.Route,
	frozen map[string]routing.Configuration,
	frozenPaid map[string]routing.Provider,
) (View, error) {
	project, err := access.ParseUUID(in.ProjectID)
	if err != nil {
		return View{}, err
	}
	if err := p.Project(&project, access.Change); err != nil {
		return View{}, err
	}
	r := in
	if err := r.ValidateAliases(); err != nil {
		return View{}, access.Invalid("aliases", err.Error())
	}
	if err := r.ValidateModelPools(); err != nil {
		return View{}, access.Invalid("model_pools", err.Error())
	}
	if err := r.Tools.Validate(); err != nil {
		return View{}, access.Invalid("tools", err.Error())
	}
	if r.Tools != nil {
		if _, err := session.ResearchAccount(ctx, tx, r); err != nil {
			if errors.Is(err, session.ErrResearchAccountUnavailable) {
				return View{}, access.Invalid("tools", "Choose an active enrolled Z.ai global account in this route's pool.")
			}
			return View{}, err
		}
	}
	if r.Enabled {
		for _, model := range r.Models {
			if _, paid := r.AllPaidTargets()[model]; paid {
				continue
			}
			var available bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_pool_accounts pa JOIN olp.code_accounts a ON a.id=pa.account_id JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id
				WHERE pa.pool_id=$1 AND a.enabled AND a.project_id=$2 AND a.models ? $3 AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now()) AND c.principal=a.principal)`, r.PoolID, r.ProjectID, model).Scan(&available)
			if err != nil {
				return View{}, err
			}
			if !available {
				return View{}, access.Invalid("models", "Every model needs an enrolled pool account.")
			}
		}
	}
	paidProviders, err := PaidProviders(ctx, tx, r)
	if err != nil {
		return View{}, err
	}
	if frozenPaid != nil {
		if len(frozenPaid) != len(paidProviders) {
			return View{}, access.Invalid("paid_connections", "Frozen metered providers must match the published paid targets.")
		}
		for providerID := range frozenPaid {
			if _, ok := paidProviders[providerID]; !ok {
				return View{}, access.Invalid("paid_connections", "Frozen metered providers must match the published paid targets.")
			}
		}
		if err := ValidatePaidProviders(r, frozenPaid); err != nil {
			return View{}, err
		}
		paidProviders = frozenPaid
	}
	connections, adapters, err := poolConnections(ctx, tx, host, r.PoolID)
	if err != nil {
		return View{}, err
	}
	if frozen != nil {
		if len(connections) != len(frozen) {
			return View{}, access.Invalid("connections", "Frozen providers must match the publication pool.")
		}
		for providerID, cfg := range frozen {
			if _, ok := connections[providerID]; !ok {
				return View{}, access.Invalid("connections", "Frozen providers must match the publication pool.")
			}
			if _, ok := vendor.ForConnection(cfg.Kind, cfg.AuthMode, cfg.ProfileID); !ok {
				return View{}, access.Invalid("connections", "Frozen connections must support a coding adapter.")
			}
		}
		connections = frozen
	}
	if len(connections) == 0 && len(paidProviders) == 0 {
		return View{}, access.Invalid("pool_id", "Assign at least one account using a supported subscription provider profile or one supported paid API target before publishing.")
	}
	encodedConnections, err := json.Marshal(connections)
	if err != nil {
		return View{}, err
	}
	encodedPaid, err := json.Marshal(paidProviders)
	if err != nil {
		return View{}, err
	}
	r.RevisionID = access.NewID()
	r.ETag = access.NewID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	r.PublishedAt = &now
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0)+1 FROM olp.code_route_revisions WHERE route_id=$1`, id).Scan(&r.Revision); err != nil {
		return View{}, err
	}
	document, err := json.Marshal(r)
	if err != nil {
		return View{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO olp.code_route_revisions(id,route_id,revision,document,created_by,published_at,connections,paid_connections)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, r.RevisionID, id, r.Revision, document, p.UserID(), now, encodedConnections, encodedPaid); err != nil {
		return View{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.code_routes SET latest_revision_id=$2,etag=$3,draft=$4 WHERE id=$1`, id, r.RevisionID, r.ETag, document); err != nil {
		return View{}, err
	}
	return View{Route: r, Adapters: adapters}, nil
}

func poolConnections(ctx context.Context, q access.Queryer, host *plugins.Host, poolID string) (map[string]routing.Configuration, []code.Adapter, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT p.id::text,p.configuration FROM olp.code_pool_accounts pa
		JOIN olp.code_accounts a ON a.id=pa.account_id JOIN olp.providers p ON p.id=a.provider_id
		WHERE pa.pool_id=$1 AND p.project_id=a.project_id AND p.state<>'disabled'`, poolID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	connections := map[string]routing.Configuration{}
	identities := map[string]vendor.ConnectionIdentity{}
	for rows.Next() {
		var id string
		var raw []byte
		var c routing.Configuration
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, nil, err
		}
		if err = json.Unmarshal(raw, &c); err != nil {
			return nil, nil, err
		}
		v, ok := vendor.ForConnection(c.Kind, c.AuthMode, c.ProfileID)
		if !ok {
			return nil, nil, access.Invalid("pool_id", "Every pool provider connection must use a supported coding subscription profile.")
		}
		if host != nil && auth.ValidateConnection(ctx, host, c, v) != nil {
			return nil, nil, access.Fail(422, "code_provider_unsupported", "Code-mode provider connections require an installed matching plugin and no request mutation.")
		}
		connections[id] = c
		identities[id] = vendor.ConnectionIdentity{Kind: c.Kind, AuthMode: c.AuthMode, ProfileID: c.ProfileID}
	}
	return connections, vendor.AdaptersForConnections(identities), rows.Err()
}

func PaidProviders(ctx context.Context, q access.Queryer, route code.Route) (map[string]routing.Provider, error) {
	if len(route.PaidTargets)+len(route.PaidFallbackTargets) == 0 {
		return map[string]routing.Provider{}, nil
	}
	ids := slices.Collect(func(yield func(string) bool) {
		for _, target := range route.AllPaidTargets() {
			if !yield(target.ProviderID) {
				return
			}
		}
	})
	rows, err := q.Query(ctx, `SELECT p.id::text,p.state,r.id::text,r.name,r.configuration,r.models,r.slots,p.project_id::text,`+release.PluginColumn+` FROM olp.providers p JOIN olp.provider_revisions r ON r.id=p.active_revision_id WHERE p.id=ANY($1::uuid[]) AND p.project_id=$2 AND p.state<>'disabled'`, ids, route.ProjectID)
	if err != nil {
		return nil, err
	}
	providers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (routing.Provider, error) {
		var revision routing.ProviderRevision
		if err := row.Scan(&revision.ID, &revision.State, &revision.RevisionID, &revision.Name, &revision.Configuration, &revision.Models, &revision.Slots, &revision.ProjectID, &revision.InstalledPlugin); err != nil {
			return routing.Provider{}, err
		}
		return routing.DecodeProviderRevision(revision)
	})
	if err != nil {
		return nil, err
	}
	published := maps.Collect(func(yield func(string, routing.Provider) bool) {
		for _, provider := range providers {
			if !yield(provider.ID, provider) {
				return
			}
		}
	})
	if err := ValidatePaidProviders(route, published); err != nil {
		return nil, err
	}
	return published, nil
}

func ValidatePaidProviders(route code.Route, published map[string]routing.Provider) error {
	for model, target := range route.AllPaidTargets() {
		provider, exists := published[target.ProviderID]
		if !exists {
			return access.Invalid("paid_targets", "Choose a published metered provider in this project.")
		}
		if _, ok := vendor.ForConnection(provider.Kind, provider.AuthMode, provider.ProfileID); ok {
			return access.Invalid("paid_targets", "Coding subscriptions belong in the subscription pool.")
		}
		if _, _, ok := routing.CodePaidProtocol(provider, model); !ok {
			return access.Invalid("paid_targets", "Use a native metered API profile without request defaults, model overrides, plugins or caller credentials.")
		}
		slot := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool {
			return slot.ID == target.SlotID && slot.Enabled && slot.CredentialID != nil && slot.CredentialVersion != nil && slot.AllowsModelRoute(model, route.Slug)
		})
		if slot < 0 {
			return access.Invalid("paid_targets", "Choose an enabled credential slot from the selected provider.")
		}
	}
	return nil
}

func Unpublish(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE olp.code_routes SET latest_revision_id=NULL,etag=$2 WHERE id=$1`, id, access.NewID())
	return err
}

type Stored struct {
	ID          string
	Project     string
	Slug        string
	Draft       code.Route
	Published   *code.Route
	Connections map[string]routing.Configuration
	Paid        map[string]routing.Provider
}

type State struct {
	Draft       code.Route
	Published   *code.Route
	Connections map[string]routing.Configuration
	Paid        map[string]routing.Provider
}

func ReadStored(ctx context.Context, q access.Queryer) ([]Stored, error) {
	rows, err := q.Query(ctx, `SELECT x.id::text,p.name,x.slug,x.draft,v.document,coalesce((SELECT jsonb_object_agg(p.name,c.value) FROM jsonb_each(v.connections) c JOIN olp.providers p ON p.id::text=c.key),'{}'),coalesce(v.paid_connections,'{}') FROM olp.code_routes x JOIN olp.projects p ON p.id=x.project_id LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Stored])
}

func ReadState(ctx context.Context, q access.Queryer, id string) (State, error) {
	var state State
	err := q.QueryRow(ctx, `SELECT x.draft,v.document,coalesce(v.connections,'{}'),coalesce(v.paid_connections,'{}') FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id WHERE x.id=$1`, id).Scan(&state.Draft, &state.Published, &state.Connections, &state.Paid)
	return state, err
}

func SlugTaken(ctx context.Context, q access.Queryer, slug, projectID string) (bool, error) {
	var taken bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.code_routes WHERE slug=$1 AND project_id<>$2)`, slug, projectID).Scan(&taken)
	return taken, err
}

func IDs(ctx context.Context, q access.Queryer, slug, projectID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id::text FROM olp.code_routes WHERE slug=$1 AND project_id=$2`, slug, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
