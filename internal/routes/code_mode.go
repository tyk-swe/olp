package routes

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/codemode/codeadmin"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/providers/connection"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/release"

	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/routing"
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

func (s *Server) codeRoutes(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return codeadmin.List(s.Access, r, p, `SELECT x.draft||jsonb_build_object('etag',x.etag,
		'revision_id',coalesce(v.id::text,''),'revision',coalesce(v.revision,0),'published_at',v.published_at)
		||jsonb_build_object('adapters',`+codeadapter.SQLAdapters("v.connections")+`)
		FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id`)
}

type CodeRouteInput struct {
	CodexCLIIdentity        bool                           `json:"codex_cli_identity"`
	PaidFallbackTargets     map[string]codemode.PaidTarget `json:"paid_fallback_targets"`
	ChatToAntigravity       bool                           `json:"chat_to_antigravity"`
	PaidParticipation       bool                           `json:"paid_participation"`
	PaidTargets             map[string]codemode.PaidTarget `json:"paid_targets"`
	ModelPools              map[string][]string            `json:"model_pools"`
	InitialRecoveryAttempts int                            `json:"initial_recovery_attempts"`
	ModelRewriting          bool                           `json:"model_rewriting"`
	Aliases                 map[string]string              `json:"aliases"`
	ChatToCodex             bool                           `json:"chat_to_codex,omitempty"`
	MessagesToCodex         bool                           `json:"messages_to_codex"`
	Tools                   *codemode.ResearchTools        `json:"tools"`
	MaxBodyBytes            *int64                         `json:"max_body_bytes,omitempty"`
	ProjectID               string                         `json:"project_id"`
	Slug                    string                         `json:"slug"`
	PoolID                  string                         `json:"pool_id"`
	Models                  []string                       `json:"models"`
	Enabled                 bool                           `json:"enabled"`
}

func (s *Server) writeCodeRoute(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var in CodeRouteInput
	if err := httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	input, validationError := in.Validated()
	if validationError != nil {
		return httpkit.Reply{}, validationError
	}
	return codeadmin.Write(s.Access, r, codeadmin.Routes, input.ProjectID, input, func(tx pgx.Tx, p authz.Principal, id, etag string, existing bool) (any, error) {
		return SaveCodeRoute(r.Context(), tx, p, id, etag, input, existing)
	})
}

func (s *Server) publishCodeRoute(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.Access, r, id, func(tx pgx.Tx, p authz.Principal) (codeDraft, error) {
		var document []byte
		var project, etag string
		if err = tx.QueryRow(r.Context(), `SELECT draft,project_id::text,etag::text FROM olp.code_routes WHERE id=$1`, id).Scan(&document, &project, &etag); err != nil {
			return codeDraft{}, err
		}
		if err = p.Project(&project, authz.Change); err != nil {
			return codeDraft{}, err
		}
		return codeDraft{document: document, etag: etag}, nil
	}, func(tx pgx.Tx, p authz.Principal, draft codeDraft) (httpkit.Reply, access.Event, error) {
		document, etag := draft.document, draft.etag
		if err = httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var route codemode.Route
		if err = json.Unmarshal(document, &route); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		published, err := SaveCodePublication(r.Context(), tx, p, id, route, nil, nil)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		route = published.Route
		adapters := published.Adapters
		if _, err := release.Publish(r.Context(), tx, p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}

		out := httpkit.Detail(codeRoute{Route: route, Adapters: adapters}, route.ETag)
		return out, access.Event{Action: "code_route.publish", Resource: "code_route", ID: id}, nil
	})
}

func (s *Server) codeRevisions(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var project string
	if err = s.Access.Pool.QueryRow(r.Context(), `SELECT project_id::text FROM olp.code_routes WHERE id=$1`, id).Scan(&project); err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&project, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := database.QueryJSON(r.Context(), s.Access.Pool, `SELECT jsonb_build_object('id',id,'route',document||jsonb_build_object('adapters',`+codeadapter.SQLAdapters("connections")+`))
		FROM olp.code_route_revisions WHERE route_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3`, id, page.Before, page.Limit+1)
	return httpkit.ListReply(items, page), err
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
			Kind      connectors.Kind     `json:"kind"`
			AuthMode  connectors.AuthMode `json:"auth_mode"`
			ProfileID string              `json:"profile_id"`
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

func UnpublishCodeRoute(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE olp.code_routes SET latest_revision_id=NULL WHERE id=$1`, id)
	return err
}

func SaveCodeRoute(ctx context.Context, tx pgx.Tx, p authz.Principal, id, etag string, raw CodeRouteInput, existing bool) (*codeRoute, error) {
	in, validationError := raw.Validated()
	if validationError != nil {
		return nil, validationError
	}
	if err := p.Project(&in.ProjectID, authz.Change); err != nil {
		return nil, err
	}
	out := codemode.Route{ChatToAntigravity: in.ChatToAntigravity, PaidParticipation: in.PaidParticipation, PaidTargets: in.PaidTargets, PaidFallbackTargets: in.PaidFallbackTargets, ModelPools: in.ModelPools, InitialRecoveryAttempts: in.InitialRecoveryAttempts, Aliases: in.Aliases, ModelRewriting: in.ModelRewriting, CodexCLIIdentity: in.CodexCLIIdentity, ChatToCodex: in.ChatToCodex, MessagesToCodex: in.MessagesToCodex, Tools: in.Tools, MaxBodyBytes: in.MaxBodyBytes, ID: id, ProjectID: in.ProjectID, Slug: in.Slug, PoolID: in.PoolID, Models: in.Models, Enabled: in.Enabled, ETag: etag}
	adapters := []codemode.Adapter{}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_pools WHERE id=$1 AND project_id=$2)`, in.PoolID, in.ProjectID).Scan(&matches); err != nil {
		return nil, err
	}
	if !matches {
		return nil, problem.Fail(404, "not_found", "The pool was not found.")
	}
	if out.Tools != nil {
		if _, err := codesessions.ResearchAccount(ctx, tx, out); err != nil {
			return nil, err
		}
	}
	if existing {
		var slug string
		if err := tx.QueryRow(ctx, `SELECT x.slug,coalesce(v.id::text,''),coalesce(v.revision,0),v.published_at,`+codeadapter.SQLAdapters("v.connections")+`
				FROM olp.code_routes x LEFT JOIN olp.code_route_revisions v ON v.id=x.latest_revision_id WHERE x.id=$1`, id).Scan(&slug, &out.RevisionID, &out.Revision, &out.PublishedAt, &adapters); err != nil {
			return nil, err
		}
		if slug != in.Slug {
			return nil, problem.Invalid("slug", "The route slug is immutable.")
		}
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE slug=$1)`, in.Slug).Scan(&matches); err != nil {
		return nil, err
	}
	if matches {
		return nil, problem.Invalid("slug", "This slug belongs to an ordinary route.")
	}
	document, _ := json.Marshal(out)
	_, err := tx.Exec(ctx, `INSERT INTO olp.code_routes(id,project_id,slug,draft,etag,created_by) VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT(id) DO UPDATE SET draft=excluded.draft,etag=excluded.etag`, id, in.ProjectID, in.Slug, document, etag, p.UserID())
	return &codeRoute{Route: out, Adapters: adapters}, err
}

func (in CodeRouteInput) validate() error {
	if err := (codemode.Route{Models: in.Models, PaidParticipation: in.PaidParticipation, PaidTargets: in.PaidTargets, PaidFallbackTargets: in.PaidFallbackTargets, InitialRecoveryAttempts: in.InitialRecoveryAttempts}).ValidatePaidTargets(); err != nil {
		return problem.Invalid("paid_targets", err.Error())
	}
	if in.InitialRecoveryAttempts < 0 || in.InitialRecoveryAttempts > 2 {
		return problem.Invalid("initial_recovery_attempts", "Use zero to disable recovery or allow at most two additional attempts.")
	}
	if !bodylimit.Valid(in.MaxBodyBytes) {
		return problem.Invalid("max_body_bytes", "Use 1 to 1073741824 bytes, or null to inherit the installation limit.")
	}
	if !routeidentity.ValidSlug(in.Slug) {
		return problem.Invalid("slug", "Use a route slug.")
	}
	if err := codemode.ValidateModels(in.Models); err != nil {
		return problem.Invalid("models", err.Error())
	}
	if err := (codemode.Route{Models: in.Models, Aliases: in.Aliases, ModelRewriting: in.ModelRewriting}).ValidateAliases(); err != nil {
		return problem.Invalid("aliases", err.Error())
	}
	if err := (codemode.Route{Models: in.Models, Aliases: in.Aliases, ModelRewriting: in.ModelRewriting, ModelPools: in.ModelPools}).ValidateModelPools(); err != nil {
		return problem.Invalid("model_pools", err.Error())
	}
	if err := in.Tools.Validate(); err != nil {
		return problem.Invalid("tools", err.Error())
	}
	return nil
}

func SaveCodePublication(ctx context.Context, tx pgx.Tx, p authz.Principal, id string, in codemode.Route, frozen map[string]connection.Configuration, frozenPaid map[string]routing.Provider) (*codeRoute, error) {
	project, projectError := ids.Parse(in.ProjectID)
	if projectError != nil {
		return nil, projectError
	}
	if err := p.Project(&project, authz.Change); err != nil {
		return nil, err
	}
	route := in
	if err := validateCodePublicationRoute(ctx, tx, route); err != nil {
		return nil, err
	}
	paidConnections, err := resolvePublishedPaidConnections(ctx, tx, route, frozenPaid)
	if err != nil {
		return nil, err
	}
	connections, adapters, err := resolvePublishedPoolConnections(ctx, tx, route.PoolID, frozen)
	if err != nil {
		return nil, err
	}
	route.RevisionID = ids.New()
	route.ETag = ids.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	route.PublishedAt = &now
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0)+1 FROM olp.code_route_revisions WHERE route_id=$1`, id).Scan(&route.Revision); err != nil {
		return nil, err
	}
	document, _ := json.Marshal(route)
	if _, err = tx.Exec(ctx, `INSERT INTO olp.code_route_revisions(id,route_id,revision,document,created_by,published_at,connections,paid_connections)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, route.RevisionID, id, route.Revision, document, p.UserID(), now, connections, paidConnections); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE olp.code_routes SET latest_revision_id=$2,etag=$3,draft=$4 WHERE id=$1`, id, route.RevisionID, route.ETag, document); err != nil {
		return nil, err
	}
	return &codeRoute{Route: route, Adapters: adapters}, nil
}

func CodePaidProviders(ctx context.Context, tx database.Queryer, route codemode.Route) (map[string]routing.Provider, error) {
	if len(route.PaidTargets)+len(route.PaidFallbackTargets) == 0 {
		return map[string]routing.Provider{}, nil
	}
	providerIDs := slices.Collect(func(yield func(string) bool) {
		for _, target := range route.AllPaidTargets() {
			if !yield(target.ProviderID) {
				return
			}
		}
	})
	rows, err := tx.Query(ctx, `SELECT p.id::text,p.state,r.id::text,r.name,r.configuration,r.models,r.slots,p.project_id::text,`+release.PluginColumn("r")+` FROM olp.providers p JOIN olp.provider_revisions r ON r.id=p.active_revision_id WHERE p.id=ANY($1::uuid[]) AND p.project_id=$2 AND p.state<>'disabled'`, providerIDs, route.ProjectID)
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
	if err := ValidateCodePaidProviders(route, published); err != nil {
		return nil, err
	}
	return published, nil
}
func ValidateCodePaidProviders(route codemode.Route, published map[string]routing.Provider) error {
	for model, target := range route.AllPaidTargets() {
		provider, exists := published[target.ProviderID]
		if !exists {
			return problem.Invalid("paid_targets", "Choose a published metered provider in this project.")
		}
		if _, ok := codeadapter.ForConnection(provider.Kind, provider.AuthMode, provider.ProfileID); ok {
			return problem.Invalid("paid_targets", "Coding subscriptions belong in the subscription pool.")
		}
		if _, ok := routing.CodePaidProtocol(provider, model); !ok {
			return problem.Invalid("paid_targets", "Use a native metered API profile without request defaults, model overrides, plugins or caller credentials.")
		}
		slot := slices.IndexFunc(provider.Slots, func(slot routing.Slot) bool {
			return slot.ID == target.SlotID && slot.Enabled && slot.CredentialID != nil && slot.CredentialVersion != nil
		})
		if slot < 0 {
			return problem.Invalid("paid_targets", "Choose an enabled credential slot from the selected provider.")
		}
	}
	return nil
}

func (in CodeRouteInput) Validated() (CodeRouteInput, error) {
	parsed, err := ids.ParseAll([]string{in.ProjectID, in.PoolID})
	if err != nil {
		return CodeRouteInput{}, err
	}
	tools, toolsError := func() (*codemode.ResearchTools, error) {
		if in.Tools == nil {
			return nil, nil
		}
		account, accountError := ids.Parse(in.Tools.AccountID)
		if accountError != nil {
			return nil, accountError
		}
		return &codemode.ResearchTools{AccountID: account, Services: in.Tools.Services}, nil
	}()
	if toolsError != nil {
		return CodeRouteInput{}, toolsError
	}
	paidTargets, targetError := validatedCodeRouteTargets(in.PaidTargets)
	if targetError != nil {
		return CodeRouteInput{}, targetError
	}
	fallbackTargets, fallbackError := validatedCodeRouteTargets(in.PaidFallbackTargets)
	if fallbackError != nil {
		return CodeRouteInput{}, fallbackError
	}
	canonical := CodeRouteInput{ProjectID: parsed[0], Slug: in.Slug, PoolID: parsed[1], Models: in.Models, Enabled: in.Enabled, ChatToAntigravity: in.ChatToAntigravity, PaidParticipation: in.PaidParticipation, PaidTargets: paidTargets, PaidFallbackTargets: fallbackTargets, ModelPools: in.ModelPools, InitialRecoveryAttempts: in.InitialRecoveryAttempts, ModelRewriting: in.ModelRewriting, Aliases: in.Aliases, CodexCLIIdentity: in.CodexCLIIdentity, ChatToCodex: in.ChatToCodex, MessagesToCodex: in.MessagesToCodex, Tools: tools, MaxBodyBytes: in.MaxBodyBytes}
	return canonical, canonical.validate()
}

func (in CodeRouteInput) Validate() error { _, err := in.Validated(); return err }

func validatedCodeRouteTargets(input map[string]codemode.PaidTarget) (map[string]codemode.PaidTarget, error) {
	models := slices.Sorted(maps.Keys(input))
	rawTargets := slices.Collect(func(yield func(string) bool) {
		for _, model := range models {
			target := input[model]
			if !yield(target.ProviderID) || !yield(target.SlotID) {
				return
			}
		}
	})
	targets, targetError := ids.ParseAll(rawTargets)
	if targetError != nil {
		return nil, targetError
	}
	paidTargets := func() map[string]codemode.PaidTarget {
		if input == nil {
			return nil
		}
		return maps.Collect(func(yield func(string, codemode.PaidTarget) bool) {
			for index, model := range models {
				if !yield(model, codemode.PaidTarget{ProviderID: targets[2*index], SlotID: targets[2*index+1]}) {
					return
				}
			}
		})
	}()

	return paidTargets, nil
}

type codeDraft struct {
	document []byte
	etag     string
}
