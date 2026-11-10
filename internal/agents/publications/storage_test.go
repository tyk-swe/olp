package publications_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents/publications"
	"github.com/tyk-swe/olp/internal/prompts"
)

type publicationTx struct {
	pgx.Tx
	revision string
}

func (publicationTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("UPDATE 1"), nil
}
func (tx publicationTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return revisionRow(tx.revision)
}

type revisionRow string

func (row revisionRow) Scan(dest ...any) error { *dest[0].(*string) = string(row); return nil }

func TestSavingAndPublishingOwnTheirDrafts(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	priorRevision := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	current := agentcontract.Publication{ID: project, ProjectID: project, Kind: "prompt", Name: "greeting", ETag: "original", RevisionID: &priorRevision, Draft: agentcontract.Draft{ProjectID: project, Name: "greeting", Prompt: &prompts.Definition{Template: "Hello {{name}}", Variables: []string{"name"}}}}
	input := agentcontract.Draft{ProjectID: project, Name: "welcome", Prompt: &prompts.Definition{Template: "Welcome {{name}}", Variables: []string{"name"}}}
	tx := publicationTx{revision: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50609"}
	saved, err := publications.SaveDraft(t.Context(), tx, "actor", &current, "prompt", input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != current.ID || saved.Name != input.Name || saved.RevisionID == nil || *saved.RevisionID != priorRevision || saved.ETag == current.ETag {
		t.Fatal("saving a draft changed publication identity or lost the published revision")
	}
	if current.Name != "greeting" || current.ETag != "original" {
		t.Fatal("saving changed the caller's publication")
	}
	published, err := publications.Publish(t.Context(), tx, "actor", saved)
	if err != nil {
		t.Fatal(err)
	}
	if *published.RevisionID != tx.revision || *saved.RevisionID != priorRevision || published.ETag == saved.ETag {
		t.Fatal("publishing did not return the new revision independently")
	}
	published.Draft.Prompt.Variables[0] = "changed"
	if saved.Draft.Prompt.Variables[0] != "name" || input.Prompt.Variables[0] != "name" {
		t.Fatal("published and editable drafts share mutable state")
	}
	input.Prompt.Variables[0] = "different"
	if saved.Draft.Prompt.Variables[0] != "name" {
		t.Fatal("saved draft retained caller-owned input")
	}
}

type retainedRevisionTx struct {
	pgx.Tx
	revision string
	latest   string
}

func (tx *retainedRevisionTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return retainedRevisionRow{revision: tx.revision}
}

func (tx *retainedRevisionTx) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	tx.latest = args[1].(string)
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

type retainedRevisionRow struct{ revision string }

func (row retainedRevisionRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return pgx.ErrNoRows
	}
	*dest[0].(*string) = row.revision
	return nil
}

func TestRetainedRevisionReusePreservesSelectionAndPublishRevalidates(t *testing.T) {
	id := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	retained := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	latest := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50609"
	tool := agentcontract.Tool{ServerID: id, RevisionID: retained, CatalogDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Name: "lookup", Alias: agentcontract.ToolAlias(id, "lookup")}
	publication := agentcontract.Publication{ID: id, ProjectID: id, Kind: "toolset", Name: "tools", ETag: "original", RevisionID: &latest, Draft: agentcontract.Draft{ProjectID: id, Name: "tools", Toolset: &agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}}
	tx := &retainedRevisionTx{revision: retained, latest: latest}
	selected, err := publications.EnsureRetainedRevision(t.Context(), tx, "actor", publication)
	if err != nil {
		t.Fatal(err)
	}
	if selected.RevisionID == nil || *selected.RevisionID != retained || selected.ETag != publication.ETag || tx.latest != latest {
		t.Fatal("selecting retained content changed the publication's current revision or ETag")
	}
	if _, err := publications.Publish(t.Context(), tx, "actor", publication); err == nil {
		t.Fatal("publishing reused a retained revision whose pinned server is unavailable")
	}
	if tx.latest != latest {
		t.Fatal("a rejected publication changed the current revision")
	}
}
