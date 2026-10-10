package configuration

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type GuardrailEntry struct {
	Name    string                `json:"name"`
	Project string                `json:"project"`
	Type    string                `json:"type"`
	Policy  *contentpolicy.Policy `json:"policy"`
}

func guardrailLabel(value string) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) >= 1 && utf8.RuneCountInString(value) <= 100 && !strings.ContainsFunc(value, unicode.IsControl)
}

func validateGuardrails(doc *Document) error {
	if len(doc.Guardrails) > 1000 {
		return access.Invalid("guardrails", "Declare at most 1000 guardrails.")
	}
	seen := map[[2]string]bool{}
	for i := range doc.Guardrails {
		entry := &doc.Guardrails[i]
		entry.Name, entry.Project = strings.TrimSpace(entry.Name), strings.TrimSpace(entry.Project)
		key := [2]string{strings.ToLower(entry.Project), strings.ToLower(entry.Name)}
		if !guardrailLabel(entry.Name) || !guardrailLabel(entry.Project) || seen[key] || entry.Type != "builtin.regex" || entry.Policy == nil {
			return access.Invalid("guardrails", "Use unique project/name pairs, builtin.regex, and bounded content policies.")
		}
		if err := contentpolicy.Validate(entry.Policy); err != nil {
			return access.Invalid("guardrails", "Use valid bounded RE2 block or redact rules.")
		}
		if entry.Policy.Rules == nil {
			entry.Policy.Rules = []contentpolicy.Rule{}
		}
		seen[key] = true
	}
	return nil
}

func exportGuardrails(ctx context.Context, q access.Queryer) ([]GuardrailEntry, error) {
	rows, err := q.Query(ctx, "SELECT g.name,p.name,g.type,v.policy FROM olp.guardrails g JOIN olp.projects p ON p.id=g.project_id JOIN olp.guardrail_revisions v ON v.id=g.latest_revision_id WHERE g.retired_at IS NULL ORDER BY lower(p.name),lower(g.name)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []GuardrailEntry{}
	for rows.Next() {
		var entry GuardrailEntry
		if err := rows.Scan(&entry.Name, &entry.Project, &entry.Type, &entry.Policy); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func planGuardrails(ctx context.Context, q access.Queryer, doc *Document, state *stateView, result *planResult) error {
	if len(doc.Guardrails) == 0 {
		return nil
	}
	for _, entry := range doc.Guardrails {
		key := entry.Project + "/" + entry.Name
		projectID := state.projects[strings.ToLower(entry.Project)]
		if projectID == "" {
			declared := false
			for _, project := range doc.Projects {
				if strings.EqualFold(project.Name, entry.Project) {
					declared = true
					break
				}
			}
			if !declared {
				result.conflict("guardrail", key, "The owning project is not declared or present at the destination.")
				continue
			}
			result.item("guardrail", key, "create", "Publish a reusable bounded content-policy definition.")
			continue
		}
		current, err := guardrails.FindActive(ctx, q, projectID, entry.Name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		action := "create"
		if current != nil {
			action = "replace"
			if current.Name == entry.Name && current.Type == entry.Type && reflect.DeepEqual(current.Policy, entry.Policy) {
				action = "reuse"
			}
		}
		result.item("guardrail", key, action, "Published routes retain their copied policies; definition revisions remain local.")
	}
	return nil
}

func applyGuardrails(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document) error {
	for _, entry := range doc.Guardrails {
		var projectID string
		if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)", entry.Project).Scan(&projectID); err != nil {
			return err
		}
		if err := p.Project(&projectID, access.Change); err != nil {
			return err
		}
		current, err := guardrails.FindActive(ctx, tx, projectID, entry.Name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if current != nil && current.Name == entry.Name && current.Type == entry.Type && reflect.DeepEqual(current.Policy, entry.Policy) {
			continue
		}
		next := &guardrails.Definition{ProjectID: projectID, Name: entry.Name, Type: entry.Type, Policy: entry.Policy}
		if err := guardrails.Save(ctx, tx, p.UserID(), current, next); err != nil {
			return err
		}
	}
	return nil
}
