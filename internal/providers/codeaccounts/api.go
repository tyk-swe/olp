package codeaccounts

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode/codeadmin"
	"github.com/tyk-swe/olp/internal/connectors"
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
	a.access.Route(mux, "PUT /api/v1/code/accounts/{id}/drain", a.setDrain)
	a.access.Route(mux, "POST /api/v1/code/accounts/{id}/allowance-reads", a.requestAllowanceRead)
}

func (a API) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return codeadmin.List(a.access, r, p, accountJSON)
}

func (a API) write(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input Input
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	in, err := input.validated()
	if err != nil {
		return httpkit.Reply{}, err
	}
	return codeadmin.Write(a.access, r, codeadmin.Accounts, in.ProjectID, in, func(tx pgx.Tx, p authz.Principal, id, etag string, existing bool) (any, error) {
		return save(r.Context(), tx, p, id, etag, in, existing)
	})
}

func (a API) setDrain(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input struct {
		Draining *bool `json:"draining"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Draining == nil {
		return httpkit.Reply{}, httpkit.Invalid("draining", "Set draining to true or false.")
	}
	ctx := r.Context()
	return access.Transact(a.access, r, func(tx pgx.Tx, p authz.Principal) (string, error) {
		var project, oldETag string
		if err := tx.QueryRow(ctx, `SELECT project_id::text,etag::text FROM olp.code_accounts WHERE id=$1`, id).Scan(&project, &oldETag); err != nil {
			return "", err
		}
		if err := p.Project(&project, authz.Change); err != nil {
			return "", err
		}
		return project, httpkit.Match(r, oldETag)
	}, func(tx pgx.Tx, _ authz.Principal, project string) (httpkit.Reply, access.Event, error) {
		etag := httpkit.NewID()
		if _, err := tx.Exec(ctx, `UPDATE olp.code_accounts SET draining=$2,etag=$3 WHERE id=$1`, id, *input.Draining, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var body json.RawMessage
		if err := tx.QueryRow(ctx, accountJSON+` WHERE x.id=$1`, id).Scan(&body); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(body, etag), access.Event{Action: drainAction(*input.Draining), Resource: "code_account", ID: id, Project: &project}, nil
	})
}

func drainAction(draining bool) string {
	if draining {
		return "code_account.drain"
	}
	return "code_account.resume"
}

type connectionProfile struct {
	kind     connectors.Kind
	authMode connectors.AuthMode
	id       string
}

func (a API) requestAllowanceRead(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.access, r, id, func(tx pgx.Tx, p authz.Principal) (connectionProfile, error) {
		var project string
		var kind, authMode, profileID *string
		if err := tx.QueryRow(r.Context(), `SELECT a.project_id::text,p.configuration->>'kind',p.configuration->>'auth_mode',p.configuration->>'profile_id' FROM olp.code_accounts a JOIN olp.providers p ON p.id=a.provider_id WHERE a.id=$1`, id).Scan(&project, &kind, &authMode, &profileID); err != nil {
			return connectionProfile{}, err
		}
		return connectionProfile{kind: connectors.Kind(providerconfig.Value(kind)), authMode: connectors.AuthMode(providerconfig.Value(authMode)), id: providerconfig.Value(profileID)}, p.Project(&project, authz.Change)
	}, func(tx pgx.Tx, p authz.Principal, profile connectionProfile) (httpkit.Reply, access.Event, error) {
		signal, known := codeadapter.AllowanceSignalForProfile(profile.kind, profile.authMode, profile.id)
		if !known || !signal.ActiveQualified {
			return httpkit.Reply{}, access.Event{}, httpkit.Invalid("adapter", "Only accounts with a proven allowance signal support an active allowance read.")
		}
		if _, err := tx.Exec(r.Context(), `UPDATE olp.code_accounts SET allowance_read_requested_at=now() WHERE id=$1`, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var body json.RawMessage
		if err := tx.QueryRow(r.Context(), accountJSON+` WHERE x.id=$1`, id).Scan(&body); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.OK(body), access.Event{Action: "code_account.allowance_read", Resource: "code_account", ID: id}, nil
	})
}
