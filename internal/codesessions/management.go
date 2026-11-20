package codesessions

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/codemode/codeadmin"
)

type Management struct{ access *access.Server }

func NewManagement(auth *access.Server) *Management { return &Management{access: auth} }

func (m *Management) Register(mux *http.ServeMux) {
	m.access.Route(mux, "GET /api/v1/code/bindings", m.codeBindings)
	m.access.Route(mux, "POST /api/v1/code/bindings/{id}/retire", m.retireCodeBinding)
}

const codeBindingRows = `(SELECT b.id,b.project_id,b.route_id,b.api_key_id,b.conversation,b.parent_id,b.root_id,b.account_id,b.principal,b.created_at,COALESCE(b.retired_at,root.retired_at) retired_at,NULL::uuid paid_provider_id,NULL::uuid paid_slot_id
FROM olp.code_bindings b JOIN olp.code_bindings root ON root.id=b.root_id
UNION ALL
SELECT b.id,b.project_id,b.route_id,b.api_key_id,b.conversation,b.parent_id,b.root_id,NULL::uuid,NULL::text,b.created_at,COALESCE(b.retired_at,root.retired_at),b.provider_id,b.slot_id
FROM olp.code_paid_bindings b JOIN olp.code_paid_bindings root ON root.id=b.root_id)`

func (m *Management) codeBindings(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return codeadmin.List(m.access, r, p, `SELECT to_jsonb(x)||jsonb_build_object('pins',`+codeBindingPins+`) FROM `+codeBindingRows+` x`, codeadmin.Match{Field: "account_id", SQL: `(x.account_id=$%[1]d::uuid OR EXISTS(SELECT 1 FROM olp.code_pins p WHERE p.root_id=x.root_id AND p.account_id=$%[1]d::uuid))`})
}

const codePinProjection = `jsonb_build_object('model',p.model,'target_model',coalesce(p.target_model,p.model),'account_id',p.account_id,'principal',p.principal,'created_at',p.created_at)`

const codePinsSelect = `(SELECT coalesce(jsonb_agg(` + codePinProjection + ` ORDER BY p.seq),'[]'::jsonb) FROM olp.code_pins p WHERE p.root_id=`

const codeBindingPins = `CASE WHEN x.paid_provider_id IS NULL THEN ` + codePinsSelect + `x.root_id)` + ` ELSE (SELECT coalesce(jsonb_agg(jsonb_build_object('model',p.model,'target_model',p.selected_model,'account_id',NULL,'principal',NULL,'paid_provider_id',p.provider_id,'paid_slot_id',p.slot_id,'created_at',p.created_at) ORDER BY p.created_at,p.id),'[]'::jsonb) FROM olp.code_paid_bindings p WHERE p.root_id=x.root_id) END`

func (m *Management) retireCodeBinding(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(m.access, r, id, func(tx pgx.Tx, p authz.Principal) (codeBinding, error) {
		var project, root string
		var provider *string
		err = tx.QueryRow(r.Context(), `SELECT x.project_id,x.root_id,x.paid_provider_id FROM `+codeBindingRows+` x WHERE x.id=$1`, id).Scan(&project, &root, &provider)
		if err != nil {
			return codeBinding{}, err
		}
		if err = p.Project(&project, authz.Change); err != nil {
			return codeBinding{}, err
		}
		return codeBinding{root: root, paid: provider != nil}, nil
	}, func(tx pgx.Tx, p authz.Principal, binding codeBinding) (httpkit.Reply, access.Event, error) {
		root := binding.root
		if _, err = tx.Exec(r.Context(), `UPDATE `+binding.table()+` SET retired_at=now() WHERE id=$1 AND retired_at IS NULL`, root); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var body json.RawMessage
		if err = tx.QueryRow(r.Context(), `SELECT to_jsonb(x)||jsonb_build_object('pins',`+codeBindingPins+`) FROM `+codeBindingRows+` x WHERE x.id=$1`, root).Scan(&body); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		out := httpkit.OK(body)
		return out, access.Event{Action: "code_binding.retire", Resource: "code_binding", ID: root}, nil
	})
}

type codeBinding struct {
	root string
	paid bool
}

func (b codeBinding) table() string {
	if b.paid {
		return "olp.code_paid_bindings"
	}
	return "olp.code_bindings"
}
