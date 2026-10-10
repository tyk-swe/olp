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
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/secrets"
)

type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
type User struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Active      bool      `json:"active"`
	AccessScope string    `json:"access_scope"`
	ETag        string    `json:"etag"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Principal is an authenticated management caller: a signed-in member
// ("user"), or a management token ("machine") acting within its creator's
// current authority.
type Principal struct {
	User
	Kind string
	// KeyAuthority is set only by an inference-key authentication alternative.
	// It grants consumer catalog reads, never management mutation authority.
	KeyAuthority     *Authority
	Creator          string
	AllProjects      bool
	Projects         map[string]string
	Organizations    map[string]string
	SessionID, Token string

	// scopes and creatorRole bound a management token's authority.
	scopes      operationSet
	creatorRole string
}

func (p Principal) UserID() string {
	if p.Kind == "machine" {
		return p.Creator
	}
	return p.ID
}

const userColumns = "u.id::text,u.email,u.display_name,u.role,u.active,u.access_scope,u.etag::text,u.created_at,u.updated_at"

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Active, &u.AccessScope, &u.ETag, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

// Authenticate resolves the caller from a management token or the session
// cookie and, for a browser session, requires the CSRF proof on unsafe
// methods. It authorizes no operation.
func (s *Server) Authenticate(r *http.Request, q Queryer) (Principal, error) {
	if secret, ok := managementBearer(r); ok {
		return s.machinePrincipal(r, q, secret)
	}
	var p Principal
	p.Kind = "user"
	p.Token = cookieValue(r, sessionCookie)
	err := q.QueryRow(r.Context(), "SELECT "+userColumns+",s.id::text FROM olp.sessions s JOIN olp.users u ON u.id=s.user_id WHERE s.digest=$1 AND s.expires_at>now() AND u.active AND u.oidc_authorized AND (s.auth_method<>'local' OR s.mfa_verified OR (NOT EXISTS(SELECT 1 FROM olp.mfa_factors f WHERE f.user_id=u.id) AND NOT COALESCE((SELECT value<>'false' FROM olp.settings WHERE key='auth.mfa_required'),false)))", s.Auth.Digest(secrets.SessionDigest, p.Token)).Scan(&p.ID, &p.Email, &p.DisplayName, &p.Role, &p.Active, &p.AccessScope, &p.ETag, &p.CreatedAt, &p.UpdatedAt, &p.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, Fail(401, "authentication_required", "Sign in to continue.")
	}
	if err != nil {
		return p, err
	}
	p.AllProjects = p.AccessScope == "global"
	if err = p.loadProjects(r.Context(), q); err != nil {
		return p, err
	}
	if r.Method != "GET" && r.Method != "HEAD" && !hmac.Equal([]byte(r.Header.Get(csrfHeader)), []byte(s.csrf(p.Token))) {
		return p, Fail(403, "csrf_invalid", "Reload the console before trying again.")
	}
	return p, nil
}

func (p *Principal) loadProjects(ctx context.Context, q Queryer) error {
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
	return p.loadOrganizations(ctx, q, p.ID)
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
func (s *Server) machinePrincipal(r *http.Request, q Queryer, secret string) (Principal, error) {
	var p Principal
	p.Kind = "machine"
	parts := strings.Split(secret, "_")
	if len(parts) != 3 {
		return p, Fail(401, "authentication_required", "Sign in to continue.")
	}
	var digest, data, projectData, memberData []byte
	var live, allProjects bool
	var creatorScope string
	err := q.QueryRow(r.Context(), `SELECT t.id::text,t.name,t.scopes,t.digest,t.created_by::text,
		t.expires_at>now() AND t.revoked_at IS NULL AND u.active AND u.oidc_authorized,
		t.all_projects,t.project_ids,u.role,u.access_scope,
		COALESCE((SELECT jsonb_object_agg(m.project_id,m.role) FROM olp.effective_project_members m WHERE m.user_id=u.id),'{}'::jsonb)
		FROM olp.management_tokens t JOIN olp.users u ON u.id=t.created_by WHERE t.lookup_id=$1`, parts[1]).Scan(&p.ID, &p.DisplayName, &data, &digest, &p.Creator, &live, &allProjects, &projectData, &p.creatorRole, &creatorScope, &memberData)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (!live || !hmac.Equal(digest, s.Auth.Digest(secrets.ManagementTokenDigest, secret))) {
		return p, Fail(401, "authentication_required", "Sign in to continue.")
	}
	if err != nil {
		return p, err
	}
	var scopes []string
	if err = json.Unmarshal(data, &scopes); err != nil {
		return p, err
	}
	for _, name := range scopes {
		if op, ok := ParseOperation(name); ok {
			p.scopes |= 1 << op
		}
	}
	if allProjects {
		if err = p.loadOrganizations(r.Context(), q, p.Creator); err != nil {
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
