package mcpservers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Server struct {
	Access *access.Server
	Egress *egress.Policy
}
type Definition struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	Name          string     `json:"name"`
	Transport     string     `json:"transport"`
	Endpoint      string     `json:"endpoint"`
	Enabled       bool       `json:"enabled"`
	HasCredential bool       `json:"has_credential"`
	ETag          string     `json:"etag"`
	RevisionID    string     `json:"revision_id"`
	Revision      int64      `json:"revision"`
	Catalog       *Catalog   `json:"catalog"`
	CertifiedAt   time.Time  `json:"certified_at"`
	RetiredAt     *time.Time `json:"retired_at"`
	CredentialID  *string    `json:"-"`
}
type Input struct {
	ProjectID  *string         `json:"project_id,omitempty"`
	Name       string          `json:"name"`
	Transport  string          `json:"transport,omitempty"`
	Endpoint   string          `json:"endpoint"`
	Enabled    *bool           `json:"enabled"`
	Credential json.RawMessage `json:"credential,omitempty"`
}

const columns = "s.id::text,s.project_id::text,s.name,s.transport,s.endpoint,s.enabled,s.credential_id,s.etag::text,s.retired_at,v.id::text,v.revision,v.protocol_version,v.tools,v.digest,v.certified_at"
const joined = " FROM olp.mcp_servers s JOIN olp.mcp_server_revisions v ON v.id=s.latest_revision_id"

func scan(row pgx.Row) (*Definition, error) {
	d := &Definition{Catalog: &Catalog{}}
	err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Transport, &d.Endpoint, &d.Enabled, &d.CredentialID, &d.ETag, &d.RetiredAt, &d.RevisionID, &d.Revision, &d.Catalog.Protocol, &d.Catalog.Tools, &d.Catalog.Digest, &d.CertifiedAt)
	d.HasCredential = d.CredentialID != nil
	return d, err
}
func Load(ctx context.Context, q access.Queryer, id string, lock bool) (*Definition, error) {
	query := "SELECT " + columns + joined + " WHERE s.id=$1"
	if lock {
		query += " FOR UPDATE OF s"
	}
	return scan(q.QueryRow(ctx, query, id))
}
func FindActive(ctx context.Context, q access.Queryer, project, name string) (*Definition, error) {
	return scan(q.QueryRow(ctx, "SELECT "+columns+joined+" WHERE s.project_id=$1 AND lower(s.name)=lower($2) AND s.retired_at IS NULL", project, name))
}
func (s *Server) Register(mux *http.ServeMux) {
	a := s.Access
	a.Route(mux, "GET /api/v1/mcp-servers", s.list)
	a.Route(mux, "POST /api/v1/mcp-servers", s.create, access.Deadline(20*time.Second))
	a.Route(mux, "GET /api/v1/mcp-servers/{server_id}", s.get)
	a.Route(mux, "GET /api/v1/mcp-servers/{server_id}/revisions/{revision_id}", s.revision)
	a.Route(mux, "PUT /api/v1/mcp-servers/{server_id}", s.update, access.Deadline(20*time.Second))
	a.Route(mux, "DELETE /api/v1/mcp-servers/{server_id}", s.retire)
}
func (s *Server) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	// Collection summaries must not load up to 200 full schema catalogs.
	summaryColumns := strings.Replace(columns, "v.tools", "'[]'::jsonb", 1)
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+summaryColumns+joined+" WHERE s.retired_at IS NULL AND s.id<$1 AND ($2 OR s.project_id=ANY($3::uuid[])) ORDER BY s.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return access.Reply{}, err
		}
		raw, _ := json.Marshal(d)
		var item map[string]any
		_ = json.Unmarshal(raw, &item)
		delete(item, "catalog")
		items = append(items, item)
	}
	return access.ListReply(items, page), rows.Err()
}
func (s *Server) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "server_id")
	if err != nil {
		return access.Reply{}, err
	}
	d, err := Load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(d, d.ETag), nil
}
func (s *Server) revision(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "server_id")
	if err != nil {
		return access.Reply{}, err
	}
	revision, err := access.IDParam(r, "revision_id")
	if err != nil {
		return access.Reply{}, err
	}
	d, err := Load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	var version int64
	catalog := Catalog{}
	var endpoint string
	var certified time.Time
	err = s.Access.Pool.QueryRow(r.Context(), "SELECT revision,endpoint,protocol_version,tools,digest,certified_at FROM olp.mcp_server_revisions WHERE server_id=$1 AND id=$2", id, revision).Scan(&version, &endpoint, &catalog.Protocol, &catalog.Tools, &catalog.Digest, &certified)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"id": revision, "server_id": id, "revision": version, "endpoint": endpoint, "catalog": catalog, "certified_at": certified}, revision), nil
}
func Validate(in *Input, policy *egress.Policy) error {
	in.Name = strings.TrimSpace(in.Name)
	if err := access.ValidText("name", in.Name, 100); err != nil {
		return err
	}
	if in.Enabled == nil {
		return access.Invalid("enabled", "Supply an explicit enabled flag.")
	}
	if policy == nil {
		return access.Fail(503, "egress_policy_unavailable", "Certification requires provider egress policy.")
	}
	u, err := policy.ValidateEndpoint(in.Endpoint)
	if err != nil {
		return access.Invalid("endpoint", "The MCP endpoint is not permitted by provider egress policy.")
	}
	in.Endpoint = u.String()
	if in.Transport != "" && in.Transport != "streamable_http" {
		return access.Invalid("transport", "Use streamable_http.")
	}
	return nil
}
func Credential(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || len(value) < 1 || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return nil, access.Invalid("credential", "Supply bounded bearer material, or null to remove it.")
	}
	return []byte(value), nil
}
func (s *Server) create(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.write(r, p, "", false)
}
func (s *Server) update(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "server_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.write(r, p, id, false)
}
func (s *Server) retire(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "server_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.write(r, p, id, true)
}

// preflight authenticates and checks replay/preconditions before network IO,
// then releases every database lock. The commit phase repeats the checks so
// upstream validation cannot race a new owner or draft observation.
func (s *Server) preflight(r *http.Request, in Input, id string) (*Definition, *access.Reply, error) {
	tx, err := s.Access.Begin(r)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return nil, nil, err
	}
	var d *Definition
	if id != "" {
		d, err = Load(r.Context(), tx, id, true)
		if err != nil {
			return nil, nil, err
		}
		if err = p.Project(&d.ProjectID, access.Change); err != nil {
			return nil, nil, err
		}
	} else {
		if in.ProjectID == nil || in.Transport != "streamable_http" {
			return nil, nil, access.Invalid("project_id", "Choose an owning project and streamable_http transport.")
		}
		if err = s.Access.RequireProject(r.Context(), tx, p, in.ProjectID); err != nil {
			return nil, nil, err
		}
	}
	_, replayed, err := s.Access.Replay(r, tx, p, in)
	if err != nil {
		return nil, nil, err
	}
	if replayed != nil {
		reply, err := access.Commit(r, tx, *replayed)
		return d, &reply, err
	}
	if d != nil {
		if err = access.Match(r, d.ETag); err != nil {
			return nil, nil, err
		}
		if d.RetiredAt != nil {
			return nil, nil, access.Fail(409, "mcp_server_retired", "Create a new registration instead of changing retained history.")
		}
		if in.ProjectID != nil || in.Transport != "" {
			return nil, nil, access.Invalid("project_id", "Project and transport are immutable.")
		}
	}
	return d, nil, nil
}
func (s *Server) write(r *http.Request, _ access.Principal, id string, retire bool) (access.Reply, error) {
	var in Input
	if !retire {
		if err := access.DecodeUnique(r, &in, 16<<10); err != nil {
			return access.Reply{}, err
		}
		if err := Validate(&in, s.Egress); err != nil {
			return access.Reply{}, err
		}
	}
	previous, replayed, err := s.preflight(r, in, id)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, nil
	}
	var material []byte
	var catalog *Catalog
	if !retire {
		material, err = Credential(in.Credential)
		if err != nil {
			return access.Reply{}, err
		}
		if len(in.Credential) == 0 && previous != nil && previous.CredentialID != nil {
			if in.Endpoint != previous.Endpoint {
				return access.Reply{}, access.Invalid("credential", "Supply a replacement credential or null when changing the MCP endpoint.")
			}
			material, err = s.Access.Keys.Read(r.Context(), s.Access.Pool, s.Access.Installation, *previous.CredentialID, secrets.MCPCredential)
			if err != nil {
				return access.Reply{}, access.Fail(422, "mcp_credential_unavailable", "The registered credential is unavailable.")
			}
		}
		defer clear(material)
		catalog, err = Certify(r.Context(), s.Egress, in.Endpoint, material)
		if err != nil {
			return access.Reply{}, access.Fail(422, "mcp_certification_failed", "The bounded MCP handshake or tool-schema certification failed.")
		}
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var current *Definition
	if id != "" {
		current, err = Load(r.Context(), tx, id, true)
		if err != nil {
			return access.Reply{}, err
		}
		if err = p.Project(&current.ProjectID, access.Change); err != nil {
			return access.Reply{}, err
		}
	} else {
		if err = s.Access.RequireProject(r.Context(), tx, p, in.ProjectID); err != nil {
			return access.Reply{}, err
		}
	}
	claim, replay, err := s.Access.Replay(r, tx, p, in)
	if err != nil {
		return access.Reply{}, err
	}
	if replay != nil {
		return access.Commit(r, tx, *replay)
	}
	if current != nil {
		if err = access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		if current.ETag != previous.ETag {
			return access.Reply{}, access.Fail(412, "etag_mismatch", "The MCP registration changed during certification.")
		}
	}
	action := "mcp_server.update"
	etag := access.NewID()
	if retire {
		action = "mcp_server.retire"
		_, err = tx.Exec(r.Context(), "UPDATE olp.mcp_servers SET enabled=false,retired_at=now(),credential_id=NULL,etag=$2 WHERE id=$1", id, etag)
	} else {
		next := &Definition{ID: id, Name: in.Name, Transport: "streamable_http", Endpoint: in.Endpoint, Enabled: *in.Enabled, Catalog: catalog}
		if current == nil {
			next.ProjectID = *in.ProjectID
			action = "mcp_server.create"
		} else {
			next.ProjectID = current.ProjectID
			next.CredentialID = current.CredentialID
		}
		if len(in.Credential) != 0 {
			next.CredentialID = nil
			if len(material) > 0 {
				key := access.NewID()
				next.CredentialID = &key
				if err = s.Access.Keys.Store(r.Context(), tx, s.Access.Installation, key, secrets.MCPCredential, material, nil); err != nil {
					return access.Reply{}, err
				}
			}
		}
		if err = Save(r.Context(), tx, p.UserID(), current, next); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" {
				return access.Reply{}, access.Fail(409, "mcp_server_exists", "Choose an unused MCP name in this project.")
			}
			return access.Reply{}, err
		}
		id, etag = next.ID, next.ETag
	}
	if err != nil {
		return access.Reply{}, err
	}
	if current != nil && current.CredentialID != nil && (retire || len(in.Credential) != 0) {
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *current.CredentialID, secrets.MCPCredential); err != nil {
			return access.Reply{}, err
		}
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), action, "mcp_server", id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: 204}
	if !retire {
		next, err := Load(r.Context(), tx, id, false)
		if err != nil {
			return access.Reply{}, err
		}
		reply = access.Detail(next, etag)
		if current == nil {
			reply.Status = 201
			reply.Location = "/api/v1/mcp-servers/" + id
		}
	}
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
