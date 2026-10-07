package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/vendors"
)

// A route template turns the models a provider activation certifies into
// ordinary routes. Each generated route is a normal draft, revision and
// release, so nothing reaches a caller without certification and nothing a
// template does escapes route review, simulation or access control.

// Template outcomes for one certified model.
const (
	TemplateDraft     = "draft"
	TemplatePublished = "published"
	TemplateSkipped   = "skipped"
)

var templateName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// TemplateInput declares a route template.
type TemplateInput struct {
	Name             string          `json:"name"`
	ProviderSelector string          `json:"provider_selector"`
	ModelFilter      string          `json:"model_filter"`
	SlugPattern      string          `json:"slug_pattern"`
	OverallTimeoutMS int             `json:"overall_timeout_ms"`
	MaxAttempts      int             `json:"max_attempts"`
	Fidelity         json.RawMessage `json:"fidelity,omitempty"`
	RoutingPolicy    *runtime.Policy `json:"routing_policy,omitempty"`
	AutoPublish      bool            `json:"auto_publish"`
	ProjectID        *string         `json:"project_id,omitempty"`
}

// ValidateTemplateInput checks a template and normalizes its fidelity. It is
// shared by the API and configuration promotion.
func ValidateTemplateInput(in *TemplateInput) error {
	if !templateName.MatchString(in.Name) {
		return access.Invalid("name", "Use 1–63 lowercase letters, digits or hyphens, starting with a letter or digit.")
	}
	if !runtime.ValidSelector(in.ProviderSelector) {
		return access.Invalid("provider_selector", "Use vendor:<catalog-id> or provider:<uuid>.")
	}
	if in.ModelFilter == "" || len(in.ModelFilter) > 200 || strings.ContainsFunc(in.ModelFilter, func(r rune) bool { return r < ' ' }) {
		return access.Invalid("model_filter", "Use 1–200 printable characters; * matches any run and ? one character.")
	}
	if !validSlugPattern(in.SlugPattern) {
		return access.Invalid("slug_pattern", "Include {model}, optionally {vendor}, and otherwise only route slug characters.")
	}
	if in.OverallTimeoutMS < 1 || in.OverallTimeoutMS > maxTimeoutMS {
		return access.Invalid("overall_timeout_ms", "Use a timeout from 1 to 3600000 milliseconds.")
	}
	if in.MaxAttempts < 1 || in.MaxAttempts > maxAttempts {
		return access.Invalid("max_attempts", "Use an attempt budget from 1 to 32767.")
	}
	fidelity, err := normalizedFidelity(in.Fidelity)
	if err != nil {
		return err
	}
	in.Fidelity = fidelity
	if in.RoutingPolicy != nil {
		if err = in.RoutingPolicy.Validate(); err != nil {
			return err
		}
		if samePolicy(in.RoutingPolicy, nil) {
			in.RoutingPolicy = nil
		}
	}
	return nil
}

var slugPlaceholders = strings.NewReplacer("{model}", "m", "{vendor}", "v")

func validSlugPattern(pattern string) bool {
	if !strings.Contains(pattern, "{model}") || len(pattern) > 100 {
		return false
	}
	return runtime.RouteSlug.MatchString(slugPlaceholders.Replace(pattern))
}

// expandSlug fills a slug pattern from a canonical model identity and vendor.
func expandSlug(pattern, identity, vendor string) (string, bool) {
	slug := strings.NewReplacer("{model}", slugPart(identity), "{vendor}", slugPart(vendor)).Replace(pattern)
	return slug, runtime.RouteSlug.MatchString(slug)
}

// slugPart lowercases s and turns every character a slug cannot carry into a
// hyphen, so openai/gpt-4o becomes openai-gpt-4o.
func slugPart(s string) string {
	part := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(s))
	return strings.Trim(part, "-._")
}

// modelGlob compiles a case-insensitive filter where * matches any run of
// characters and ? exactly one.
func modelGlob(filter string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?i)^")
	for _, r := range filter {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

type template struct {
	TemplateInput
	ID        string
	ETag      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const templateColumns = "id::text,name,provider_selector,model_filter,slug_pattern,overall_timeout_ms,max_attempts,fidelity,routing_policy,auto_publish,project_id::text,etag::text,created_at,updated_at"

func scanTemplate(row pgx.Row) (*template, error) {
	t := &template{}
	var policy []byte
	err := row.Scan(&t.ID, &t.Name, &t.ProviderSelector, &t.ModelFilter, &t.SlugPattern, &t.OverallTimeoutMS, &t.MaxAttempts, &t.Fidelity, &policy, &t.AutoPublish, &t.ProjectID, &t.ETag, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(policy) > 0 {
		t.RoutingPolicy = &runtime.Policy{}
		err = json.Unmarshal(policy, t.RoutingPolicy)
	}
	return t, err
}

func loadTemplate(ctx context.Context, q access.Queryer, id string, lock bool) (*template, error) {
	query := "SELECT " + templateColumns + " FROM olp.route_templates WHERE id=$1"
	if lock {
		query += " FOR UPDATE"
	}
	t, err := scanTemplate(q.QueryRow(ctx, query, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, access.Fail(404, "not_found", "Route template not found.")
	}
	return t, err
}

func loadTemplates(ctx context.Context, q access.Queryer) ([]*template, error) {
	rows, err := q.Query(ctx, "SELECT "+templateColumns+" FROM olp.route_templates ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (t *template) json() map[string]any {
	return map[string]any{
		"id": t.ID, "name": t.Name, "provider_selector": t.ProviderSelector, "model_filter": t.ModelFilter,
		"slug_pattern": t.SlugPattern, "overall_timeout_ms": t.OverallTimeoutMS, "max_attempts": t.MaxAttempts,
		"fidelity": json.RawMessage(t.Fidelity), "routing_policy": t.RoutingPolicy, "auto_publish": t.AutoPublish,
		"project_id": t.ProjectID, "etag": t.ETag, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
	}
}

// TemplateResult is what applying a template did with one certified model.
type TemplateResult struct {
	TemplateID      string    `json:"template_id"`
	ProviderID      string    `json:"provider_id"`
	ProviderModelID string    `json:"provider_model_id"`
	UpstreamModel   string    `json:"upstream_model"`
	RouteSlug       string    `json:"route_slug"`
	DraftID         *string   `json:"draft_id"`
	Outcome         string    `json:"outcome"`
	Reason          *string   `json:"reason"`
	CreatedAt       time.Time `json:"created_at,omitzero"`
}

func (r TemplateResult) skip(reason string) TemplateResult {
	r.Outcome, r.Reason = TemplateSkipped, &reason
	return r
}

// certifiedModel is a model an active provider revision publishes with every
// capability certified.
type certifiedModel struct {
	id, providerID, upstream, identity, vendor string
	project                                    *string
	operations                                 []string
}

func certifiedModels(ctx context.Context, q access.Queryer, providerID string) ([]certifiedModel, error) {
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
		var models []runtime.RevisionModel
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
		slices.SortFunc(models, func(a, b runtime.RevisionModel) int { return strings.Compare(a.UpstreamModel, b.UpstreamModel) })
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
func certifiedOperations(m runtime.RevisionModel) []string {
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
			if !runtime.SelectorMatches(t.ProviderSelector, m.providerID, m.vendor) || !match.MatchString(m.identity) || !sameProject(m.project, t.ProjectID) {
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
		result, err = s.join(ctx, tx, draftID, m, result)
	}
	if err != nil || result.Outcome == TemplateSkipped {
		return result, err
	}
	if t.AutoPublish {
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
	targets, err := ValidateDraftInput(ctx, tx, &input, t.ProjectID, nil)
	if refusal, ok := refusalCode(err); ok {
		return result.skip(refusal), nil
	}
	if err != nil {
		return result, err
	}
	id, etag := access.NewID(), access.NewID()
	operations, _ := json.Marshal(input.Operations)
	encoded, _ := json.Marshal(targets)
	if _, err = tx.Exec(ctx, "INSERT INTO olp.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,content_policy,etag,created_by,project_id,fidelity,behavior) VALUES($1,$2,'draft',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)", id, input.Slug, operations, input.OverallTimeoutMS, input.MaxAttempts, encoded, input.ContentPolicy, etag, actor, input.ProjectID, input.Fidelity, input.Behavior); err != nil {
		return result, err
	}
	if t.RoutingPolicy != nil {
		policy, _ := json.Marshal(t.RoutingPolicy)
		if _, err = tx.Exec(ctx, "INSERT INTO olp.routing_policies(scope,scope_id,policy,etag,updated_by) VALUES('route-draft',$1,$2,$3,$4)", id, policy, access.NewID(), actor); err != nil {
			return result, err
		}
	}
	result.DraftID, result.Outcome = &id, TemplateDraft
	return result, nil
}

func (s *Server) join(ctx context.Context, tx pgx.Tx, draftID string, m certifiedModel, result TemplateResult) (TemplateResult, error) {
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
	// The new target is validated as a one-target draft of the same shape,
	// which applies every target rule without revisiting the others.
	input := DraftInput{
		Slug: d.Slug, Operations: d.Operations, OverallTimeoutMS: d.OverallTimeoutMS, MaxAttempts: d.MaxAttempts,
		Targets: []TargetInput{{ProviderModelID: &m.id, Weight: 1, TimeoutMS: d.OverallTimeoutMS}}, Fidelity: d.Fidelity,
	}
	added, err := ValidateDraftInput(ctx, tx, &input, d.ProjectID, nil)
	if refusal, ok := refusalCode(err); ok {
		return result.skip(refusal), nil
	}
	if err != nil {
		return result, err
	}
	added[0].Position = len(d.Targets)
	encoded, _ := json.Marshal(append(d.Targets, added[0]))
	if _, err = tx.Exec(ctx, "UPDATE olp.route_drafts SET targets=$2,state='draft',etag=$3,updated_at=now() WHERE id=$1", d.ID, encoded, access.NewID()); err != nil {
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
	var problem *access.Problem
	if !errors.As(err, &problem) {
		return "", false
	}
	return problem.Code, true
}

func (s *Server) templates(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+templateColumns+" FROM olp.route_templates WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, t.json())
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReply(items, page), nil
}

func (s *Server) template(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "template_id")
	if err != nil {
		return access.Reply{}, err
	}
	t, err := loadTemplate(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(t.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return s.templateDetail(r.Context(), s.Access.Pool, t)
}

func (s *Server) templateDetail(ctx context.Context, q access.Queryer, t *template) (access.Reply, error) {
	rows, err := q.Query(ctx, "SELECT provider_id::text,provider_model_id::text,upstream_model,route_slug,draft_id::text,outcome,created_at FROM olp.route_template_routes WHERE template_id=$1 ORDER BY created_at,route_slug,upstream_model", t.ID)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	routes := []TemplateResult{}
	for rows.Next() {
		g := TemplateResult{TemplateID: t.ID}
		if err = rows.Scan(&g.ProviderID, &g.ProviderModelID, &g.UpstreamModel, &g.RouteSlug, &g.DraftID, &g.Outcome, &g.CreatedAt); err != nil {
			return access.Reply{}, err
		}
		routes = append(routes, g)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	body := t.json()
	body["routes"] = routes
	return access.Detail(body, t.ETag), nil
}

func (s *Server) createTemplate(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	var input TemplateInput
	if err := access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if err = a.RequireProject(r.Context(), tx, p, input.ProjectID); err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	if err = ValidateTemplateInput(&input); err != nil {
		return access.Reply{}, err
	}
	id, etag := access.NewID(), access.NewID()
	if err = WriteTemplate(r.Context(), tx, id, etag, p.UserID(), &input, false); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "route_template.create", "route_template", id, "success"); err != nil {
		return access.Reply{}, err
	}
	created, err := loadTemplate(r.Context(), tx, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	result, err := s.templateDetail(r.Context(), tx, created)
	if err != nil {
		return access.Reply{}, err
	}
	result.Status, result.Location = 201, "/api/v1/route-templates/"+id
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

// WriteTemplate inserts or replaces a validated template; a duplicate name is
// a conflict.
func WriteTemplate(ctx context.Context, tx pgx.Tx, id, etag, actor string, in *TemplateInput, replace bool) error {
	var policy []byte
	if in.RoutingPolicy != nil {
		policy, _ = json.Marshal(in.RoutingPolicy)
	}
	var taken bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.route_templates WHERE name=$1 AND id<>$2)", in.Name, id).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return access.Fail(409, "route_template_name_taken", "Another route template is named "+in.Name+".")
	}
	query := "INSERT INTO olp.route_templates(id,name,provider_selector,model_filter,slug_pattern,overall_timeout_ms,max_attempts,fidelity,routing_policy,auto_publish,project_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)"
	args := []any{id, in.Name, in.ProviderSelector, in.ModelFilter, in.SlugPattern, in.OverallTimeoutMS, in.MaxAttempts, in.Fidelity, policy, in.AutoPublish, in.ProjectID, etag, actor}
	if replace {
		query = "UPDATE olp.route_templates SET name=$2,provider_selector=$3,model_filter=$4,slug_pattern=$5,overall_timeout_ms=$6,max_attempts=$7,fidelity=$8,routing_policy=$9,auto_publish=$10,etag=$11,updated_at=now() WHERE id=$1"
		args = []any{id, in.Name, in.ProviderSelector, in.ModelFilter, in.SlugPattern, in.OverallTimeoutMS, in.MaxAttempts, in.Fidelity, policy, in.AutoPublish, etag}
	}
	_, err := tx.Exec(ctx, query, args...)
	return err
}

func (s *Server) replaceTemplate(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "template_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input TemplateInput
	if err = access.DecodeUnique(r, &input, 1<<20); err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, err := loadTemplate(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if input.ProjectID != nil && !sameProject(input.ProjectID, current.ProjectID) {
		return access.Reply{}, access.Invalid("project_id", "The template's project is set at creation and cannot change.")
	}
	if err = ValidateTemplateInput(&input); err != nil {
		return access.Reply{}, err
	}
	if err = WriteTemplate(r.Context(), tx, id, access.NewID(), p.UserID(), &input, true); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "route_template.update", "route_template", id, "success"); err != nil {
		return access.Reply{}, err
	}
	updated, err := loadTemplate(r.Context(), tx, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	result, err := s.templateDetail(r.Context(), tx, updated)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) deleteTemplate(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "template_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, err := loadTemplate(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, current.ETag); err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.route_templates WHERE id=$1", id); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "route_template.delete", "route_template", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: 204})
}

// applyTemplate applies one template to every active provider, catching up
// models certified before the template existed.
func (s *Server) applyTemplate(r *http.Request, _ access.Principal) (access.Reply, error) {
	a := s.Access
	id, err := access.IDParam(r, "template_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := a.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	t, err := loadTemplate(r.Context(), tx, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(t.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, p, nil)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	results, promoted, err := s.instantiate(r.Context(), tx, []*template{t}, "", p.UserID())
	if err != nil {
		return access.Reply{}, err
	}
	var generation *runtime.Published
	if promoted {
		published, err := runtime.Publish(r.Context(), tx, p.UserID())
		if err != nil {
			return access.Reply{}, err
		}
		generation = &published
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "route_template.apply", "route_template", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 200, Body: map[string]any{"results": results, "runtime_generation": generation}}
	if err = a.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}
