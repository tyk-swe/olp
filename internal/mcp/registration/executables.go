package registration

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/routing"
)

type Executables struct {
	tier  *plugins.Unconfined
	query access.Queryer
}

func NewExecutables(tier *plugins.Unconfined, query access.Queryer) *Executables {
	return &Executables{tier: tier, query: query}
}

type executablePermission struct {
	plugins.ExecutableFile
	Permitted bool `json:"permitted"`
}

type executablePermitInput struct {
	Digest          string `json:"digest"`
	AcknowledgeRisk bool   `json:"acknowledge_risk"`
}

func (e *Executables) available() error {
	if e == nil || e.tier == nil {
		return access.Fail(http.StatusNotFound, "unconfined_plugins_disabled", "This deployment does not enable unconfined executables.")
	}
	return nil
}

func (e *Executables) permitted(ctx context.Context, name, digest string) (bool, error) {
	var permitted bool
	err := e.query.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.mcp_executable_permissions WHERE executable=$1 AND digest=$2)", name, digest).Scan(&permitted)
	return permitted, err
}

func (e *Executables) describe(ctx context.Context, name string) (executablePermission, error) {
	if err := e.available(); err != nil {
		return executablePermission{}, err
	}
	file, err := e.tier.Describe(name)
	if err != nil {
		return executablePermission{}, access.Fail(http.StatusNotFound, "mcp_executable_unavailable", "The installed executable is unavailable.")
	}
	permitted, err := e.permitted(ctx, file.Name, file.Digest)
	return executablePermission{ExecutableFile: file, Permitted: permitted}, err
}

func (s *Server) registerExecutables(mux *http.ServeMux) {
	s.access.Route(mux, "GET /api/v1/mcp-executables", s.listExecutables)
	s.access.Route(mux, "POST /api/v1/mcp-executables/{executable}/review", s.reviewExecutable)
	s.access.Route(mux, "POST /api/v1/mcp-executables/{executable}/permit", s.permitExecutable)
	s.access.Route(mux, "DELETE /api/v1/mcp-executables/{executable}/permissions/{digest}", s.revokeExecutable)
}

func (s *Server) listExecutables(r *http.Request, _ access.Principal) (access.Reply, error) {
	if err := s.executables.available(); err != nil {
		return access.Reply{}, err
	}
	files, err := s.executables.tier.Executables()
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(struct {
		Items []plugins.ExecutableFile `json:"items"`
	}{files}), nil
}

func (s *Server) reviewExecutable(r *http.Request, _ access.Principal) (access.Reply, error) {
	file, err := s.executables.describe(r.Context(), r.PathValue("executable"))
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(file), nil
}

func (s *Server) permitExecutable(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input executablePermitInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if !plugins.ValidDigest(input.Digest) {
		return access.Reply{}, access.Invalid("digest", "Use the reviewed executable digest.")
	}
	if !input.AcknowledgeRisk {
		return access.Reply{}, access.Invalid("acknowledge_risk", "Acknowledge that this executable runs with the process's native privileges outside confinement.")
	}
	file, err := s.executables.describe(r.Context(), r.PathValue("executable"))
	if err != nil {
		return access.Reply{}, err
	}
	if file.Digest != input.Digest {
		return access.Reply{}, access.Fail(http.StatusConflict, "executable_changed", "The executable changed. Review the installed build again.")
	}
	tx, principal, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err := s.access.ConsumeRecent(r, tx, principal, "plugin_permit", ""); err != nil {
		return access.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp.mcp_executable_permissions(executable,digest,approved_by) VALUES($1,$2,$3) ON CONFLICT(executable,digest) DO NOTHING", file.Name, file.Digest, principal.ID); err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, principal.Actor(), "mcp_executable.permit", "mcp_executable", file.Digest, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusCreated, Body: executablePermission{ExecutableFile: file.ExecutableFile, Permitted: true}}, tx.Commit(r.Context())
}

func (s *Server) revokeExecutable(r *http.Request, _ access.Principal) (access.Reply, error) {
	tx, principal, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), "DELETE FROM olp.mcp_executable_permissions WHERE executable=$1 AND digest=$2", r.PathValue("executable"), r.PathValue("digest")); err != nil {
		return access.Reply{}, err
	}
	if err := access.Audit(r.Context(), tx, r, principal.Actor(), "mcp_executable.revoke", "mcp_executable", r.PathValue("digest"), "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: http.StatusNoContent}, tx.Commit(r.Context())
}

func (e *Executables) Start(ctx context.Context, binding *routing.MCPStdioBinding, timeout time.Duration) (*plugins.NativeProcess, error) {
	if e.available() != nil || !binding.Valid() {
		return nil, upstream.ErrTransport
	}
	permitted, err := e.permitted(ctx, binding.Executable, binding.Digest)
	if err != nil || !permitted {
		return nil, upstream.ErrTransport
	}
	return e.tier.StartNative(ctx, binding.Executable, binding.Digest, timeout)
}
