package codeaccounts

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/providerconfig"
)

type API struct {
	access *access.Server
}

func NewAPI(a *access.Server) API { return API{access: a} }

func (a API) Register(mux *http.ServeMux) {
	a.access.Route(mux, "GET /api/v1/code/accounts", a.list)
	a.access.Route(mux, "POST /api/v1/code/accounts", a.write)
	a.access.Route(mux, "PUT /api/v1/code/accounts/{id}", a.write)
	a.access.Route(mux, "POST /api/v1/code/accounts/{id}/allowance-reads", a.requestAllowanceRead)
}

func (a API) list(r *http.Request, p access.Principal) (access.Reply, error) {
	return a.access.CodeList(r, p, accountJSON)
}

func (a API) write(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input Input
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	in, err := input.validated()
	if err != nil {
		return access.Reply{}, err
	}
	return a.access.CodeWrite(r, access.CodeAccounts, in.ProjectID, in, func(tx pgx.Tx, p access.Principal, id, etag string, existing bool) (any, error) {
		return save(r.Context(), tx, p, id, etag, in, existing)
	})
}

type connectionProfile struct {
	kind, authMode, id string
}

func (a API) requestAllowanceRead(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	return access.Mutate(a.access, r, id, func(tx pgx.Tx, p access.Principal) (connectionProfile, error) {
		var project string
		var kind, authMode, profileID *string
		if err := tx.QueryRow(r.Context(), `SELECT a.project_id::text,p.configuration->>'kind',p.configuration->>'auth_mode',p.configuration->>'profile_id' FROM olp.code_accounts a JOIN olp.providers p ON p.id=a.provider_id WHERE a.id=$1`, id).Scan(&project, &kind, &authMode, &profileID); err != nil {
			return connectionProfile{}, err
		}
		return connectionProfile{kind: providerconfig.Value(kind), authMode: providerconfig.Value(authMode), id: providerconfig.Value(profileID)}, p.Project(&project, access.Change)
	}, func(tx pgx.Tx, p access.Principal, profile connectionProfile) (access.Reply, error) {
		signal, known := codeadapter.AllowanceSignalForProfile(profile.kind, profile.authMode, profile.id)
		if !known || !signal.ActiveQualified {
			return access.Reply{}, access.Invalid("adapter", "Only accounts with a proven allowance signal support an active allowance read.")
		}
		if _, err := tx.Exec(r.Context(), `UPDATE olp.code_accounts SET allowance_read_requested_at=now() WHERE id=$1`, id); err != nil {
			return access.Reply{}, err
		}
		if err := access.Audit(r.Context(), tx, r, p.Actor(), "code_account.allowance_read", "code_account", id, "success"); err != nil {
			return access.Reply{}, err
		}
		var body json.RawMessage
		if err := tx.QueryRow(r.Context(), accountJSON+` WHERE x.id=$1`, id).Scan(&body); err != nil {
			return access.Reply{}, err
		}
		return access.OK(body), nil
	})
}
