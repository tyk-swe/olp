package agents

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type InvestigationFilters struct {
	From          *string `json:"from,omitempty"`
	To            *string `json:"to,omitempty"`
	RunID         *string `json:"run_id,omitempty"`
	MinDurationMS *int64  `json:"min_duration_ms,omitempty"`
	MaxDurationMS *int64  `json:"max_duration_ms,omitempty"`
	Error         *bool   `json:"error,omitempty"`
	Tool          *string `json:"tool,omitempty"`
}

type InvestigationEvidence struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type investigationEvidenceView struct {
	InvestigationEvidence
	RunID string `json:"run_id,omitempty"`
}

type InvestigationInput struct {
	ProjectID string                  `json:"project_id"`
	Filters   *InvestigationFilters   `json:"filters"`
	Evidence  []InvestigationEvidence `json:"evidence"`
	Summary   string                  `json:"summary"`
}

type Investigation struct {
	ID string `json:"id"`
	InvestigationInput
	CreatedAt time.Time `json:"created_at"`
}

func (input InvestigationInput) Validate() error {
	if !investigationUUID(input.ProjectID) || input.Filters == nil || input.Evidence == nil || len(input.Evidence) > 100 || utf8.RuneCountInString(input.Summary) > 4096 || strings.TrimSpace(input.Summary) == "" {
		return access.Invalid("investigation", "Supply filters, up to 100 evidence references, and a summary of at most 4096 characters.")
	}
	if !validInvestigationFilters(*input.Filters) {
		return access.Invalid("filters", "Use valid time, run, duration, error and tool filters.")
	}
	for _, evidence := range input.Evidence {
		if investigationEvidenceQuery(evidence.Kind) == "" || !investigationUUID(evidence.ID) {
			return access.Invalid("evidence", "Use a valid evidence kind and canonical UUID.")
		}
	}
	return nil
}

func investigationUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func investigationTime(value *string) (time.Time, error) {
	if value == nil {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, *value)
}

func validInvestigationFilters(filters InvestigationFilters) bool {
	from, fromErr := investigationTime(filters.From)
	to, toErr := investigationTime(filters.To)
	if fromErr != nil || toErr != nil || filters.From != nil && filters.To != nil && from.After(to) {
		return false
	}
	if filters.RunID != nil && !investigationUUID(*filters.RunID) {
		return false
	}
	if filters.MinDurationMS != nil && *filters.MinDurationMS < 0 || filters.MaxDurationMS != nil && *filters.MaxDurationMS < 0 {
		return false
	}
	if filters.MinDurationMS != nil && filters.MaxDurationMS != nil && *filters.MinDurationMS > *filters.MaxDurationMS {
		return false
	}
	return filters.Tool == nil || strings.TrimSpace(*filters.Tool) != "" && utf8.RuneCountInString(*filters.Tool) <= 256
}

func investigationEvidenceQuery(kind string) string {
	return map[string]string{
		"run":        `SELECT EXISTS(SELECT 1 FROM olp.agent_runs WHERE id=$1 AND project_id=$2)`,
		"step":       `SELECT EXISTS(SELECT 1 FROM olp.agent_steps s JOIN olp.agent_runs r ON r.id=s.run_id WHERE s.id=$1 AND r.project_id=$2)`,
		"approval":   `SELECT EXISTS(SELECT 1 FROM olp.agent_approvals WHERE id=$1 AND project_id=$2)`,
		"span":       `SELECT EXISTS(SELECT 1 FROM olp.trace_spans WHERE id=$1 AND project_id=$2)`,
		"task":       `SELECT EXISTS(SELECT 1 FROM olp.a2a_tasks WHERE id=$1 AND project_id=$2)`,
		"delegation": `SELECT EXISTS(SELECT 1 FROM olp.agent_delegations WHERE id=$1 AND project_id=$2)`,
	}[kind]
}

func validateInvestigationEvidence(ctx context.Context, tx pgx.Tx, project string, evidence InvestigationEvidence) error {
	var exists bool
	if err := tx.QueryRow(ctx, investigationEvidenceQuery(evidence.Kind), evidence.ID, project).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return access.Invalid("evidence", "Evidence must exist in the investigation project.")
	}
	return nil
}

func (s *Server) listInvestigations(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	project := r.URL.Query().Get("project_id")
	if project != "" {
		id, err := uuid.Parse(project)
		if err != nil || id == uuid.Nil || id.String() != project {
			return access.Reply{}, access.Invalid("project_id", "Use a canonical project UUID.")
		}
		if err := p.Project(&project, access.View); err != nil {
			return access.Reply{}, err
		}
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT id::text,project_id::text,filters,evidence,summary,created_at FROM olp.investigations
 WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) AND ($4='' OR project_id=NULLIF($4,'')::uuid)
 ORDER BY id DESC LIMIT $5`, page.Before, p.AllProjects, p.ProjectIDs(), project, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[Investigation])
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item Investigation) string { return item.ID }), nil
}

func (s *Server) getInvestigation(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "investigation_id")
	if err != nil {
		return access.Reply{}, err
	}
	itemRows, err := s.Access.Pool.Query(r.Context(), `SELECT id::text AS id,project_id::text AS project_id,filters,evidence,summary,created_at FROM olp.investigations WHERE id=$1`, id)
	if err != nil {
		return access.Reply{}, err
	}
	item, err := pgx.CollectOneRow(itemRows, pgx.RowToStructByName[Investigation])
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(&item.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT ref.value->>'kind' AS kind,ref.value->>'id' AS id,
 COALESCE(COALESCE(run.id,approval.run_id,span.run_id)::text,'') AS run_id
 FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY AS ref(value,position)
 LEFT JOIN olp.agent_steps step ON ref.value->>'kind'='step' AND step.id=(ref.value->>'id')::uuid
 LEFT JOIN olp.agent_runs run ON run.id=step.run_id AND run.project_id=$2
 LEFT JOIN olp.agent_approvals approval ON ref.value->>'kind'='approval' AND approval.id=(ref.value->>'id')::uuid AND approval.project_id=$2
 LEFT JOIN olp.trace_spans span ON ref.value->>'kind'='span' AND span.id=(ref.value->>'id')::uuid AND span.project_id=$2
 ORDER BY ref.position`, item.Evidence, item.ProjectID)
	if err != nil {
		return access.Reply{}, err
	}
	evidence, err := pgx.CollectRows(rows, pgx.RowToStructByName[investigationEvidenceView])
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(struct {
		Investigation
		Evidence []investigationEvidenceView `json:"evidence"`
	}{Investigation: item, Evidence: evidence}), nil
}

func (s *Server) createInvestigation(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input InvestigationInput
	if err := access.DecodeUnique(r, &input, 32<<10); err != nil {
		return access.Reply{}, err
	}
	if err := input.Validate(); err != nil {
		return access.Reply{}, err
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
	if err := s.Access.RequireProject(r.Context(), tx, p, &input.ProjectID); err != nil {
		return access.Reply{}, err
	}
	claim, replay, err := s.Access.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replay != nil {
		return access.Commit(r, tx, *replay)
	}
	if input.Filters.RunID != nil {
		if err := validateInvestigationEvidence(r.Context(), tx, input.ProjectID, InvestigationEvidence{Kind: "run", ID: *input.Filters.RunID}); err != nil {
			return access.Reply{}, err
		}
	}
	for _, evidence := range input.Evidence {
		if err := validateInvestigationEvidence(r.Context(), tx, input.ProjectID, evidence); err != nil {
			return access.Reply{}, err
		}
	}
	itemRows, err := tx.Query(r.Context(), `INSERT INTO olp.investigations(id,project_id,filters,evidence,summary) VALUES($1,$2,$3,$4,$5)
 RETURNING id::text AS id,project_id::text AS project_id,filters,evidence,summary,created_at`, access.NewID(), input.ProjectID, input.Filters, input.Evidence, input.Summary)
	if err != nil {
		return access.Reply{}, err
	}
	item, err := pgx.CollectOneRow(itemRows, pgx.RowToStructByName[Investigation])
	if err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "investigation.create", "investigation", item.ID, "success", &item.ProjectID); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: http.StatusCreated, Body: item, Location: "/api/v1/investigations/" + item.ID}
	if err := s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
