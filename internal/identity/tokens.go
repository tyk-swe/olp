package identity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type managementTokenInput struct {
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at"`
	ProjectIDs *[]string  `json:"project_ids"`
}

func validManagementToken(input managementTokenInput) error {
	if err := httpkit.ValidText("name", input.Name, 100); err != nil {
		return err
	}
	delegable := authz.TokenScopes()
	if len(input.Scopes) < 1 || len(input.Scopes) > len(delegable) {
		return httpkit.Invalid("scopes", fmt.Sprintf("Select 1–%d unique scopes.", len(delegable)))
	}
	seen := map[string]bool{}
	for _, scope := range input.Scopes {
		op, ok := authz.ParseOperation(scope)
		if seen[scope] || !ok || !slices.Contains(delegable, op) {
			return httpkit.Invalid("scopes", "Use unique management operation scopes.")
		}
		seen[scope] = true
	}
	if input.ProjectIDs != nil {
		if len(*input.ProjectIDs) < 1 || len(*input.ProjectIDs) > 100 {
			return httpkit.Invalid("project_ids", "Select 1–100 unique projects, or omit the field for all projects.")
		}
		seen = map[string]bool{}
		for i, id := range *input.ProjectIDs {
			parsed, err := httpkit.ParseUUID(id)
			if err != nil || seen[parsed] {
				return httpkit.Invalid("project_ids", "Use unique project identifiers.")
			}
			(*input.ProjectIDs)[i] = parsed
			seen[parsed] = true
		}
	}
	if input.ExpiresAt == nil {
		return httpkit.Invalid("expires_at", "Choose an expiry.")
	}
	now := time.Now()
	if !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(366*24*time.Hour)) {
		return httpkit.Invalid("expires_at", "Choose a future expiry no more than 366 days ahead.")
	}
	return nil
}

const managementTokenFields = `'id',t.id,'lookup_id',t.lookup_id,'name',t.name,'scopes',t.scopes,'all_projects',t.all_projects,'project_ids',t.project_ids,'created_by',t.created_by,'created_by_email',u.email,'etag',t.etag,'expires_at',t.expires_at,'revoked_at',t.revoked_at,'created_at',t.created_at`
const managementTokenFrom = " FROM olp.management_tokens t JOIN olp.users u ON u.id=t.created_by"

func (s *Server) managementTokens(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	p, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT jsonb_build_object("+managementTokenFields+")"+managementTokenFrom+" WHERE t.id<$1 ORDER BY t.id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.ListReply(items, p), err
}

func (s *Server) managementToken(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "management_token_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var data []byte
	var etag string
	err = s.access.Pool.QueryRow(r.Context(), "SELECT jsonb_build_object("+managementTokenFields+"),t.etag::text"+managementTokenFrom+" WHERE t.id=$1", id).Scan(&data, &etag)
	return httpkit.Detail(json.RawMessage(data), etag), err
}

func (s *Server) createManagementToken(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input managementTokenInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.access, r, input, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err := validManagementToken(input); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		projectIDs := []string{}
		allProjects := input.ProjectIDs == nil
		if input.ProjectIDs != nil {
			projectIDs = *input.ProjectIDs
			var known int64
			if err := tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.projects WHERE id=ANY($1::uuid[])", projectIDs).Scan(&known); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if int(known) != len(projectIDs) {
				return httpkit.Reply{}, access.Event{}, httpkit.Invalid("project_ids", "Every project must exist.")
			}
		}
		id, lookup, etag := httpkit.NewID(), secrets.Token(), httpkit.NewID()
		secret := "olpm_" + lookup + "_" + secrets.Token()
		scopes, err := json.Marshal(input.Scopes)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		projectData, err := json.Marshal(projectIDs)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var createdAt time.Time
		if err = tx.QueryRow(r.Context(), "INSERT INTO olp.management_tokens(id,lookup_id,digest,name,scopes,all_projects,project_ids,created_by,etag,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at", id, lookup, s.access.Auth.Digest(secrets.ManagementTokenDigest, secret), strings.TrimSpace(input.Name), scopes, allProjects, projectData, p.ID, etag, input.ExpiresAt).Scan(&createdAt); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		body := map[string]any{"id": id, "lookup_id": lookup, "name": strings.TrimSpace(input.Name), "scopes": input.Scopes, "all_projects": allProjects, "project_ids": projectIDs, "created_by": p.ID, "created_by_email": p.Email, "etag": etag, "expires_at": input.ExpiresAt, "revoked_at": nil, "created_at": createdAt, "secret": secret}
		result := httpkit.Reply{Status: 201, ETag: etag, Location: "/api/v1/management-tokens/" + id, Body: body}
		return result, access.Event{Action: "management_token.create", Resource: "management_token", ID: id}, nil
	})
}

func (s *Server) revokeManagementToken(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "management_token_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.access, r, nil, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var etag string
		var revoked *time.Time
		if err = tx.QueryRow(r.Context(), "SELECT etag::text,revoked_at FROM olp.management_tokens WHERE id=$1", id).Scan(&etag, &revoked); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if revoked != nil {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "management_token_revoked", "This token is already revoked.")
		}
		etag = httpkit.NewID()
		if _, err = tx.Exec(r.Context(), "UPDATE olp.management_tokens SET revoked_at=now(),etag=$1 WHERE id=$2", etag, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result := httpkit.Detail(map[string]any{"id": id, "etag": etag}, etag)
		return result, access.Event{Action: "management_token.revoke", Resource: "management_token", ID: id}, nil
	})
}
