package routes

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

func (s *Server) registerCodeMode(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/code/routes", s.codeRoutes)
	s.Access.Route(mux, "POST /api/v1/code/routes", s.writeCodeRoute)
	s.Access.Route(mux, "PUT /api/v1/code/routes/{id}", s.writeCodeRoute)
	s.Access.Route(mux, "POST /api/v1/code/routes/{id}/publish", s.publishCodeRoute)
	s.Access.Route(mux, "GET /api/v1/code/routes/{id}/revisions", s.codeRevisions)
	s.Access.Route(mux, "GET /api/v1/code/routes/{id}/client-config", s.CodeClientConfiguration)
}

// codeRoute is a route as management returns it: with the adapters its latest
// published revision serves, derived from the connections it froze.
type codeRoute struct {
	codemode.Route
	Adapters []codemode.Adapter `json:"adapters"`
}

func (s *Server) codeRoutes(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.Access.CodeList(r, p, `SELECT x.draft||jsonb_build_object('etag',x.etag,
		'revision_id',coalesce(v.id::text,''),'revision',coalesce(v.revision,0),'published_at',v.published_at)
		||jsonb_build_object('adapters',`+codeadapter.SQLAdapters("v.connections")+`)
		FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id`)
}

type codeRouteInput struct {
	ChatToAntigravity       bool                           `json:"chat_to_antigravity"`
	ModelPools              map[string][]string            `json:"model_pools"`
	InitialRecoveryAttempts int                            `json:"initial_recovery_attempts"`
	ModelRewriting          bool                           `json:"model_rewriting"`
	Aliases                 map[string]string              `json:"aliases"`
	MessagesToCodex         bool                           `json:"messages_to_codex"`
	Tools                   *codemode.ResearchTools        `json:"tools"`
	MaxBodyBytes            *int64                         `json:"max_body_bytes,omitempty"`
	ProjectID               string                         `json:"project_id"`
	Slug                    string                         `json:"slug"`
	PoolID                  string                         `json:"pool_id"`
	Models                  []string                       `json:"models"`
	Enabled                 bool                           `json:"enabled"`
	PaidParticipation       bool                           `json:"paid_participation"`
	PaidTargets             map[string]codemode.PaidTarget `json:"paid_targets"`
	PaidFallbackTargets     map[string]codemode.PaidTarget `json:"paid_fallback_targets"`
}

func (s *Server) writeCodeRoute(r *http.Request, _ access.Principal) (access.Reply, error) {
	var in codeRouteInput
	if err := access.Decode(r, &in); err != nil {
		return access.Reply{}, err
	}
	if in.InitialRecoveryAttempts < 0 || in.InitialRecoveryAttempts > 2 {
		return access.Reply{}, access.Invalid("initial_recovery_attempts", "Use zero to disable recovery or allow at most two additional attempts.")
	}
	if !bodylimit.Valid(in.MaxBodyBytes) {
		return access.Reply{}, access.Invalid("max_body_bytes", "Use 1 to 1073741824 bytes, or null to inherit the installation limit.")
	}
	projectID, err := access.ParseUUID(in.ProjectID)
	if err != nil {
		return access.Reply{}, err
	}
	in.ProjectID = projectID
	if !access.RouteSlug.MatchString(in.Slug) {
		return access.Reply{}, access.Invalid("slug", "Use a route slug.")
	}
	if in.PoolID, err = access.ParseUUID(in.PoolID); err != nil {
		return access.Reply{}, err
	}
	if err := codemode.ValidateModels(in.Models); err != nil {
		return access.Reply{}, access.Invalid("models", err.Error())
	}
	if err := (codemode.Route{Models: in.Models, Aliases: in.Aliases, ModelRewriting: in.ModelRewriting}).ValidateAliases(); err != nil {
		return access.Reply{}, access.Invalid("aliases", err.Error())
	}
	if err := (codemode.Route{Models: in.Models, Aliases: in.Aliases, ModelRewriting: in.ModelRewriting, ModelPools: in.ModelPools}).ValidateModelPools(); err != nil {
		return access.Reply{}, access.Invalid("model_pools", err.Error())
	}
	if err := (codemode.Route{Models: in.Models, PaidParticipation: in.PaidParticipation, PaidTargets: in.PaidTargets, PaidFallbackTargets: in.PaidFallbackTargets, InitialRecoveryAttempts: in.InitialRecoveryAttempts}).ValidatePaidTargets(); err != nil {
		return access.Reply{}, access.Invalid("paid_targets", err.Error())
	}
	if err := in.Tools.Validate(); err != nil {
		return access.Reply{}, access.Invalid("tools", err.Error())
	}
	return s.Access.CodeWrite(r, "code_routes", "code_route", in.ProjectID, in, func(tx pgx.Tx, p access.Principal, id, etag string) (any, error) {
		out := codemode.Route{ChatToAntigravity: in.ChatToAntigravity, PaidParticipation: in.PaidParticipation, PaidTargets: in.PaidTargets, PaidFallbackTargets: in.PaidFallbackTargets, ModelPools: in.ModelPools, InitialRecoveryAttempts: in.InitialRecoveryAttempts, Aliases: in.Aliases, ModelRewriting: in.ModelRewriting, MessagesToCodex: in.MessagesToCodex, Tools: in.Tools, MaxBodyBytes: in.MaxBodyBytes, ID: id, ProjectID: in.ProjectID, Slug: in.Slug, PoolID: in.PoolID, Models: in.Models, Enabled: in.Enabled, ETag: etag}
		adapters := []codemode.Adapter{}
		var matches bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.code_pools WHERE id=$1 AND project_id=$2)`, in.PoolID, in.ProjectID).Scan(&matches); err != nil {
			return nil, err
		}
		if !matches {
			return nil, access.Fail(404, "not_found", "The pool was not found.")
		}
		if out.Tools != nil {
			if _, err := resources.ResearchAccount(r.Context(), tx, out); err != nil {
				return nil, err
			}
		}
		if r.PathValue("id") != "" {
			var slug string
			if err := tx.QueryRow(r.Context(), `SELECT x.slug,coalesce(v.id::text,''),coalesce(v.revision,0),v.published_at,`+codeadapter.SQLAdapters("v.connections")+`
				FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id WHERE x.id=$1`, id).Scan(&slug, &out.RevisionID, &out.Revision, &out.PublishedAt, &adapters); err != nil {
				return nil, err
			}
			if slug != in.Slug {
				return nil, access.Invalid("slug", "The route slug is immutable.")
			}
		}
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE slug=$1)`, in.Slug).Scan(&matches); err != nil {
			return nil, err
		}
		if matches {
			return nil, access.Invalid("slug", "This slug belongs to an ordinary route.")
		}
		document, _ := json.Marshal(out)
		_, err := tx.Exec(r.Context(), `INSERT INTO olp.code_routes(id,project_id,slug,draft,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT(id) DO UPDATE SET draft=excluded.draft,etag=excluded.etag`, id, in.ProjectID, in.Slug, document, etag, p.UserID())
		return codeRoute{Route: out, Adapters: adapters}, err
	})
}

func (s *Server) publishCodeRoute(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var document []byte
	var project, etag string
	if err = tx.QueryRow(r.Context(), `SELECT draft,project_id::text,etag::text FROM olp.code_routes WHERE id=$1`, id).Scan(&document, &project, &etag); err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&project, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	var route codemode.Route
	if err = json.Unmarshal(document, &route); err != nil {
		return access.Reply{}, err
	}
	if err := route.ValidatePaidTargets(); err != nil {
		return access.Reply{}, access.Invalid("paid_targets", err.Error())
	}
	if err := route.ValidateAliases(); err != nil {
		return access.Reply{}, access.Invalid("aliases", err.Error())
	}
	if err := route.ValidateModelPools(); err != nil {
		return access.Reply{}, access.Invalid("model_pools", err.Error())
	}
	if err = route.Tools.Validate(); err != nil {
		return access.Reply{}, access.Invalid("tools", err.Error())
	}
	if route.Tools != nil {
		if _, err = resources.ResearchAccount(r.Context(), tx, route); err != nil {
			return access.Reply{}, err
		}
	}
	if route.Enabled {
		for _, model := range route.Models {
			if _, paid := route.AllPaidTargets()[model]; paid {
				continue
			}
			var available bool
			err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.code_pool_accounts pa JOIN olp.code_accounts a ON a.id=pa.account_id JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id
				WHERE pa.pool_id=$1 AND a.enabled AND a.project_id=$2 AND a.models ? $3 AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now()) AND c.principal=a.principal)`, route.PoolID, project, model).Scan(&available)
			if err != nil {
				return access.Reply{}, err
			}
			if !available {
				return access.Reply{}, access.Invalid("models", "Every model needs an enrolled pool account.")
			}
		}
	}
	paidConnections, paidErr := codePaidPublication(r.Context(), tx, route)
	if paidErr != nil {
		return access.Reply{}, paidErr
	}
	connections, adapters, err := codePublication(r.Context(), tx, route.PoolID)
	if err != nil {
		return access.Reply{}, err
	}
	route.RevisionID = access.NewID()
	route.ETag = access.NewID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	route.PublishedAt = &now
	if err = tx.QueryRow(r.Context(), `SELECT COALESCE(max(revision),0)+1 FROM olp.code_route_revisions WHERE route_id=$1`, id).Scan(&route.Revision); err != nil {
		return access.Reply{}, err
	}
	document, _ = json.Marshal(route)
	if _, err = tx.Exec(r.Context(), `INSERT INTO olp.code_route_revisions(id,route_id,revision,document,created_by,published_at,connections,paid_connections)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, route.RevisionID, id, route.Revision, document, p.UserID(), now, connections, paidConnections); err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), `UPDATE olp.code_routes SET latest_revision_id=$2,etag=$3,draft=$4 WHERE id=$1`, id, route.RevisionID, route.ETag, document); err != nil {
		return access.Reply{}, err
	}
	if _, err = runtime.Publish(r.Context(), tx, p.UserID()); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "code_route.publish", "code_route", id, "success"); err != nil {
		return access.Reply{}, err
	}
	out := access.Detail(codeRoute{Route: route, Adapters: adapters}, route.ETag)
	if err = s.Access.CompleteReplay(r, tx, claim, out); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, out)
}

func (s *Server) codeRevisions(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	var project string
	if err = s.Access.Pool.QueryRow(r.Context(), `SELECT project_id::text FROM olp.code_routes WHERE id=$1`, id).Scan(&project); err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&project, access.View); err != nil {
		return access.Reply{}, err
	}
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT jsonb_build_object('id',id,'route',document||jsonb_build_object('adapters',`+codeadapter.SQLAdapters("connections")+`))
		FROM olp.code_route_revisions WHERE route_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3`, id, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}

// codePublication reads the configurations of the providers behind a pool's
// accounts, which a revision freezes as its connections, and the adapters
// they serve in table order. A pool may mix adapters.
func codePublication(ctx context.Context, tx pgx.Tx, poolID string) ([]byte, []codemode.Adapter, error) {
	rows, err := tx.Query(ctx, `SELECT p.id::text,p.configuration FROM olp.providers p
		WHERE EXISTS(SELECT 1 FROM olp.code_accounts a JOIN olp.code_pool_accounts pa ON pa.account_id=a.id WHERE pa.pool_id=$1 AND a.provider_id=p.id)`, poolID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	connections := map[string]json.RawMessage{}
	adapters := []codemode.Adapter{}
	for rows.Next() {
		var id string
		var configuration json.RawMessage
		if err = rows.Scan(&id, &configuration); err != nil {
			return nil, nil, err
		}
		var c struct {
			Kind      string `json:"kind"`
			AuthMode  string `json:"auth_mode"`
			ProfileID string `json:"profile_id"`
		}
		if err = json.Unmarshal(configuration, &c); err != nil {
			return nil, nil, err
		}
		connections[id] = configuration
		if v, ok := codeadapter.ForConnection(c.Kind, c.AuthMode, c.ProfileID); ok && !slices.Contains(adapters, v.Adapter) {
			adapters = append(adapters, v.Adapter)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	slices.SortFunc(adapters, codeadapter.Compare)
	encoded, err := json.Marshal(connections)
	return encoded, adapters, err
}

func codePaidPublication(ctx context.Context, tx pgx.Tx, route codemode.Route) ([]byte, error) {
	if len(route.PaidTargets)+len(route.PaidFallbackTargets) == 0 {
		return json.Marshal(map[string]routing.Provider{})
	}
	ids := slices.Collect(func(yield func(string) bool) {
		for _, target := range route.AllPaidTargets() {
			if !yield(target.ProviderID) {
				return
			}
		}
	})
	rows, err := tx.Query(ctx, `SELECT p.id::text,p.state,v.id::text,v.name,v.configuration,v.models,v.slots,p.project_id::text,`+runtime.PluginColumn+` FROM olp.providers p JOIN olp.provider_revisions v ON v.id=p.active_revision_id WHERE p.id=ANY($1::uuid[]) AND p.project_id=$2 AND p.state<>'disabled'`, ids, route.ProjectID)
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
	for model, target := range route.AllPaidTargets() {
		provider, exists := published[target.ProviderID]
		if !exists {
			return nil, access.Invalid("paid_targets", "Choose a published metered provider in this project.")
		}
		if _, ok := codeadapter.ForConnection(provider.Kind, provider.AuthMode, provider.ProfileID); ok {
			return nil, access.Invalid("paid_targets", "Coding subscriptions belong in the subscription pool.")
		}
		if _, ok := routing.CodePaidProtocol(provider, model); !ok {
			return nil, access.Invalid("paid_targets", "Use a native metered API profile without request defaults, model overrides, plugins or caller credentials.")
		}
		slot := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool {
			return slot.ID == target.SlotID && slot.Enabled && slot.CredentialID != nil && slot.CredentialVersion != nil
		})
		if slot < 0 {
			return nil, access.Invalid("paid_targets", "Choose an enabled credential slot from the selected provider.")
		}
	}
	return json.Marshal(published)
}
