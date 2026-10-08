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

func (m *Management) codeBindings(r *http.Request, p access.Principal) (access.Reply, error) {
	return m.Access.CodeList(r, p, `SELECT to_jsonb(x)||jsonb_build_object('retired_at',COALESCE(x.retired_at,root.retired_at),'pins',`+codePinsJSON("x.root_id")+`)
		FROM olp.code_bindings x JOIN olp.code_bindings root ON root.id=x.root_id`)
}

// codePinsJSON is the JSON array of a conversation tree's pins, oldest first.
func codePinsJSON(root string) string {
	return `(SELECT coalesce(jsonb_agg(to_jsonb(p)-'root_id'-'seq' ORDER BY p.seq),'[]'::jsonb) FROM olp.code_pins p WHERE p.root_id=` + root + `)`
}

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
	b, err := ScanCodeBinding(tx.QueryRow(r.Context(), `SELECT `+CodeBindingColumns+` FROM olp.code_bindings b WHERE b.id=$1`, id))
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&b.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := m.Access.Replay(r, tx, p, id)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if _, err = tx.Exec(r.Context(), `UPDATE olp.code_bindings SET retired_at=now() WHERE id=$1 AND retired_at IS NULL`, b.RootID); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "code_binding.retire", "code_binding", b.RootID, "success"); err != nil {
		return access.Reply{}, err
	}
	var body json.RawMessage
	if err = tx.QueryRow(r.Context(), `SELECT to_jsonb(b)||jsonb_build_object('pins',`+codePinsJSON("b.id")+`) FROM olp.code_bindings b WHERE id=$1`, b.RootID).Scan(&body); err != nil {
		return access.Reply{}, err
	}
	out := access.OK(body)
	if err = m.Access.CompleteReplay(r, tx, claim, out); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, out)
}
