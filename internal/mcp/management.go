package mcp

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

type Management struct {
	Access *access.Server
}

type ToolCall struct {
	ID                string    `json:"id"`
	DispatchID        string    `json:"dispatch_id"`
	APIKeyID          string    `json:"api_key_id"`
	ProjectID         string    `json:"project_id"`
	ToolsetID         string    `json:"toolset_id"`
	ToolsetRevisionID string    `json:"toolset_revision_id"`
	ServerID          string    `json:"server_id"`
	ServerRevisionID  string    `json:"server_revision_id"`
	ToolAlias         string    `json:"tool_alias"`
	InputDigest       string    `json:"input_digest"`
	ResultDigest      string    `json:"result_digest"`
	Status            string    `json:"status"`
	IsError           bool      `json:"is_error"`
	StartedAt         time.Time `json:"started_at"`
	CompletedAt       time.Time `json:"completed_at"`
	CreatedAt         time.Time `json:"created_at"`
}

const toolCallColumns = "id::text,dispatch_id::text,api_key_id::text,project_id::text,toolset_id::text,toolset_revision_id::text,server_id::text,server_revision_id::text,tool_alias,input_digest,result_digest,status,is_error,started_at,completed_at,created_at"

type toolCallRow interface {
	Scan(dest ...any) error
}

func scanToolCall(row toolCallRow) (ToolCall, error) {
	var call ToolCall
	err := row.Scan(&call.ID, &call.DispatchID, &call.APIKeyID, &call.ProjectID, &call.ToolsetID, &call.ToolsetRevisionID, &call.ServerID, &call.ServerRevisionID, &call.ToolAlias, &call.InputDigest, &call.ResultDigest, &call.Status, &call.IsError, &call.StartedAt, &call.CompletedAt, &call.CreatedAt)
	return call, err
}

func LoadToolCall(ctx context.Context, q access.Queryer, id string) (ToolCall, error) {
	return scanToolCall(q.QueryRow(ctx, "SELECT "+toolCallColumns+" FROM olp.mcp_tool_calls WHERE id=$1", id))
}

func (m *Management) Register(mux *http.ServeMux) {
	m.Access.Route(mux, "GET /api/v1/mcp-tool-calls", m.list)
	m.Access.Route(mux, "GET /api/v1/mcp-tool-calls/{call_id}", m.get)
}

func (m *Management) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	key := r.URL.Query().Get("api_key_id")
	toolset := r.URL.Query().Get("toolset_id")
	for field, value := range map[string]string{"api_key_id": key, "toolset_id": toolset} {
		if value != "" && !canonicalUUID(value) {
			return access.Reply{}, access.Invalid(field, "Use a canonical UUID.")
		}
	}
	rows, err := m.Access.Pool.Query(r.Context(), "SELECT "+toolCallColumns+" FROM olp.mcp_tool_calls WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) AND ($4='' OR api_key_id=$4::uuid) AND ($5='' OR toolset_id=$5::uuid) ORDER BY id DESC LIMIT $6", page.Before, p.AllProjects, p.ProjectIDs(), key, toolset, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []ToolCall{}
	for rows.Next() {
		call, err := scanToolCall(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, call)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(call ToolCall) string { return call.ID }), nil
}

func (m *Management) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "call_id")
	if err != nil {
		return access.Reply{}, err
	}
	call, err := LoadToolCall(r.Context(), m.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&call.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(call, call.ID), nil
}
