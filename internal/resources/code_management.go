package resources

import (
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

func (m *Management) registerCodeMode(mux *http.ServeMux) {
	m.Access.Route(mux, "GET /api/v1/code/bindings", m.codeBindings)
	m.Access.Route(mux, "POST /api/v1/code/bindings/{id}/retire", m.retireCodeBinding)
}

const codeBindingRows = `(SELECT b.id,b.project_id,b.route_id,b.api_key_id,b.conversation,b.parent_id,b.root_id,b.account_id,b.principal,b.created_at,COALESCE(b.retired_at,root.retired_at) retired_at,NULL::uuid paid_provider_id,NULL::uuid paid_slot_id
FROM olp.code_bindings b JOIN olp.code_bindings root ON root.id=b.root_id
UNION ALL
SELECT b.id,b.project_id,b.route_id,b.api_key_id,b.conversation,b.parent_id,b.root_id,NULL::uuid,NULL::text,b.created_at,COALESCE(b.retired_at,root.retired_at),b.provider_id,b.slot_id
FROM olp.code_paid_bindings b JOIN olp.code_paid_bindings root ON root.id=b.root_id)`

func (m *Management) codeBindings(r *http.Request, p access.Principal) (access.Reply, error) {
	return m.Access.CodeList(r, p, `SELECT to_jsonb(x)||jsonb_build_object('pins',`+codeBindingPins+`) FROM `+codeBindingRows+` x`, access.CodeMatch{Field: "account_id", SQL: `(x.account_id=$%[1]d::uuid OR EXISTS(SELECT 1 FROM olp.code_pins p WHERE p.root_id=x.root_id AND p.account_id=$%[1]d::uuid))`})
}

const codePinProjection = `jsonb_build_object('model',p.model,'target_model',coalesce(p.target_model,p.model),'account_id',p.account_id,'principal',p.principal,'created_at',p.created_at)`

const codePinsSelect = `(SELECT coalesce(jsonb_agg(` + codePinProjection + ` ORDER BY p.seq),'[]'::jsonb) FROM olp.code_pins p WHERE p.root_id=`

func codePinsJSON(root string) string { return codePinsSelect + root + `)` }

const codeBindingPins = `CASE WHEN x.paid_provider_id IS NULL THEN ` + codePinsSelect + `x.root_id)` + ` ELSE (SELECT coalesce(jsonb_agg(jsonb_build_object('model',p.model,'target_model',p.selected_model,'account_id',NULL,'principal',NULL,'paid_provider_id',p.provider_id,'paid_slot_id',p.slot_id,'created_at',p.created_at) ORDER BY p.created_at,p.id),'[]'::jsonb) FROM olp.code_paid_bindings p WHERE p.root_id=x.root_id) END`

func (m *Management) retireCodeBinding(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var project, root string
	var provider *string
	err = tx.QueryRow(r.Context(), `SELECT x.project_id,x.root_id,x.paid_provider_id FROM `+codeBindingRows+` x WHERE x.id=$1`, id).Scan(&project, &root, &provider)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&project, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := m.Access.Replay(r, tx, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	table := func() string {
		if provider != nil {
			return "olp.code_paid_bindings"
		}
		return "olp.code_bindings"
	}()
	if _, err = tx.Exec(r.Context(), `UPDATE `+table+` SET retired_at=now() WHERE id=$1 AND retired_at IS NULL`, root); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "code_binding.retire", "code_binding", root, "success"); err != nil {
		return access.Reply{}, err
	}
	var body json.RawMessage
	if err = tx.QueryRow(r.Context(), `SELECT to_jsonb(x)||jsonb_build_object('pins',`+codeBindingPins+`) FROM `+codeBindingRows+` x WHERE x.id=$1`, root).Scan(&body); err != nil {
		return access.Reply{}, err
	}
	out := access.OK(body)
	if err = m.Access.CompleteReplay(r, tx, claim, out); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, out)
}
