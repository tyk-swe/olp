package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
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
	RoutingPolicy    *routing.Policy `json:"routing_policy,omitempty"`
	AutoPublish      bool            `json:"auto_publish"`
	ProjectID        *string         `json:"project_id,omitempty"`
}

// ValidateTemplateInput checks a template and normalizes its fidelity. It is
// shared by the API and configuration promotion.
func ValidateTemplateInput(in *TemplateInput) error {
	if !templateName.MatchString(in.Name) {
		return httpkit.Invalid("name", "Use 1–63 lowercase letters, digits or hyphens, starting with a letter or digit.")
	}
	if !routing.ValidSelector(in.ProviderSelector) {
		return httpkit.Invalid("provider_selector", "Use vendor:<catalog-id> or provider:<uuid>.")
	}
	if in.ModelFilter == "" || len(in.ModelFilter) > 200 || strings.ContainsFunc(in.ModelFilter, func(r rune) bool { return r < ' ' }) {
		return httpkit.Invalid("model_filter", "Use 1–200 printable characters; * matches any run and ? one character.")
	}
	if !validSlugPattern(in.SlugPattern) {
		return httpkit.Invalid("slug_pattern", "Include {model}, optionally {vendor}, and otherwise only route slug characters.")
	}
	if in.OverallTimeoutMS < 1 || in.OverallTimeoutMS > maxTimeoutMS {
		return httpkit.Invalid("overall_timeout_ms", "Use a timeout from 1 to 3600000 milliseconds.")
	}
	if in.MaxAttempts < 1 || in.MaxAttempts > maxAttempts {
		return httpkit.Invalid("max_attempts", "Use an attempt budget from 1 to 32767.")
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
		if SamePolicy(in.RoutingPolicy, nil) {
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
	return routeidentity.ValidSlug(slugPlaceholders.Replace(pattern))
}

// expandSlug fills a slug pattern from a canonical model identity and vendor.
func expandSlug(pattern, identity, vendor string) (string, bool) {
	slug := strings.NewReplacer("{model}", slugPart(identity), "{vendor}", slugPart(vendor)).Replace(pattern)
	return slug, routeidentity.ValidSlug(slug)
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
		t.RoutingPolicy = &routing.Policy{}
		err = json.Unmarshal(policy, t.RoutingPolicy)
	}
	return t, err
}

func loadTemplate(ctx context.Context, q database.Queryer, id string, lock bool) (*template, error) {
	query := "SELECT " + templateColumns + " FROM olp.route_templates WHERE id=$1"
	if lock {
		query += " FOR UPDATE"
	}
	t, err := scanTemplate(q.QueryRow(ctx, query, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpkit.Fail(404, "not_found", "Route template not found.")
	}
	return t, err
}

func loadTemplates(ctx context.Context, q database.Queryer) ([]*template, error) {
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
		"fidelity": t.Fidelity, "routing_policy": t.RoutingPolicy, "auto_publish": t.AutoPublish,
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

func (s *Server) templates(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+templateColumns+" FROM olp.route_templates WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) ORDER BY id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, t.json())
	}
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReply(items, page), nil
}

func (s *Server) template(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "template_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	t, err := loadTemplate(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(t.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return s.templateDetail(r.Context(), s.Access.Pool, t)
}

func (s *Server) templateDetail(ctx context.Context, q database.Queryer, t *template) (httpkit.Reply, error) {
	rows, err := q.Query(ctx, "SELECT provider_id::text,provider_model_id::text,upstream_model,route_slug,draft_id::text,outcome,created_at FROM olp.route_template_routes WHERE template_id=$1 ORDER BY created_at,route_slug,upstream_model", t.ID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	routes := []TemplateResult{}
	for rows.Next() {
		g := TemplateResult{TemplateID: t.ID}
		if err = rows.Scan(&g.ProviderID, &g.ProviderModelID, &g.UpstreamModel, &g.RouteSlug, &g.DraftID, &g.Outcome, &g.CreatedAt); err != nil {
			return httpkit.Reply{}, err
		}
		routes = append(routes, g)
	}
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	body := t.json()
	body["routes"] = routes
	return httpkit.Detail(body, t.ETag), nil
}

func (s *Server) createTemplate(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	var input TemplateInput
	if err := httpkit.DecodeUnique(r, &input, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a, r, input, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		// The stored reply carries what was created, so the caller must reach
		// its project before a replay is served.
		return struct{}{}, a.RequireProject(r.Context(), tx, p, input.ProjectID)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err := ValidateTemplateInput(&input); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		id, etag := httpkit.NewID(), httpkit.NewID()
		if err := WriteTemplate(r.Context(), tx, id, etag, p.UserID(), &input, false); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		created, err := loadTemplate(r.Context(), tx, id, false)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result, err := s.templateDetail(r.Context(), tx, created)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result.Status, result.Location = 201, "/api/v1/route-templates/"+id
		return result, access.Event{Action: "route_template.create", Resource: "route_template", ID: id}, nil
	})
}

// WriteTemplate inserts or replaces a validated template; a duplicate name is
// a conflict.
func WriteTemplate(ctx context.Context, tx pgx.Tx, id, etag, actor string, in *TemplateInput, replace bool) error {
	policy, err := encodePolicy(in.RoutingPolicy)
	if err != nil {
		return err
	}
	var taken bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.route_templates WHERE name=$1 AND id<>$2)", in.Name, id).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return httpkit.Fail(409, "route_template_name_taken", "Another route template is named "+in.Name+".")
	}
	query := "INSERT INTO olp.route_templates(id,name,provider_selector,model_filter,slug_pattern,overall_timeout_ms,max_attempts,fidelity,routing_policy,auto_publish,project_id,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)"
	args := []any{id, in.Name, in.ProviderSelector, in.ModelFilter, in.SlugPattern, in.OverallTimeoutMS, in.MaxAttempts, in.Fidelity, policy, in.AutoPublish, in.ProjectID, etag, actor}
	if replace {
		query = "UPDATE olp.route_templates SET name=$2,provider_selector=$3,model_filter=$4,slug_pattern=$5,overall_timeout_ms=$6,max_attempts=$7,fidelity=$8,routing_policy=$9,auto_publish=$10,etag=$11,updated_at=now() WHERE id=$1"
		args = []any{id, in.Name, in.ProviderSelector, in.ModelFilter, in.SlugPattern, in.OverallTimeoutMS, in.MaxAttempts, in.Fidelity, policy, in.AutoPublish, etag}
	}
	_, err = tx.Exec(ctx, query, args...)
	return err
}

func (s *Server) replaceTemplate(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	id, err := httpkit.IDParam(r, "template_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input TemplateInput
	if err = httpkit.DecodeUnique(r, &input, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(a, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		current, err := loadTemplate(r.Context(), tx, id, true)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := p.Project(current.ProjectID, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if input.ProjectID != nil && !sameProject(input.ProjectID, current.ProjectID) {
			return httpkit.Reply{}, access.Event{}, httpkit.Invalid("project_id", "The template's project is set at creation and cannot change.")
		}
		if err = ValidateTemplateInput(&input); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = WriteTemplate(r.Context(), tx, id, httpkit.NewID(), p.UserID(), &input, true); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		updated, err := loadTemplate(r.Context(), tx, id, false)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result, err := s.templateDetail(r.Context(), tx, updated)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return result, access.Event{Action: "route_template.update", Resource: "route_template", ID: id}, nil
	})
}

func (s *Server) deleteTemplate(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	id, err := httpkit.IDParam(r, "template_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(a, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		current, err := loadTemplate(r.Context(), tx, id, true)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := p.Project(current.ProjectID, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.route_templates WHERE id=$1", id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{Status: 204}, access.Event{Action: "route_template.delete", Resource: "route_template", ID: id}, nil
	})
}

// applyTemplate applies one template to every active provider, catching up
// models certified before the template existed.
func (s *Server) applyTemplate(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	a := s.Access
	id, err := httpkit.IDParam(r, "template_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a, r, nil, func(tx pgx.Tx, p authz.Principal) (*template, error) {
		t, err := loadTemplate(r.Context(), tx, id, true)
		if err != nil {
			return nil, err
		}
		if err := p.Project(t.ProjectID, authz.Change); err != nil {
			return nil, err
		}
		return t, nil
	}, func(tx pgx.Tx, p authz.Principal, t *template) (httpkit.Reply, access.Event, error) {
		results, promoted, err := s.instantiate(r.Context(), tx, []*template{t}, "", p.UserID())
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var generation *release.Published
		if promoted {
			published, err := release.Publish(r.Context(), tx, p.UserID())
			if err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			generation = &published
		}
		result := httpkit.Reply{Status: 200, Body: map[string]any{"results": results, "runtime_generation": generation}}
		return result, access.Event{Action: "route_template.apply", Resource: "route_template", ID: id}, nil
	})
}
