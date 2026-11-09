package policy

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type input struct {
	Name      string             `json:"name"`
	ProjectID string             `json:"project_id,omitempty"`
	Entries   []guardrails.Entry `json:"entries"`
}

type Draft struct {
	project string
	name    string
	entries []guardrails.Entry
}

func NewDraft(ctx context.Context, q access.Queryer, project, name string, entries []guardrails.Entry) (Draft, error) {
	trimmed, err := shape(name, entries)
	if err != nil {
		return Draft{}, err
	}
	if err := pinnable(ctx, q, project, entries); err != nil {
		return Draft{}, err
	}
	return Draft{project: project, name: trimmed, entries: slices.Clone(entries)}, nil
}

func shape(name string, entries []guardrails.Entry) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len(trimmed) > 100 || len(entries) > guardrails.MaxEntries {
		return "", access.Invalid("policy", "Supply a name and at most 128 entries.")
	}
	if slices.ContainsFunc(entries, func(e guardrails.Entry) bool { return uuid.Validate(e.RevisionID) != nil }) {
		return "", unpinnable()
	}
	return trimmed, nil
}

func pinnable(ctx context.Context, q access.Queryer, project string, entries []guardrails.Entry) error {
	ids := revisionIDs(entries)
	definitions, err := guardrails.ReadRevisions(ctx, q, ids)
	if err != nil {
		return err
	}
	retired, err := anyRetired(ctx, q, ids)
	if err != nil {
		return err
	}
	if retired || !owned(definitions, ids, project) {
		return unpinnable()
	}
	if _, err := guardrails.Compile(entries, definitions); err != nil {
		return access.Invalid("entries", "Supply valid phases, actions and pinned revisions.")
	}
	return nil
}

func revisionIDs(entries []guardrails.Entry) []string {
	return slices.Compact(slices.Sorted(func(yield func(string) bool) {
		for _, e := range entries {
			if !yield(e.RevisionID) {
				return
			}
		}
	}))
}

func owned(definitions map[string]guardrails.Revision, ids []string, project string) bool {
	return !slices.ContainsFunc(ids, func(id string) bool {
		d, ok := definitions[id]
		return !ok || d.ProjectID != project
	})
}

func unpinnable() error {
	return access.Invalid("entries", "Choose existing guardrail revisions in this project.")
}
