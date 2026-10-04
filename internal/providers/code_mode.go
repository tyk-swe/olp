package providers

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func (s *Server) registerCodeMode(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/code/accounts", s.codeAccounts)
	s.Access.Route(mux, "POST /api/v1/code/accounts", s.writeCodeAccount)
	s.Access.Route(mux, "PUT /api/v1/code/accounts/{id}", s.writeCodeAccount)
}

var codeAccountJSON = `SELECT jsonb_build_object('id',x.id,'project_id',x.project_id,'provider_id',x.provider_id,'credential_id',x.credential_id,'principal',x.principal,'name',x.name,'enabled',x.enabled,'models',x.models,'etag',x.etag,'health',x.health,
	'adapter',` + codeadapter.SQL("p.configuration") + `,
	'allowance',x.allowance,'eligible',coalesce(x.enabled AND p.state<>'disabled' AND olp.code_account_available(x) AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now()),false),
	'grant_state',CASE WHEN c.revoked_at IS NOT NULL THEN 'revoked' WHEN g.lapsed_at IS NOT NULL THEN 'lapsed' WHEN g.expires_at<=now() THEN 'expired' ELSE 'current' END)
	FROM olp.code_accounts x JOIN olp.providers p ON p.id=x.provider_id JOIN olp.provider_credentials c ON c.id=x.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id`

func (s *Server) codeAccounts(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.Access.CodeList(r, p, codeAccountJSON)
}

type codeAccountInput struct {
	ProjectID    string   `json:"project_id"`
	ProviderID   string   `json:"provider_id"`
	CredentialID string   `json:"credential_id"`
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	Models       []string `json:"models"`
}

func (s *Server) writeCodeAccount(r *http.Request, _ access.Principal) (access.Reply, error) {
	var in codeAccountInput
	if err := access.Decode(r, &in); err != nil {
		return access.Reply{}, err
	}
	if err := access.ValidText("name", in.Name, 100); err != nil {
		return access.Reply{}, err
	}
	for _, id := range []*string{&in.ProviderID, &in.CredentialID} {
		var err error
		if *id, err = access.ParseUUID(*id); err != nil {
			return access.Reply{}, err
		}
	}
	if err := codemode.ValidateModels(in.Models); err != nil {
		return access.Reply{}, access.Invalid("models", err.Error())
	}
	return s.Access.CodeWrite(r, "code_accounts", "code_account", in.ProjectID, in, func(tx pgx.Tx, p access.Principal, id, etag string) (any, error) {
		var principal string
		err := tx.QueryRow(r.Context(), `SELECT c.principal FROM olp.provider_credentials c JOIN olp.providers p ON p.id=c.provider_id JOIN olp.provider_grants g ON g.credential_id=c.id
			WHERE p.id=$1 AND p.project_id=$2 AND c.id=$3 AND (NOT $4 OR (c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())))`, in.ProviderID, in.ProjectID, in.CredentialID, in.Enabled).Scan(&principal)
		if err != nil {
			return nil, err
		}
		if principal == "" {
			return nil, access.Invalid("credential_id", "Enrollment must establish an upstream principal.")
		}
		if r.PathValue("id") != "" {
			var oldPrincipal, oldProvider string
			if err = tx.QueryRow(r.Context(), `SELECT principal,provider_id::text FROM olp.code_accounts WHERE id=$1`, id).Scan(&oldPrincipal, &oldProvider); err != nil {
				return nil, err
			}
			if oldPrincipal != principal || oldProvider != in.ProviderID {
				return nil, access.Invalid("credential_id", "Account identity cannot change during credential rotation.")
			}
		}
		models, _ := json.Marshal(in.Models)
		_, err = tx.Exec(r.Context(), `INSERT INTO olp.code_accounts(id,project_id,provider_id,credential_id,principal,name,enabled,models,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT(id) DO UPDATE SET credential_id=excluded.credential_id,name=excluded.name,enabled=excluded.enabled,models=excluded.models,etag=excluded.etag,
			health=CASE WHEN code_accounts.credential_id<>excluded.credential_id THEN 'unknown' ELSE code_accounts.health END`, id, in.ProjectID, in.ProviderID, in.CredentialID, principal, in.Name, in.Enabled, models, etag, p.UserID())
		if err != nil {
			return nil, err
		}
		var body json.RawMessage
		err = tx.QueryRow(r.Context(), codeAccountJSON+` WHERE x.id=$1`, id).Scan(&body)
		return body, err
	})
}
