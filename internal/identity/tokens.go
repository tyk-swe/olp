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
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type managementTokenInput struct {
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at"`
	ProjectIDs *[]string  `json:"project_ids"`
}

type managementTokenRecord struct {
	ID             string     `json:"id"`
	LookupID       string     `json:"lookup_id"`
	Name           string     `json:"name"`
	Scopes         []string   `json:"scopes"`
	AllProjects    bool       `json:"all_projects"`
	ProjectIDs     []string   `json:"project_ids"`
	CreatedBy      string     `json:"created_by"`
	CreatedByEmail string     `json:"created_by_email"`
	ETag           string     `json:"etag"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RevokedAt      *time.Time `json:"revoked_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

type createdManagementToken struct {
	managementTokenRecord
	Secret string `json:"secret"`
}

type revokedManagementToken struct {
	ID   string `json:"id"`
	ETag string `json:"etag"`
}

func validManagementToken(input managementTokenInput) ([]string, error) {
	return validManagementTokenAt(input, time.Now())
}

func validManagementTokenAt(input managementTokenInput, now time.Time) ([]string, error) {
	if err := textfield.Validate("name", input.Name, 100); err != nil {
		return nil, err
	}
	delegable := authz.TokenScopes()
	if len(input.Scopes) < 1 || len(input.Scopes) > len(delegable) {
		return nil, problem.Invalid("scopes", fmt.Sprintf("Select 1–%d unique scopes.", len(delegable)))
	}
	seenScopes := make(map[string]bool, len(input.Scopes))
	for _, scope := range input.Scopes {
		op, ok := authz.ParseOperation(scope)
		if seenScopes[scope] || !ok || !slices.Contains(delegable, op) {
			return nil, problem.Invalid("scopes", "Use unique management operation scopes.")
		}
		seenScopes[scope] = true
	}
	projectIDs := []string{}
	if input.ProjectIDs != nil {
		if len(*input.ProjectIDs) < 1 || len(*input.ProjectIDs) > 100 {
			return nil, problem.Invalid("project_ids", "Select 1–100 unique projects, or omit the field for all projects.")
		}
		seenProjects := make(map[string]bool, len(*input.ProjectIDs))
		projectIDs = make([]string, 0, len(*input.ProjectIDs))
		for _, id := range *input.ProjectIDs {
			parsed, err := ids.Parse(id)
			if err != nil || seenProjects[parsed] {
				return nil, problem.Invalid("project_ids", "Use unique project identifiers.")
			}
			projectIDs = append(projectIDs, parsed)
			seenProjects[parsed] = true
		}
	}
	if input.ExpiresAt == nil {
		return nil, problem.Invalid("expires_at", "Choose an expiry.")
	}
	if !input.ExpiresAt.After(now) || input.ExpiresAt.After(now.Add(366*24*time.Hour)) {
		return nil, problem.Invalid("expires_at", "Choose a future expiry no more than 366 days ahead.")
	}
	return projectIDs, nil
}

const managementTokenColumns = `t.id::text,t.lookup_id,t.name,t.scopes,t.all_projects,t.project_ids,t.created_by::text,u.email,t.etag::text,t.expires_at,t.revoked_at,t.created_at`
const managementTokenFrom = " FROM olp.management_tokens t JOIN olp.users u ON u.id=t.created_by"

func scanManagementToken(row pgx.Row) (managementTokenRecord, error) {
	var rec managementTokenRecord
	var scopesRaw, projectIDsRaw []byte
	err := row.Scan(
		&rec.ID,
		&rec.LookupID,
		&rec.Name,
		&scopesRaw,
		&rec.AllProjects,
		&projectIDsRaw,
		&rec.CreatedBy,
		&rec.CreatedByEmail,
		&rec.ETag,
		&rec.ExpiresAt,
		&rec.RevokedAt,
		&rec.CreatedAt,
	)
	if err != nil {
		return managementTokenRecord{}, err
	}
	if err := json.Unmarshal(scopesRaw, &rec.Scopes); err != nil {
		return managementTokenRecord{}, err
	}
	if err := json.Unmarshal(projectIDsRaw, &rec.ProjectIDs); err != nil {
		return managementTokenRecord{}, err
	}
	if rec.Scopes == nil {
		rec.Scopes = []string{}
	}
	if rec.ProjectIDs == nil {
		rec.ProjectIDs = []string{}
	}
	return rec, nil
}

func (s *accountService) managementTokens(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	p, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.access.Pool.Query(r.Context(), "SELECT "+managementTokenColumns+managementTokenFrom+" WHERE t.id<$1 ORDER BY t.id DESC LIMIT $2", p.Before, p.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []managementTokenRecord{}
	for rows.Next() {
		rec, err := scanManagementToken(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, rec)
	}
	if err := rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, p, func(rec managementTokenRecord) string { return rec.ID }), nil
}

func (s *accountService) managementToken(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "management_token_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	rec, err := scanManagementToken(s.access.Pool.QueryRow(r.Context(), "SELECT "+managementTokenColumns+managementTokenFrom+" WHERE t.id=$1", id))
	return httpkit.Detail(rec, rec.ETag), err
}

func (s *accountService) createManagementToken(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input managementTokenInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.access, r, input, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		projectIDs, err := validManagementToken(input)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		allProjects := input.ProjectIDs == nil
		if !allProjects {
			var known int64
			if err := tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.projects WHERE id=ANY($1::uuid[])", projectIDs).Scan(&known); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if int(known) != len(projectIDs) {
				return httpkit.Reply{}, access.Event{}, problem.Invalid("project_ids", "Every project must exist.")
			}
		}
		id, lookup, etag := ids.New(), secrets.Token(), ids.New()
		secret := "olpm_" + lookup + "_" + secrets.Token()
		name := strings.TrimSpace(input.Name)
		scopes, err := json.Marshal(input.Scopes)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		projectData, err := json.Marshal(projectIDs)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var createdAt time.Time
		if err := tx.QueryRow(r.Context(), "INSERT INTO olp.management_tokens(id,lookup_id,digest,name,scopes,all_projects,project_ids,created_by,etag,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at", id, lookup, s.access.Auth.Digest(secrets.ManagementTokenDigest, secret), name, scopes, allProjects, projectData, p.ID, etag, input.ExpiresAt).Scan(&createdAt); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		body := createdManagementToken{
			managementTokenRecord: managementTokenRecord{
				ID:             id,
				LookupID:       lookup,
				Name:           name,
				Scopes:         input.Scopes,
				AllProjects:    allProjects,
				ProjectIDs:     projectIDs,
				CreatedBy:      p.ID,
				CreatedByEmail: p.Email,
				ETag:           etag,
				ExpiresAt:      *input.ExpiresAt,
				RevokedAt:      nil,
				CreatedAt:      createdAt,
			},
			Secret: secret,
		}
		result := httpkit.Reply{Status: 201, ETag: etag, Location: "/api/v1/management-tokens/" + id, Body: body}
		return result, access.Event{Action: "management_token.create", Resource: "management_token", ID: id}, nil
	})
}

func (s *accountService) revokeManagementToken(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "management_token_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.access, r, nil, access.Unlocked, func(tx pgx.Tx, _ authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var etag string
		var revoked *time.Time
		if err := tx.QueryRow(r.Context(), "SELECT etag::text,revoked_at FROM olp.management_tokens WHERE id=$1", id).Scan(&etag, &revoked); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if revoked != nil {
			return httpkit.Reply{}, access.Event{}, problem.Fail(409, "management_token_revoked", "This token is already revoked.")
		}
		nextETag := ids.New()
		if _, err := tx.Exec(r.Context(), "UPDATE olp.management_tokens SET revoked_at=now(),etag=$1 WHERE id=$2", nextETag, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result := httpkit.Detail(revokedManagementToken{ID: id, ETag: nextETag}, nextETag)
		return result, access.Event{Action: "management_token.revoke", Resource: "management_token", ID: id}, nil
	})
}
