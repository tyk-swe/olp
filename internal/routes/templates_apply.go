package routes

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/routing"
)

// certifiedModel is a model an active provider revision publishes with every
// capability certified.
type certifiedModel struct {
	id, providerID, upstream, identity, vendor string
	project                                    *string
	operations                                 []string
}

func certifiedModels(ctx context.Context, q database.Queryer, providerID string) ([]certifiedModel, error) {
	rows, err := q.Query(ctx, `SELECT p.id::text,p.project_id::text,r.models,coalesce((r.configuration->'options'->'models')::jsonb,'{}'::jsonb),
		coalesce(r.configuration->'options'->>'vendor_id',''),coalesce(r.configuration->>'kind',p.kind)
		FROM olp.providers p JOIN olp.provider_revisions r ON r.id=p.active_revision_id
		WHERE p.state='active' AND ($1='' OR p.id::text=$1) ORDER BY p.id`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []certifiedModel
	for rows.Next() {
		var id, vendor, kind string
		var project *string
		var published, metadata []byte
		if err = rows.Scan(&id, &project, &published, &metadata, &vendor, &kind); err != nil {
			return nil, err
		}
		var models []routing.RevisionModel
		var facts map[string]struct {
			CanonicalModel string `json:"canonical_model"`
		}
		if err = json.Unmarshal(published, &models); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(metadata, &facts); err != nil {
			return nil, err
		}
		if vendor == "" {
			vendor = vendors.DefaultFor(kind)
		}
		slices.SortFunc(models, func(a, b routing.RevisionModel) int { return strings.Compare(a.UpstreamModel, b.UpstreamModel) })
		for _, m := range models {
			operations := certifiedOperations(m)
			if operations == nil {
				continue
			}
			out = append(out, certifiedModel{
				id: m.ID, providerID: id, upstream: m.UpstreamModel, vendor: vendor, project: project, operations: operations,
				identity: cmpOr(facts[m.UpstreamModel].CanonicalModel, m.UpstreamModel),
			})
		}
	}
	return out, rows.Err()
}

// certifiedOperations lists the operations of m in route order, or nil when
// any capability is uncertified.
func certifiedOperations(m routing.RevisionModel) []string {
	if len(m.Capabilities) == 0 {
		return nil
	}
	var operations []string
	for _, c := range m.Capabilities {
		if c.Source != "certified" {
			return nil
		}
		operations = append(operations, c.Operation)
	}
	return slices.DeleteFunc(slices.Clone(supportedOperations), func(op string) bool { return !slices.Contains(operations, op) })
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Activated applies every template to the provider activation just recorded
// in tx. The activation publishes the release, so generated routes go live
// with the provider that serves them.
func (s *Server) Activated(ctx context.Context, tx pgx.Tx, providerID, actor string) error {
	templates, err := loadTemplates(ctx, tx)
	if err != nil || len(templates) == 0 {
		return err
	}
	_, _, err = s.instantiate(ctx, tx, templates, providerID, actor)
	return err
}

// instantiate applies templates to the certified models of every active
// provider, or of providerID alone, and reports whether any route revision
// was promoted and so needs a release.
func (s *Server) instantiate(ctx context.Context, tx pgx.Tx, templates []*template, providerID, actor string) ([]TemplateResult, bool, error) {
	models, err := certifiedModels(ctx, tx, providerID)
	if err != nil {
		return nil, false, err
	}
	results := []TemplateResult{}
	promoted := false
	for _, t := range templates {
		match := modelGlob(t.ModelFilter)
		for _, m := range models {
			if !routing.SelectorMatches(t.ProviderSelector, m.providerID, m.vendor) || !match.MatchString(m.identity) || !sameProject(m.project, t.ProjectID) {
				continue
			}
			var placed bool
			if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.route_template_routes WHERE template_id=$1 AND provider_model_id=$2)", t.ID, m.id).Scan(&placed); err != nil {
				return nil, false, err
			}
			if placed {
				continue
			}
			result, err := s.place(ctx, tx, t, m, actor)
			if err != nil {
				return nil, false, err
			}
			results = append(results, result)
			promoted = promoted || result.Outcome == TemplatePublished
		}
	}
	return results, promoted, nil
}

// place puts one certified model on the route its template names: a new
// draft, or another target on the draft the template generated for the same
// slug, so one canonical model served by several connections is one route.
func (s *Server) place(ctx context.Context, tx pgx.Tx, t *template, m certifiedModel, actor string) (TemplateResult, error) {
	result := TemplateResult{TemplateID: t.ID, ProviderID: m.providerID, ProviderModelID: m.id, UpstreamModel: m.upstream}
	slug, ok := expandSlug(t.SlugPattern, m.identity, m.vendor)
	result.RouteSlug = slug
	if !ok {
		return result.skip("slug_invalid"), nil
	}
	var draftID string
	err := tx.QueryRow(ctx, "SELECT draft_id::text FROM olp.route_template_routes WHERE template_id=$1 AND route_slug=$2 AND draft_id IS NOT NULL ORDER BY created_at LIMIT 1", t.ID, slug).Scan(&draftID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		result, err = s.generate(ctx, tx, t, m, result, actor)
	case err == nil:
		result, err = s.join(ctx, tx, draftID, m, result, t.AutoPublish)
	}
	if err != nil || result.Outcome == TemplateSkipped {
		return result, err
	}
	if t.AutoPublish && result.Reason == nil {
		reason, err := s.promoteGenerated(ctx, tx, *result.DraftID, actor)
		if err != nil {
			return result, err
		}
		if reason == "" {
			result.Outcome = TemplatePublished
		} else {
			result.Reason = &reason
		}
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.route_template_routes(template_id,provider_model_id,provider_id,upstream_model,route_slug,draft_id,outcome) VALUES($1,$2,$3,$4,$5,$6,$7)", t.ID, m.id, m.providerID, m.upstream, slug, result.DraftID, result.Outcome)
	return result, err
}

func (s *Server) generate(ctx context.Context, tx pgx.Tx, t *template, m certifiedModel, result TemplateResult, actor string) (TemplateResult, error) {
	var taken bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.routes WHERE slug=$1) OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE slug=$1)", result.RouteSlug).Scan(&taken); err != nil || taken {
		return result.skip("slug_taken"), err
	}
	input := DraftInput{
		Slug: result.RouteSlug, Operations: m.operations, OverallTimeoutMS: t.OverallTimeoutMS, MaxAttempts: t.MaxAttempts,
		Targets: []TargetInput{{ProviderModelID: &m.id, Weight: 1, TimeoutMS: t.OverallTimeoutMS}}, Fidelity: t.Fidelity, ProjectID: t.ProjectID,
	}
	targets, err := ValidateAndNormalizeDraftInput(ctx, tx, &input, t.ProjectID, nil)
	if refusal, ok := refusalCode(err); ok {
		return result.skip(refusal), nil
	}
	if err != nil {
		return result, err
	}
	id, etag := httpkit.NewID(), httpkit.NewID()
	if err = StageDraft(ctx, tx, NewDraft{ID: id, CreatedBy: actor, ProjectID: input.ProjectID}, etag, input.Content(targets, nil)); err != nil {
		return result, err
	}
	if t.RoutingPolicy != nil {
		if err = StageDraftPolicy(ctx, tx, id, httpkit.NewID(), actor, t.RoutingPolicy); err != nil {
			return result, err
		}
	}
	result.DraftID, result.Outcome = &id, TemplateDraft
	return result, nil
}

func (s *Server) join(ctx context.Context, tx pgx.Tx, draftID string, m certifiedModel, result TemplateResult, autoPublish bool) (TemplateResult, error) {
	d, err := loadDraft(ctx, tx, draftID, true)
	if err != nil {
		return result, err
	}
	if slices.ContainsFunc(d.Operations, func(op string) bool { return !slices.Contains(m.operations, op) }) {
		return result.skip("operations_not_certified"), nil
	}
	if len(d.Targets) >= maxTargets {
		return result.skip("targets_full"), nil
	}
	if autoPublish {
		// Compare before adding the target: a matching revision ID also belongs
		// to drafts with pending edits, even after an operator validates them.
		// Without a published baseline, an existing draft needs activation too.
		var unchanged bool
		defaultPolicy, _ := json.Marshal(routing.Policy{})
		if err = tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM olp.route_drafts d
			JOIN olp.route_revisions v ON v.id=d.based_on_revision_id
			LEFT JOIN olp.routing_policies p ON p.scope='route-draft' AND p.scope_id=d.id
			WHERE d.id=$1 AND
			ROW(d.slug,d.operations,d.overall_timeout_ms,d.max_attempts,d.targets,d.content_policy,d.fidelity,d.behavior,COALESCE(p.policy,$2::jsonb))
			IS NOT DISTINCT FROM
			ROW(v.slug,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.content_policy,v.fidelity,v.behavior,COALESCE(v.routing_policy,$2::jsonb))
		)`, d.ID, defaultPolicy).Scan(&unchanged); err != nil {
			return result, err
		}
		if !unchanged {
			reason := "draft_modified"
			result.Reason = &reason
		}
	}
	// The new target is validated as a one-target draft of the same shape,
	// which applies every target rule without revisiting the others.
	input := DraftInput{
		Slug: d.Slug, Operations: d.Operations, OverallTimeoutMS: d.OverallTimeoutMS, MaxAttempts: d.MaxAttempts,
		Targets: []TargetInput{{ProviderModelID: &m.id, Weight: 1, TimeoutMS: d.OverallTimeoutMS}}, Fidelity: d.Fidelity,
	}
	added, err := ValidateAndNormalizeDraftInput(ctx, tx, &input, d.ProjectID, nil)
	if refusal, ok := refusalCode(err); ok {
		return result.skip(refusal), nil
	}
	if err != nil {
		return result, err
	}
	added[0].Position = len(d.Targets)
	encoded, _ := json.Marshal(append(d.Targets, added[0]))
	if _, err = tx.Exec(ctx, "UPDATE olp.route_drafts SET targets=$2,state='draft',etag=$3,updated_at=now() WHERE id=$1", d.ID, encoded, httpkit.NewID()); err != nil {
		return result, err
	}
	result.DraftID, result.Outcome = &d.ID, TemplateDraft
	return result, nil
}

// promoteGenerated promotes a generated draft inside a savepoint, so a draft
// the live configuration refuses stays a draft without failing the activation
// that certified its model. It returns the refusal code, if any.
func (s *Server) promoteGenerated(ctx context.Context, tx pgx.Tx, draftID, actor string) (string, error) {
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer savepoint.Rollback(ctx)
	d, err := loadDraft(ctx, savepoint, draftID, true)
	if err != nil {
		return "", err
	}
	var latest, state string
	err = savepoint.QueryRow(ctx, "SELECT latest_revision_id::text, state FROM olp.routes WHERE slug=$1 FOR UPDATE", d.Slug).Scan(&latest, &state)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err == nil && state == "retired" {
		return "route_retired", nil
	}
	if err == nil && (d.BasedOnRevision == nil || *d.BasedOnRevision != latest) {
		return "draft_stale", nil
	}
	_, err = s.promote(ctx, savepoint, d, actor)
	if refusal, ok := refusalCode(err); ok {
		return refusal, nil
	}
	if err != nil {
		return "", err
	}
	return "", savepoint.Commit(ctx)
}

// refusalCode names the problem a validation refused with.
func refusalCode(err error) (string, bool) {
	var problem *httpkit.Problem
	if !errors.As(err, &problem) {
		return "", false
	}
	return problem.Code, true
}
