package serve

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/agentpublication"
	"github.com/tyk-swe/olp/internal/promotion/guardrail"
	"github.com/tyk-swe/olp/internal/promotion/identity"
	"github.com/tyk-swe/olp/internal/promotion/mcpserver"
	"github.com/tyk-swe/olp/internal/promotion/pricing"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/sinks"
)

const maxDocument = 4 << 20

type Deps struct {
	Access     *access.Server
	Egress     *egress.Policy
	Unconfined *plugins.Unconfined
	Identity   *http.Client
	Costs      project.Costs
	VendorKind pricing.VendorKind
	Catalogue  *providers.Catalogue
}

type server struct {
	access       *access.Server
	egress       *egress.Policy
	costs        project.Costs
	vendorKind   pricing.VendorKind
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

func Register(mux *http.ServeMux, deps Deps) {
	keys := deps.Access.Keys
	installation := deps.Access.Installation
	s := server{
		access:       deps.Access,
		egress:       deps.Egress,
		costs:        deps.Costs,
		vendorKind:   deps.VendorKind,
		identity:     identity.Deps{Access: deps.Access, Identity: deps.Identity},
		providers:    provider.Deps{Egress: deps.Egress, Unconfined: deps.Unconfined, Keys: keys, Installation: installation, Catalogue: deps.Catalogue},
		sinks:        sinks.Credentials{Keys: keys, Installation: installation},
		vault:        guardrail.Vault{Keys: keys, Installation: installation},
		servers:      mcpserver.Deps{Keys: mcpserver.Keys{Ring: keys, Installation: installation}, Egress: deps.Egress, Unconfined: deps.Unconfined},
		publications: agentpublication.Deps{Keys: keys, Installation: installation, Egress: deps.Egress, RuntimePublisher: deps.Access.RuntimePublisher},
	}
	deps.Access.Route(mux, "GET /api/v1/configuration/export", s.export)
	deps.Access.Route(mux, "POST /api/v1/configuration/plan", s.plan, access.MaxBody(maxDocument))
	deps.Access.Route(mux, "POST /api/v1/configuration/apply", s.apply, access.MaxBody(maxDocument), access.Deadline(60*time.Second))
}

func (s server) export(r *http.Request, p access.Principal) (access.Reply, error) {
	current, err := read(r.Context(), s.access.Pool)
	if err != nil {
		return access.Reply{}, err
	}
	if err := identity.Authorize(current.identity, p); err != nil {
		return access.Reply{}, err
	}
	exportedAt := time.Now().UTC().Truncate(time.Second)
	document, err := current.render(&exportedAt)
	if err != nil {
		return access.Reply{}, err
	}
	digest, err := promotion.Digest(document)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(contract.ConfigurationExportResponse{Digest: digest, Document: document}), nil
}

func (s server) plan(r *http.Request, p access.Principal) (access.Reply, error) {
	d, err := s.decode(r)
	if err != nil {
		return access.Reply{}, err
	}
	if err := authorize(r.Context(), s.access.Pool, p, d); err != nil {
		return access.Reply{}, err
	}
	result, err := evaluate(r.Context(), s.access.Pool, d)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(result), nil
}

func (s server) apply(r *http.Request, _ access.Principal) (access.Reply, error) {
	d, err := s.decode(r)
	if err != nil {
		return access.Reply{}, err
	}
	return s.mutate(r, d, nil)
}

func (s server) mutate(r *http.Request, d desired, ready *prepared) (access.Reply, error) {
	ctx := r.Context()
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(ctx)
	if err := authorize(ctx, tx, p, d); err != nil {
		return access.Reply{}, err
	}
	requestPrint, err := d.fingerprint()
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.access.Replay(r, tx, p, requestPrint)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(ctx)
	}
	if ready == nil && d.needsPrepare() {
		if err := tx.Rollback(ctx); err != nil {
			return access.Reply{}, err
		}
		prepared, err := s.prepare(ctx, p, d)
		if err != nil {
			return access.Reply{}, err
		}
		return s.mutate(r, d, &prepared)
	}
	result, err := evaluate(ctx, tx, d)
	if err != nil {
		return access.Reply{}, err
	}
	if len(result.Conflicts) > 0 || len(result.Blockers) > 0 {
		return access.Reply{Status: http.StatusConflict, Body: notApplicable{
			Type:                      "https://openllmproxy.dev/problems/configuration_not_applicable",
			Title:                     http.StatusText(http.StatusConflict),
			Status:                    http.StatusConflict,
			Detail:                    "The artifact cannot be applied while conflicts or blockers remain.",
			ConfigurationPlanResponse: result,
		}}, nil
	}
	for _, step := range s.appliers(d, promotion.ValueOf(ready), r) {
		if err := step(ctx, tx, p); err != nil {
			return access.Reply{}, err
		}
	}
	if err := access.Audit(ctx, tx, r, p.Actor(), "configuration.apply", "configuration", result.Digest, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.OK(result)
	if err := s.access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return reply, tx.Commit(ctx)
}

func (s server) decode(r *http.Request) (desired, error) {
	var request contract.ConfigurationPromotionRequest
	if err := access.DecodeUnique(r, &request, maxDocument); err != nil {
		return desired{}, err
	}
	if request.Document.ApiVersion == "" {
		return desired{}, access.Invalid("document", "Send the configuration artifact.")
	}
	if !request.Document.ApiVersion.Valid() {
		return desired{}, access.Fail(http.StatusUnprocessableEntity, "unsupported_api_version", "The artifact declares an unsupported api_version.")
	}
	document, err := guardrail.Upgrade(request.Document)
	if err != nil {
		return desired{}, err
	}
	parsed, err := s.parse(document, request.CodeBindings)
	if err != nil {
		return desired{}, err
	}
	bindings, err := promotion.ParseBindings(request)
	if err != nil {
		return desired{}, err
	}
	planners := s.planners(parsed, promotion.NamesOf(document), bindings)
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

func (s server) prepare(ctx context.Context, p access.Principal, d desired) (prepared, error) {
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

func authorize(ctx context.Context, q access.Queryer, p access.Principal, d desired) error {
	if d.bindings.HasExternal() {
		if err := p.Authorize(access.Settings); err != nil {
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
