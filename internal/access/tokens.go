package access

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/secrets"
)

var managementScopeNames = []string{"read", "access_read", "access", "settings", "configure", "keys", "playground", "usage"}

type managementTokenInput struct {
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at"`
	ProjectIDs *[]string  `json:"project_ids"`
}

func validManagementToken(input managementTokenInput) error {
	if err := ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if len(input.Scopes) < 1 || len(input.Scopes) > 8 {
		return Invalid("scopes", "Select 1–8 unique scopes.")
	}
	seen := map[string]bool{}
	for _, scope := range input.Scopes {
		if seen[scope] || !slices.Contains(managementScopeNames, scope) {
			return Invalid("scopes", "Use unique management operation scopes.")
		}
		seen[scope] = true
	}
	if input.ProjectIDs != nil {
		if len(*input.ProjectIDs) < 1 || len(*input.ProjectIDs) > 100 {
			return Invalid("project_ids", "Select 1–100 unique projects, or omit the field for all projects.")
		}
		seen = map[string]bool{}
		for i, id := range *input.ProjectIDs {
			parsed, err := ParseUUID(id)
			if err != nil || seen[parsed] {
				return Invalid("project_ids", "Use unique project identifiers.")
			}
			(*input.ProjectIDs)[i] = parsed
			seen[parsed] = true
		}
	}
	if input.ExpiresAt == nil {
		return Invalid("expires_at", "Choose an expiry.")
	}
	now := time.Now()
	if !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(366*24*time.Hour)) {
		return Invalid("expires_at", "Choose a future expiry no more than 366 days ahead.")
	}
	return nil
}

const managementTokenFields = `'id',t.id,'lookup_id',t.lookup_id,'name',t.name,'scopes',t.scopes,'all_projects',t.all_projects,'project_ids',t.project_ids,'created_by',t.created_by,'created_by_email',u.email,'etag',t.etag,'expires_at',t.expires_at,'revoked_at',t.revoked_at,'created_at',t.created_at`
const managementTokenFrom = " FROM olp_go.management_tokens t JOIN olp_go.users u ON u.id=t.created_by"

func (s *Server) ownerPrincipal(r *http.Request, q Queryer) (Principal, error) {
	p, err := s.Principal(r, q, "access")
	if err != nil {
		return p, err
	}
	if p.Kind != "user" || p.Role != "owner" {
		return p, Forbidden()
	}
	return p, nil
}

func (s *Server) managementTokens(r *http.Request) (Reply, error) {
	if _, err := s.ownerPrincipal(r, s.Pool); err != nil {
		return Reply{}, err
	}
	p, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT jsonb_build_object("+managementTokenFields+")"+managementTokenFrom+" WHERE t.id<$1 ORDER BY t.id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, p), err
}

func (s *Server) managementToken(r *http.Request) (Reply, error) {
	if _, err := s.ownerPrincipal(r, s.Pool); err != nil {
		return Reply{}, err
	}
	id, err := IDParam(r, "management_token_id")
	if err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	err = s.Pool.QueryRow(r.Context(), "SELECT jsonb_build_object("+managementTokenFields+"),t.etag::text"+managementTokenFrom+" WHERE t.id=$1", id).Scan(&data, &etag)
	return Detail(json.RawMessage(data), etag), err
}

func (s *Server) createManagementToken(r *http.Request) (Reply, error) {
	var input managementTokenInput
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.ownerPrincipal(r, tx)
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	if err = validManagementToken(input); err != nil {
		return Reply{}, err
	}
	projectIDs := []string{}
	allProjects := input.ProjectIDs == nil
	if input.ProjectIDs != nil {
		projectIDs = *input.ProjectIDs
		var known int64
		if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM olp_go.projects WHERE id=ANY($1::uuid[])", projectIDs).Scan(&known); err != nil {
			return Reply{}, err
		}
		if int(known) != len(projectIDs) {
			return Reply{}, Invalid("project_ids", "Every project must exist.")
		}
	}
	id, lookup, etag := NewID(), secrets.Token(), NewID()
	secret := "olpm_" + lookup + "_" + secrets.Token()
	scopes, err := json.Marshal(input.Scopes)
	if err != nil {
		return Reply{}, err
	}
	projectData, err := json.Marshal(projectIDs)
	if err != nil {
		return Reply{}, err
	}
	var createdAt time.Time
	if err = tx.QueryRow(r.Context(), "INSERT INTO olp_go.management_tokens(id,lookup_id,digest,name,scopes,all_projects,project_ids,created_by,etag,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at", id, lookup, s.Auth.Digest("management_token", secret), strings.TrimSpace(input.Name), scopes, allProjects, projectData, p.ID, etag, input.ExpiresAt).Scan(&createdAt); err != nil {
		return Reply{}, err
	}
	body := map[string]any{"id": id, "lookup_id": lookup, "name": strings.TrimSpace(input.Name), "scopes": input.Scopes, "all_projects": allProjects, "project_ids": projectIDs, "created_by": p.ID, "created_by_email": p.Email, "etag": etag, "expires_at": input.ExpiresAt, "revoked_at": nil, "created_at": createdAt, "secret": secret}
	result := Reply{Status: 201, ETag: etag, Location: "/api/v3/management-tokens/" + id, Body: body}
	if err = Audit(r.Context(), tx, r, p.ID, "management_token.create", "management_token", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func (s *Server) revokeManagementToken(r *http.Request) (Reply, error) {
	id, err := IDParam(r, "management_token_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.ownerPrincipal(r, tx)
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, nil)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	var etag string
	var revoked *time.Time
	if err = tx.QueryRow(r.Context(), "SELECT etag::text,revoked_at FROM olp_go.management_tokens WHERE id=$1", id).Scan(&etag, &revoked); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if revoked != nil {
		return Reply{}, Fail(409, "management_token_revoked", "This token is already revoked.")
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.management_tokens SET revoked_at=now(),etag=$1 WHERE id=$2", etag, id); err != nil {
		return Reply{}, err
	}
	result := Detail(map[string]any{"id": id, "etag": etag}, etag)
	if err = Audit(r.Context(), tx, r, p.ID, "management_token.revoke", "management_token", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}
