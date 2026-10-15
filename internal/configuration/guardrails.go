package configuration

import (
	"bytes"
	"context"
	"encoding/json"
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
	Configuration json.RawMessage       `json:"configuration,omitempty"`
	TimeoutMS     int64                 `json:"timeout_ms,omitempty"`
	OnError       string                `json:"on_error,omitempty"`
	Name          string                `json:"name"`
	Project       string                `json:"project"`
	Type          string                `json:"type"`
	Policy        *contentpolicy.Policy `json:"policy,omitempty"`
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
		if !guardrailLabel(entry.Name) || (entry.Project != "" && !guardrailLabel(entry.Project)) || seen[key] || (entry.Policy == nil && len(entry.Configuration) == 0) {
			return access.Invalid("guardrails", "Use unique project/name pairs, builtin.regex, and bounded content policies.")
		}
		if len(entry.Configuration) > 0 {
			if entry.Policy != nil {
				return access.Invalid("guardrails", "Do not mix legacy policies and typed configuration.")
			}
			if entry.TimeoutMS == 0 {
				entry.TimeoutMS = 1000
			}
			if entry.OnError == "" {
				entry.OnError = guardrails.FailClosed
			}
			if err := guardrails.ValidateDefinition(guardrails.Revision{Type: entry.Type, Configuration: entry.Configuration, TimeoutMS: entry.TimeoutMS, OnError: entry.OnError}); err != nil {
				return access.Invalid("guardrails", "Invalid guardrail configuration.")
			}
		} else if err := contentpolicy.Validate(entry.Policy); err != nil {
			return access.Invalid("guardrails", "Use valid bounded RE2 block or redact rules.")
		}
		if entry.Policy != nil && entry.Policy.Rules == nil {
			entry.Policy.Rules = []contentpolicy.Rule{}
		}
		seen[key] = true
	}
	return nil
}

func exportGuardrails(ctx context.Context, q access.Queryer) ([]GuardrailEntry, error) {
	rows, err := q.Query(ctx, "SELECT g.name,COALESCE(p.name,''),g.type,v.configuration,v.timeout_ms,v.on_error FROM olp.guardrails g LEFT JOIN olp.projects p ON p.id=g.project_id JOIN olp.guardrail_revisions v ON v.id=g.active_revision_id WHERE g.retired_at IS NULL AND g.managed_route_revision_id IS NULL ORDER BY lower(p.name),lower(g.name)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []GuardrailEntry{}
	for rows.Next() {
		var entry GuardrailEntry
		if err := rows.Scan(&entry.Name, &entry.Project, &entry.Type, &entry.Configuration, &entry.TimeoutMS, &entry.OnError); err != nil {
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
		if entry.Type == "plugin" {
			var config guardrails.ExternalConfig
			_ = json.Unmarshal(entry.Configuration, &config)
			var name string
			var approved, confined, capable bool
			err := q.QueryRow(ctx, "SELECT manifest->>'name',approved_at IS NOT NULL,module IS NOT NULL,COALESCE((manifest->>'guardrail')::boolean,false) FROM olp.plugins WHERE digest=$1", config.PluginVersion).Scan(&name, &approved, &confined, &capable)
			if errors.Is(err, pgx.ErrNoRows) {
				result.blocker("plugin", config.PluginVersion, "plugin_not_installed")
			} else if err != nil {
				return err
			} else if !approved {
				result.blocker("plugin", config.PluginVersion, "plugin_not_approved")
			} else if !confined || !capable || name != config.PluginID {
				result.conflict("plugin", config.PluginVersion, "A matching confined guardrail plugin is required.")
			}
		}
		projectID := state.projects[strings.ToLower(entry.Project)]
		if entry.Project == "" {
			projectID = "installation"
		}
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
			if current.Type != entry.Type {
				result.conflict("guardrail", key, "A definition type is immutable; use a new name.")
				continue
			}
			action = "replace"
			if current.Name == entry.Name && current.Type == entry.Type && guardrailEqual(current, entry) {
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
		if entry.Project != "" {
			if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)", entry.Project).Scan(&projectID); err != nil {
				return err
			}
		}
		var project *string
		if projectID != "" {
			project = &projectID
		}
		if err := p.Project(project, access.Change); err != nil {
			return err
		}
		current, err := guardrails.FindActive(ctx, tx, projectID, entry.Name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if current != nil && current.Name == entry.Name && current.Type == entry.Type && guardrailEqual(current, entry) {
			continue
		}
		next := &guardrails.Definition{ProjectID: projectID, Name: entry.Name, Type: entry.Type, Policy: entry.Policy, Configuration: entry.Configuration, TimeoutMS: entry.TimeoutMS, OnError: entry.OnError}
		if err := guardrails.Save(ctx, tx, p.UserID(), current, next); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO olp.guardrail_revision_activations(revision_id,activated_by) VALUES($1,$2) ON CONFLICT DO NOTHING", next.RevisionID, p.UserID()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE olp.guardrails SET active_revision_id=$2 WHERE id=$1", next.ID, next.RevisionID); err != nil {
			return err
		}
	}
	return nil
}

func guardrailEqual(current *guardrails.Definition, entry GuardrailEntry) bool {
	if len(entry.Configuration) == 0 {
		return reflect.DeepEqual(current.Policy, entry.Policy)
	}
	var a, b any
	_ = json.Unmarshal(current.Configuration, &a)
	_ = json.Unmarshal(entry.Configuration, &b)
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y) && current.TimeoutMS == entry.TimeoutMS && current.OnError == entry.OnError
}
