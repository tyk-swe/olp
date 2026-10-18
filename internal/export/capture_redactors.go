package export

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

func captureDetectorEligible(kind string) bool {
	return slices.Contains([]string{"builtin.regex", "builtin.pii", "builtin.secrets", "builtin.keywords"}, kind)
}

// Publication is explicit: creation or a PATCH containing redact, even if the
// names have not changed. All other edits leave the bindings untouched.
func publishCaptureRedactors(ctx context.Context, q access.Queryer, p *CapturePolicy) error {
	p.RedactorRevisions = []CaptureRedactor{}
	if len(p.Redact) == 0 {
		return nil
	}
	project := p.ProjectID
	if project == nil && p.Route != nil {
		if err := q.QueryRow(ctx, captureRouteScopeSQL, *p.Route).Scan(&project); err != nil {
			return err
		}
	}
	if project == nil {
		return access.Invalid("redact", "redactors require a project-scoped policy or route")
	}
	definitions := map[string]guardrails.Revision{}
	for _, name := range p.Redact {
		rows, err := q.Query(ctx, `SELECT g.id::text,v.id::text,v.revision,g.type,v.configuration,v.timeout_ms,v.on_error
   FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.id=g.active_revision_id AND v.guardrail_id=g.id
   JOIN olp.guardrail_revision_activations a ON a.revision_id=v.id
   WHERE g.project_id=$1 AND lower(g.name)=lower($2) AND g.retired_at IS NULL FOR SHARE OF g`, *project, name)
		if err != nil {
			return err
		}
		count := 0
		var d guardrails.Revision
		var revision int64
		for rows.Next() {
			count++
			if err = rows.Scan(&d.GuardrailID, &d.ID, &revision, &d.Type, &d.Configuration, &d.TimeoutMS, &d.OnError); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		if count != 1 || !captureDetectorEligible(d.Type) {
			return access.Invalid("redact", "each name must resolve to one active, non-retired local regex, PII, secrets or keywords detector in the policy project")
		}
		d.ProjectID, d.Name = *project, name
		definitions[d.ID] = d
		p.RedactorRevisions = append(p.RedactorRevisions, CaptureRedactor{Name: name, GuardrailID: d.GuardrailID, RevisionID: d.ID, Revision: revision, Type: d.Type})
	}
	if err := p.compileRedactors(*project, definitions); err != nil {
		return access.Invalid("redact", "capture detector revisions could not be compiled")
	}
	return nil
}

func storeCaptureRedactors(ctx context.Context, tx pgx.Tx, p CapturePolicy) error {
	if _, err := tx.Exec(ctx, `DELETE FROM olp.capture_redactors WHERE policy_id=$1`, p.ID); err != nil {
		return err
	}
	for i, b := range p.RedactorRevisions {
		if _, err := tx.Exec(ctx, `INSERT INTO olp.capture_redactors(policy_id,position,name,revision_id) VALUES($1,$2,$3,$4)`, p.ID, i, b.Name, b.RevisionID); err != nil {
			return err
		}
	}
	return nil
}

func (p *CapturePolicy) compileRedactors(project string, definitions map[string]guardrails.Revision) error {
	p.redactor = nil
	p.redactorInvalid = true
	if len(p.Redact) != len(p.RedactorRevisions) {
		return errors.New("capture binding mismatch")
	}
	var entries []guardrails.Entry
	pinned := map[string]guardrails.Revision{}
	for i, b := range p.RedactorRevisions {
		d, ok := definitions[b.RevisionID]
		if !ok || project == "" || d.ProjectID != project || d.GuardrailID != b.GuardrailID || b.Name != p.Redact[i] || d.Type != b.Type || !captureDetectorEligible(d.Type) {
			return errors.New("capture binding unavailable")
		}
		d.OnError = guardrails.FailClosed
		pinned[d.ID] = d
		entries = append(entries, guardrails.Entry{GuardrailID: d.GuardrailID, RevisionID: d.ID, Phase: guardrails.Input, Action: guardrails.Redact})
	}
	engine, err := guardrails.Compile(entries, pinned, nil)
	if err != nil {
		return err
	}
	p.redactor, p.redactorInvalid, p.redactorProject = engine, false, project
	return nil
}

func loadCaptureRedactors(ctx context.Context, q access.Queryer) (map[string]guardrails.Revision, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT v.id::text,g.id::text,COALESCE(g.project_id::text,''),g.type,v.configuration,v.timeout_ms,v.on_error
 FROM olp.capture_redactors b JOIN olp.guardrail_revisions v ON v.id=b.revision_id JOIN olp.guardrails g ON g.id=v.guardrail_id
 WHERE b.policy_id IN (SELECT p.id FROM olp.capture_policies p JOIN olp.export_sinks s ON s.id=p.sink_id
 WHERE p.enabled AND s.enabled AND s.project_id IS NULL AND s.format IN ('json','jsonl','otlp') ORDER BY p.id LIMIT 1000)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	definitions := map[string]guardrails.Revision{}
	for rows.Next() {
		var d guardrails.Revision
		if err := rows.Scan(&d.ID, &d.GuardrailID, &d.ProjectID, &d.Type, &d.Configuration, &d.TimeoutMS, &d.OnError); err != nil {
			return nil, err
		}
		definitions[d.ID] = d
	}
	return definitions, rows.Err()
}
