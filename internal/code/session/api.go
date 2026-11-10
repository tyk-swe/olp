package session

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
)

type API struct {
	server *access.Server
}

func NewAPI(server *access.Server) *API { return &API{server: server} }

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/code/bindings", a.listBindings)
	a.server.Route(mux, "POST /api/v1/code/bindings/{id}/retire", a.retireBinding)
	a.server.Route(mux, "GET /api/v1/code/attempts", a.listAttempts)
	a.server.Route(mux, "GET /api/v1/code/decisions", a.listDecisions)
	a.server.Route(mux, "GET /api/v1/code/refusals", a.listRefusals)
}

type diagnosticFilters struct {
	project       string
	routeID       string
	apiKeyID      string
	accountID     string
	bindingID     string
	attemptID     string
	endUserFilter bool
	endUserDigest string
}

func parseDiagnosticFilters(r *http.Request, p access.Principal, allowEndUser bool) (diagnosticFilters, error) {
	var f diagnosticFilters
	var err error
	query := r.URL.Query()
	if f.project = query.Get("project_id"); f.project != "" {
		if f.project, err = access.ParseUUID(f.project); err != nil {
			return f, err
		}
		if err = p.Project(&f.project, access.View); err != nil {
			return f, err
		}
	}
	for _, entry := range []struct {
		name   string
		target *string
	}{
		{"route_id", &f.routeID},
		{"api_key_id", &f.apiKeyID},
		{"account_id", &f.accountID},
		{"binding_id", &f.bindingID},
		{"attempt_id", &f.attemptID},
	} {
		if raw := query.Get(entry.name); raw != "" {
			if *entry.target, err = access.ParseUUID(raw); err != nil {
				return f, err
			}
		}
	}
	if digest := query.Get("end_user_digest"); allowEndUser && digest != "" {
		f.endUserFilter = true
		if digest == "unidentified" {
			f.endUserDigest = ""
		} else if !keyauthority.ValidEndUserDigest(digest) {
			return f, access.Invalid("end_user_digest", "Use an end-user digest or unidentified.")
		} else {
			f.endUserDigest = digest
		}
	}
	return f, nil
}

const bindingRows = `(SELECT b.id,b.project_id,b.route_id,b.api_key_id,b.conversation,b.parent_id,b.root_id,b.account_id,b.principal,b.created_at,COALESCE(b.retired_at,root.retired_at) retired_at,NULL::uuid paid_provider_id,NULL::uuid paid_slot_id
FROM olp.code_bindings b JOIN olp.code_bindings root ON root.id=b.root_id
UNION ALL
SELECT b.id,b.project_id,b.route_id,b.api_key_id,b.conversation,b.parent_id,b.root_id,NULL::uuid,NULL::text,b.created_at,COALESCE(b.retired_at,root.retired_at),b.provider_id,b.slot_id
FROM olp.code_paid_bindings b JOIN olp.code_paid_bindings root ON root.id=b.root_id)`

const pinProjection = `jsonb_build_object('model',p.model,'target_model',coalesce(p.target_model,p.model),'account_id',p.account_id,'principal',p.principal,'created_at',p.created_at)`

const bindingPins = `CASE WHEN x.paid_provider_id IS NULL THEN (SELECT coalesce(jsonb_agg(` + pinProjection + ` ORDER BY p.seq),'[]'::jsonb) FROM olp.code_pins p WHERE p.root_id=x.root_id) ELSE (SELECT coalesce(jsonb_agg(jsonb_build_object('model',p.model,'target_model',p.selected_model,'account_id',NULL,'principal',NULL,'paid_provider_id',p.provider_id,'paid_slot_id',p.slot_id,'created_at',p.created_at) ORDER BY p.created_at,p.id),'[]'::jsonb) FROM olp.code_paid_bindings p WHERE p.root_id=x.root_id) END`

func (a *API) listBindings(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	f, err := parseDiagnosticFilters(r, p, false)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), `SELECT to_jsonb(x)||jsonb_build_object('pins',`+bindingPins+`) FROM `+bindingRows+` x
		WHERE x.id<$1 AND ($2 OR x.project_id=ANY($3::uuid[])) AND ($4='' OR x.project_id::text=$4)
		AND ($6='' OR x.route_id::text=$6) AND ($7='' OR x.api_key_id::text=$7)
		AND ($8='' OR x.account_id=$8::uuid OR EXISTS(SELECT 1 FROM olp.code_pins p WHERE p.root_id=x.root_id AND p.account_id=$8::uuid))
		AND ($9='' OR x.id::text=$9)
		ORDER BY x.id DESC LIMIT $5`,
		page.Before, p.AllProjects, p.ProjectIDs(), f.project, page.Limit+1, f.routeID, f.apiKeyID, f.accountID, f.bindingID)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}

func (a *API) retireBinding(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	ctx := r.Context()
	type lockedBinding struct {
		root string
		paid bool
	}
	return access.Mutate(a.server, r, id, func(tx pgx.Tx, p access.Principal) (lockedBinding, error) {
		var project, root string
		var provider *string
		if err := tx.QueryRow(ctx, `SELECT x.project_id::text,x.root_id::text,x.paid_provider_id::text FROM `+bindingRows+` x WHERE x.id=$1`, id).Scan(&project, &root, &provider); err != nil {
			return lockedBinding{}, err
		}
		return lockedBinding{root: root, paid: provider != nil}, p.Project(&project, access.Change)
	}, func(tx pgx.Tx, p access.Principal, locked lockedBinding) (access.Reply, error) {
		var err error
		if locked.paid {
			_, err = tx.Exec(ctx, `UPDATE olp.code_paid_bindings SET retired_at=now() WHERE id=$1 AND retired_at IS NULL`, locked.root)
		} else {
			_, err = tx.Exec(ctx, `UPDATE olp.code_bindings SET retired_at=now() WHERE id=$1 AND retired_at IS NULL`, locked.root)
		}
		if err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(ctx, tx, r, p.Actor(), "code_binding.retire", "code_binding", locked.root, "success"); err != nil {
			return access.Reply{}, err
		}
		var body json.RawMessage
		if err := tx.QueryRow(ctx, `SELECT to_jsonb(x)||jsonb_build_object('pins',`+bindingPins+`) FROM `+bindingRows+` x WHERE x.id=$1`, locked.root).Scan(&body); err != nil {
			return access.Reply{}, err
		}
		return access.OK(body), nil
	})
}

func (a *API) listAttempts(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	f, err := parseDiagnosticFilters(r, p, true)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), `SELECT jsonb_build_object(
		'upstream_status',x.upstream_status,'outcome_origin',x.outcome_origin,'outcome',x.outcome,'outcome_observed_at',x.outcome_observed_at,
		'account_id',x.account_id,'api_key_id',x.api_key_id,'binding_id',x.binding_id,'bound_evidence',x.bound_evidence,
		'cached_tokens',x.cached_tokens,'created_at',x.created_at,'finished_at',x.finished_at,'id',x.id,
		'input_tokens',x.input_tokens,'model',x.model,'operation',x.operation,'output_tokens',x.output_tokens,
		'project_id',x.project_id,'reasoning_tokens',x.reasoning_tokens,'refusal',x.refusal,'allowance_charge',x.allowance_charge,'reported_tokens',x.reported_tokens,
		'reserved_tokens',x.reserved_tokens,'route_id',x.route_id,'state',x.state,'route_revision_id',x.route_revision_id,
		'end_user_digest',x.end_user_digest,'attribution',x.attribution,'paid_binding_id',x.paid_binding_id,'paid_provider_id',x.paid_provider_id,
		'paid_slot_id',x.paid_slot_id,'requested_model',x.requested_model,
		'initial_recovery',x.initial_recovery,'nonexecution',x.nonexecution,'recovery_from',prior.id,
		'recovery_reason',CASE WHEN prior.nonexecution AND prior.outcome_origin='upstream' AND prior.outcome='rejected'
			THEN CASE prior.upstream_status WHEN 401 THEN 'credential_rejected' WHEN 429 THEN 'quota_rejected' END END,
		'codex_cli_identity_enabled',COALESCE((SELECT v.document->>'codex_cli_identity'='true' FROM olp.code_route_revisions v WHERE v.id=x.route_revision_id),false),
		'paid_monetary_state',CASE WHEN x.paid_binding_id IS NULL THEN NULL WHEN x.state='prepared' THEN 'pending' WHEN x.state='aborted' THEN 'released'
			WHEN EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE f.request_id=x.id AND f.usage_complete AND f.charge_status<>'billing_uncertain' AND f.estimated_cost IS NOT NULL AND f.charge IS NOT NULL) THEN 'settled' ELSE 'unknown' END,
		'decision',to_jsonb(d)
	) FROM olp.code_attempts x
	LEFT JOIN olp.code_attempts prior ON prior.id=x.recovery_from AND prior.project_id=x.project_id AND prior.route_id=x.route_id AND prior.api_key_id=x.api_key_id
	LEFT JOIN olp.code_decisions d ON d.attempt_id=x.id AND d.project_id=x.project_id
	WHERE x.id<$1 AND ($2 OR x.project_id=ANY($3::uuid[])) AND ($4='' OR x.project_id::text=$4)
	AND ($6='' OR x.route_id::text=$6) AND ($7='' OR x.api_key_id::text=$7) AND ($8='' OR x.account_id::text=$8)
	AND ($9='' OR x.binding_id::text=$9 OR x.paid_binding_id::text=$9) AND ($10='' OR x.id::text=$10)
	AND (NOT $11 OR x.end_user_digest=$12)
	ORDER BY x.id DESC LIMIT $5`,
		page.Before, p.AllProjects, p.ProjectIDs(), f.project, page.Limit+1,
		f.routeID, f.apiKeyID, f.accountID, f.bindingID, f.attemptID, f.endUserFilter, f.endUserDigest)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}

func (a *API) listDecisions(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	f, err := parseDiagnosticFilters(r, p, false)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), `SELECT to_jsonb(x) FROM olp.code_decisions x
		WHERE x.id<$1 AND ($2 OR x.project_id=ANY($3::uuid[])) AND ($4='' OR x.project_id::text=$4)
		AND ($6='' OR x.route_id::text=$6) AND ($7='' OR x.api_key_id::text=$7)
		AND ($8='' OR x.chosen_account_id::text=$8) AND ($9='' OR x.binding_id::text=$9)
		AND ($10='' OR x.attempt_id::text=$10)
		ORDER BY x.id DESC LIMIT $5`,
		page.Before, p.AllProjects, p.ProjectIDs(), f.project, page.Limit+1,
		f.routeID, f.apiKeyID, f.accountID, f.bindingID, f.attemptID)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}

func (a *API) listRefusals(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	f, err := parseDiagnosticFilters(r, p, true)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), `SELECT to_jsonb(x) FROM olp.code_refusals x
		WHERE x.id<$1 AND ($2 OR x.project_id=ANY($3::uuid[])) AND ($4='' OR x.project_id::text=$4)
		AND ($6='' OR x.route_id::text=$6) AND ($7='' OR x.api_key_id::text=$7)
		AND (NOT $8 OR x.end_user_digest=$9)
		ORDER BY x.id DESC LIMIT $5`,
		page.Before, p.AllProjects, p.ProjectIDs(), f.project, page.Limit+1,
		f.routeID, f.apiKeyID, f.endUserFilter, f.endUserDigest)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := access.JSONRows(rows)
	return access.ListReply(items, page), err
}
