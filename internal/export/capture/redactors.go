package capture

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

var errUnbound = errors.New("capture binding unavailable")

type candidate struct {
	position   int64
	revision   int64
	definition guardrails.Revision
}

func pin(ctx context.Context, q access.Queryer, project string, names []string) ([]Redactor, error) {
	if len(names) == 0 {
		return []Redactor{}, nil
	}
	rows, err := q.Query(ctx, pinRedactorsSQL, project, names)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		d := &c.definition
		err := row.Scan(&c.position, &d.Name, &d.ProjectID, &d.GuardrailID, &d.ID, &c.revision, &d.Type, &d.Configuration, &d.TimeoutMS, &d.OnError)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	if !resolvedInOrder(found, len(names)) {
		return nil, access.Invalid("redact", "each name must resolve to one active, non-retired local regex, PII, secrets or keywords detector in the policy project")
	}
	pins := make([]Redactor, len(found))
	definitions := make(map[string]guardrails.Revision, len(found))
	for i, c := range found {
		d := c.definition
		pins[i] = Redactor{Name: d.Name, GuardrailID: d.GuardrailID, RevisionID: d.ID, Revision: c.revision, Type: d.Type}
		definitions[d.ID] = d
	}
	if _, err := compile(project, names, pins, definitions); err != nil {
		return nil, access.Invalid("redact", "capture detector revisions could not be compiled")
	}
	return pins, nil
}

func resolvedInOrder(found []candidate, names int) bool {
	if len(found) != names {
		return false
	}
	for i, c := range found {
		if c.position != int64(i+1) || !c.definition.Type.Redacts() {
			return false
		}
	}
	return true
}

func compile(project string, names []string, pins []Redactor, definitions map[string]guardrails.Revision) (*guardrails.Engine, error) {
	if len(names) != len(pins) {
		return nil, errUnbound
	}
	if len(pins) == 0 {
		return nil, nil
	}
	entries := make([]guardrails.Entry, len(pins))
	pinned := make(map[string]guardrails.Revision, len(pins))
	for i, b := range pins {
		d, ok := definitions[b.RevisionID]
		if !ok || project == "" || d.ProjectID != project || d.GuardrailID != b.GuardrailID || b.Name != names[i] || d.Type != b.Type || !d.Type.Redacts() {
			return nil, errUnbound
		}
		d.OnError = guardrails.FailClosed
		pinned[d.ID] = d
		entries[i] = guardrails.Entry{GuardrailID: d.GuardrailID, RevisionID: d.ID, Phase: guardrails.Input, Action: guardrails.Redact}
	}
	return guardrails.Compile(entries, pinned, nil)
}

func storePins(ctx context.Context, tx pgx.Tx, policyID string, pins []Redactor) error {
	if _, err := tx.Exec(ctx, clearPinsSQL, policyID); err != nil {
		return err
	}
	names := make([]string, len(pins))
	revisions := make([]string, len(pins))
	for i, b := range pins {
		names[i], revisions[i] = b.Name, b.RevisionID
	}
	_, err := tx.Exec(ctx, insertPinsSQL, policyID, names, revisions)
	return err
}

func loadDefinitions(ctx context.Context, q access.Queryer) (map[string]guardrails.Revision, error) {
	rows, err := q.Query(ctx, loadDefinitionsSQL)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (guardrails.Revision, error) {
		var d guardrails.Revision
		err := row.Scan(&d.ID, &d.GuardrailID, &d.ProjectID, &d.Type, &d.Configuration, &d.TimeoutMS, &d.OnError)
		return d, err
	})
	if err != nil {
		return nil, err
	}
	definitions := make(map[string]guardrails.Revision, len(found))
	for _, d := range found {
		definitions[d.ID] = d
	}
	return definitions, nil
}
