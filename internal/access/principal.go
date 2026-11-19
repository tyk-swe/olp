package access

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/secrets"
)

const userColumns = "u.id::text,u.email,u.display_name,u.role,u.active,u.access_scope,u.etag::text,u.created_at,u.updated_at"

func scanUser(row pgx.Row) (authz.User, error) {
	var u authz.User
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Active, &u.AccessScope, &u.ETag, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

// Authenticate resolves the caller from a management token or the session
// cookie and, for a browser session, requires the CSRF proof on unsafe
// methods. It authorizes no operation.
func (s *Server) Authenticate(r *http.Request, q database.Queryer) (authz.Principal, error) {
	if secret, ok := managementBearer(r); ok {
		return s.machinePrincipal(r, q, secret)
	}
	var p authz.Principal
	p.Kind = "user"
	p.Token = cookieValue(r, sessionCookie)
	err := q.QueryRow(r.Context(), "SELECT "+userColumns+",s.id::text FROM olp.sessions s JOIN olp.users u ON u.id=s.user_id WHERE s.digest=$1 AND s.expires_at>now() AND u.active AND u.oidc_authorized AND (s.auth_method<>'local' OR s.mfa_verified OR (NOT EXISTS(SELECT 1 FROM olp.mfa_factors f WHERE f.user_id=u.id) AND NOT COALESCE((SELECT value<>'false' FROM olp.settings WHERE key='auth.mfa_required'),false)))", s.Auth.Digest(secrets.SessionDigest, p.Token)).Scan(&p.ID, &p.Email, &p.DisplayName, &p.Role, &p.Active, &p.AccessScope, &p.ETag, &p.CreatedAt, &p.UpdatedAt, &p.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, httpkit.Fail(401, "authentication_required", "Sign in to continue.")
	}
	if err != nil {
		return p, err
	}
	p.AllProjects = p.AccessScope == "global"
	if err = loadProjects(r.Context(), q, &p); err != nil {
		return p, err
	}
	if r.Method != "GET" && r.Method != "HEAD" && !hmac.Equal([]byte(r.Header.Get(csrfHeader)), []byte(s.csrf(p.Token))) {
		return p, httpkit.Fail(403, "csrf_invalid", "Reload the console before trying again.")
	}
	return p, nil
}

func loadProjects(ctx context.Context, q database.Queryer, p *authz.Principal) error {
	rows, err := q.Query(ctx, "SELECT project_id::text,role FROM olp.effective_project_members WHERE user_id=$1", p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, role string
		if err = rows.Scan(&id, &role); err != nil {
			return err
		}
		if p.Projects == nil {
			p.Projects = map[string]string{}
		}
		p.Projects[id] = role
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	return loadOrganizations(ctx, q, p, p.ID)
}
func managementBearer(r *http.Request) (string, bool) {
	const prefix = "Bearer olpm_"
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	return strings.TrimPrefix(header, "Bearer "), true
}

// machinePrincipal authenticates a management token. A token acts within its
// creator's current authority: an inactive or deauthorized creator retires it,
// Authorize bounds its scopes by the creator's role, and an assigned creator
// narrows it to the creator's projects and project roles. The stored token is
// never changed.
func (s *Server) machinePrincipal(r *http.Request, q database.Queryer, secret string) (authz.Principal, error) {
	var p authz.Principal
	p.Kind = "machine"
	parts := strings.Split(secret, "_")
	if len(parts) != 3 {
		return p, httpkit.Fail(401, "authentication_required", "Sign in to continue.")
	}
	var digest, data, projectData, memberData []byte
	var live, allProjects bool
	var creatorScope, creatorRole string
	err := q.QueryRow(r.Context(), `SELECT t.id::text,t.name,t.scopes,t.digest,t.created_by::text,
		t.expires_at>now() AND t.revoked_at IS NULL AND u.active AND u.oidc_authorized,
		t.all_projects,t.project_ids,u.role,u.access_scope,
		COALESCE((SELECT jsonb_object_agg(m.project_id,m.role) FROM olp.effective_project_members m WHERE m.user_id=u.id),'{}'::jsonb)
		FROM olp.management_tokens t JOIN olp.users u ON u.id=t.created_by WHERE t.lookup_id=$1`, parts[1]).Scan(&p.ID, &p.DisplayName, &data, &digest, &p.Creator, &live, &allProjects, &projectData, &creatorRole, &creatorScope, &memberData)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!live || !hmac.Equal(digest, s.Auth.Digest(secrets.ManagementTokenDigest, secret))) {
		return p, httpkit.Fail(401, "authentication_required", "Sign in to continue.")
	}
	if err != nil {
		return p, err
	}
	var names []string
	if err = json.Unmarshal(data, &names); err != nil {
		return p, err
	}
	var scopes []authz.Operation
	for _, name := range names {
		if op, ok := authz.ParseOperation(name); ok {
			scopes = append(scopes, op)
		}
	}
	p = p.Delegated(scopes, creatorRole)
	if allProjects {
		if err = loadOrganizations(r.Context(), q, &p, p.Creator); err != nil {
			return p, err
		}
	}
	creatorGlobal := creatorScope == "global"
	p.AllProjects = allProjects && creatorGlobal
	if p.AllProjects {
		p.AccessScope = "global"
		return p, nil
	}
	p.AccessScope = "assigned"
	var memberships map[string]string
	if err = json.Unmarshal(memberData, &memberships); err != nil {
		return p, err
	}
	projectIDs := slices.Collect(maps.Keys(memberships))
	if !allProjects {
		if err = json.Unmarshal(projectData, &projectIDs); err != nil {
			return p, err
		}
	}
	p.Projects = map[string]string{}
	for _, id := range projectIDs {
		if creatorGlobal {
			p.Projects[id] = "manager"
		} else if role, ok := memberships[id]; ok {
			p.Projects[id] = role
		}
	}
	return p, nil
}
func (s *Server) csrf(token string) string {
	return base64.RawURLEncoding.EncodeToString(s.Auth.Digest(secrets.CSRFDigest, token))
}
