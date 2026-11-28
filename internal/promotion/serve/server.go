package serve

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	signin "github.com/tyk-swe/olp/internal/identity"

	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/agentpublication"
	"github.com/tyk-swe/olp/internal/promotion/guardrail"
	"github.com/tyk-swe/olp/internal/promotion/identity"
	"github.com/tyk-swe/olp/internal/promotion/mcpserver"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage/pricebook"
)

const maxDocument = 4 << 20

type Deps struct {
	Access     *access.Server
	SignIn     *signin.Server
	Egress     *egress.Policy
	Unconfined *plugins.Unconfined
	Identity   *http.Client
	Costs      project.Costs
	Normalizer pricebook.Normalizer
	Drafts     provider.Drafts
}

type server struct {
	access       *access.Server
	egress       *egress.Policy
	costs        project.Costs
	normalizer   pricebook.Normalizer
	identity     identity.Deps
	providers    provider.Deps
	sinks        sinks.Credentials
	vault        guardrail.Vault
	servers      mcpserver.Deps
	publications agentpublication.Deps
}

type desired struct {
	sections
	bindings     promotion.Bindings
	planners     []promotion.Planner
	digest       string
	expected     *string
	codeBindings *contract.ConfigurationCodeBindings
}

type notApplicable struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	contract.ConfigurationPlanResponse
}

func (notApplicable) Error() string { return "configuration_not_applicable" }

func Register(mux *http.ServeMux, deps Deps) {
	keys := deps.Access.Keys
	installation := deps.Access.Installation
	s := server{
		access:       deps.Access,
		egress:       deps.Egress,
		costs:        deps.Costs,
		normalizer:   deps.Normalizer,
		identity:     identity.Deps{Access: deps.Access, SignIn: deps.SignIn, Identity: deps.Identity},
		providers:    provider.Deps{Egress: deps.Egress, Unconfined: deps.Unconfined, Keys: keys, Installation: installation, Drafts: deps.Drafts},
		sinks:        sinks.Credentials{Keys: keys, Installation: installation},
		vault:        guardrail.Vault{Keys: keys, Installation: installation},
		servers:      mcpserver.Deps{Keys: mcpserver.Keys{Ring: keys, Installation: installation}, Egress: deps.Egress, Unconfined: deps.Unconfined},
		publications: agentpublication.Deps{Keys: keys, Installation: installation, Egress: deps.Egress, RuntimePublisher: deps.Access.RuntimePublisher},
	}
	deps.Access.Route(mux, "GET /api/v1/configuration/export", s.export)
	deps.Access.Route(mux, "POST /api/v1/configuration/plan", s.plan, access.MaxBody(maxDocument))
	deps.Access.Route(mux, "POST /api/v1/configuration/apply", s.apply, access.MaxBody(maxDocument), access.Deadline(60*time.Second))
}

func (s server) export(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	current, err := read(r.Context(), s.access.Pool)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := identity.Authorize(current.identity, p); err != nil {
		return httpkit.Reply{}, err
	}
	exportedAt := time.Now().UTC().Truncate(time.Second)
	document, err := current.render(&exportedAt)
	if err != nil {
		return httpkit.Reply{}, err
	}
	digest, err := promotion.Digest(document)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(contract.ConfigurationExportResponse{Digest: digest, Document: document}), nil
}

func (s server) plan(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	d, err := s.decode(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := authorize(r.Context(), s.access.Pool, p, d); err != nil {
		return httpkit.Reply{}, err
	}
	result, err := evaluate(r.Context(), s.access.Pool, d)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(result), nil
}

func (s server) apply(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	d, err := s.decode(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	requestPrint, err := d.fingerprint()
	if err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	admitted := func(tx pgx.Tx, p authz.Principal) (authz.Principal, error) { return p, authorize(ctx, tx, p, d) }
	ready, replayed, err := s.ready(r, d, requestPrint, admitted)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, nil
	}
	reply, err := access.Mutate(s.access, r, requestPrint, admitted, func(tx pgx.Tx, p authz.Principal, _ authz.Principal) (httpkit.Reply, access.Event, error) {
		result, err := evaluate(ctx, tx, d)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if len(result.Conflicts) > 0 || len(result.Blockers) > 0 {
			return httpkit.Reply{}, access.Event{}, notApplicable{
				Type:                      "https://openllmproxy.dev/problems/configuration_not_applicable",
				Title:                     http.StatusText(http.StatusConflict),
				Status:                    http.StatusConflict,
				Detail:                    "The artifact cannot be applied while conflicts or blockers remain.",
				ConfigurationPlanResponse: result,
			}
		}
		for _, step := range s.appliers(d, ptr.Value(ready), r) {
			if err := step(ctx, tx, p); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		return httpkit.OK(result), access.Event{Action: "configuration.apply", Resource: "configuration", ID: result.Digest}, nil
	})
	// A refusal changes nothing and is never stored for replay, so the same
	// Idempotency-Key applies the artifact once its conflicts are resolved.
	if refused, ok := errors.AsType[notApplicable](err); ok {
		return httpkit.Reply{Status: http.StatusConflict, Body: refused}, nil
	}
	return reply, err
}

func (s server) ready(r *http.Request, d desired, requestPrint fingerprint, admitted func(pgx.Tx, authz.Principal) (authz.Principal, error)) (*prepared, *httpkit.Reply, error) {
	if !d.needsPrepare() {
		return nil, nil, nil
	}
	p, replayed, err := access.Preflight(s.access, r, requestPrint, admitted)
	if err != nil || replayed != nil {
		return nil, replayed, err
	}
	ready, err := s.prepare(r.Context(), p, d)
	return &ready, nil, err
}

func (s server) decode(r *http.Request) (desired, error) {
	var request contract.ConfigurationPromotionRequest
	if err := httpkit.DecodeUnique(r, &request, maxDocument); err != nil {
		return desired{}, err
	}
	if request.Document.ApiVersion == "" {
		return desired{}, problem.Invalid("document", "Send the configuration artifact.")
	}
	if !request.Document.ApiVersion.Valid() {
		return desired{}, problem.Fail(http.StatusUnprocessableEntity, "unsupported_api_version", "The artifact declares an unsupported api_version.")
	}
	parsed, err := s.parse(request.Document, request.CodeBindings)
	if err != nil {
		return desired{}, err
	}
	bindings, err := promotion.ParseBindings(request)
	if err != nil {
		return desired{}, err
	}
	planners := s.planners(parsed, promotion.NamesOf(request.Document), bindings)
	if err := promotion.Claimed(bindings, claims(planners)); err != nil {
		return desired{}, err
	}
	canonical, err := parsed.snapshot().render(nil)
	if err != nil {
		return desired{}, err
	}
	digest, err := promotion.Digest(canonical)
	if err != nil {
		return desired{}, err
	}
	return desired{
		sections:     parsed,
		bindings:     bindings,
		planners:     planners,
		digest:       digest,
		expected:     expectedDigest(request.ExpectedDigest),
		codeBindings: declaredCode(request.CodeBindings),
	}, nil
}

func claims(planners []promotion.Planner) []promotion.Claim {
	return slices.Concat(promotion.Map(planners, func(planner promotion.Planner) []promotion.Claim { return planner.Claims })...)
}

func expectedDigest(value nullable.Nullable[string]) *string {
	digest, ok := promotion.Present(value)
	if !ok {
		return nil
	}
	return &digest
}

func (s server) prepare(ctx context.Context, p authz.Principal, d desired) (prepared, error) {
	workload, err := identity.Prepare(ctx, d.identity, p, s.identity)
	if err != nil {
		return prepared{}, err
	}
	servers, err := mcpserver.Prepare(ctx, s.access.Pool, p, d.servers, d.bindings, s.servers)
	if err != nil {
		return prepared{}, err
	}
	return prepared{workload: workload, servers: servers}, nil
}

func (d desired) needsPrepare() bool {
	return d.identity.NeedsPrepare() || len(d.servers) > 0
}

func authorize(ctx context.Context, q database.Queryer, p authz.Principal, d desired) error {
	if d.bindings.HasExternal() {
		if err := p.Authorize(authz.Settings); err != nil {
			return err
		}
	}
	for _, planner := range d.planners {
		if planner.Authorize == nil {
			continue
		}
		if err := planner.Authorize(ctx, q, p); err != nil {
			return err
		}
	}
	return nil
}
