package route

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routing"
)

type Template struct {
	Name             string          `json:"name"`
	Project          *string         `json:"project"`
	ProviderSelector string          `json:"provider_selector"`
	ModelFilter      string          `json:"model_filter"`
	SlugPattern      string          `json:"slug_pattern"`
	OverallTimeoutMS int             `json:"overall_timeout_ms"`
	MaxAttempts      int             `json:"max_attempts"`
	Fidelity         json.RawMessage `json:"fidelity"`
	RoutingPolicy    *routing.Policy `json:"routing_policy"`
	AutoPublish      bool            `json:"auto_publish"`
}

func (t Template) input(projectID *string) routes.TemplateInput {
	return routes.TemplateInput{
		Name: t.Name, ProviderSelector: t.ProviderSelector, ModelFilter: t.ModelFilter, SlugPattern: t.SlugPattern,
		OverallTimeoutMS: t.OverallTimeoutMS, MaxAttempts: t.MaxAttempts, Fidelity: t.Fidelity, RoutingPolicy: t.RoutingPolicy,
		AutoPublish: t.AutoPublish, ProjectID: projectID,
	}
}

func templateOf(in routes.TemplateInput, project *string) Template {
	return Template{
		Name: in.Name, Project: project, ProviderSelector: in.ProviderSelector, ModelFilter: in.ModelFilter, SlugPattern: in.SlugPattern,
		OverallTimeoutMS: in.OverallTimeoutMS, MaxAttempts: in.MaxAttempts, Fidelity: in.Fidelity, RoutingPolicy: in.RoutingPolicy,
		AutoPublish: in.AutoPublish,
	}
}

func normalized(t Template) (Template, error) {
	in := t.input(nil)
	if err := routes.ValidateTemplateInput(&in); err != nil {
		return Template{}, err
	}
	return templateOf(in, t.Project), nil
}

func parseTemplates(entries *[]contract.ConfigurationTemplateEntry) ([]Template, error) {
	declared, err := promotion.Convert[[]Template](entries)
	if err != nil {
		return nil, err
	}
	first := promotion.FirstIndex(declared, func(t Template) string { return t.Name })
	return promotion.TryMapIndex(declared, func(i int, t Template) (Template, error) {
		field := "templates." + strconv.Itoa(i)
		if first[t.Name] != i {
			return Template{}, access.Invalid(field+".name", "Template names must be unique.")
		}
		parsed, err := normalized(t)
		if err != nil {
			return Template{}, promotion.WithFieldPrefix(err, field)
		}
		return parsed, nil
	})
}

func readTemplates(ctx context.Context, q access.Queryer, directory project.Directory) ([]Template, error) {
	stored, err := routes.ReadTemplateInputs(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(stored, func(in routes.TemplateInput) Template {
		return templateOf(in, projectName(directory, in.ProjectID))
	}), nil
}

func planTemplate(t Template, current map[string]Template, known func(string) bool) ([]promotion.Item, error) {
	change, err := templateChange(t, current)
	if err != nil {
		return nil, err
	}
	return slices.Concat(unknownTemplateProject(t, known), []promotion.Item{change}), nil
}

func templateChange(t Template, current map[string]Template) (promotion.Item, error) {
	found, ok := current[t.Name]
	if !ok {
		return templateItem(t.Name, contract.ConfigurationPlanItemActionCreate), nil
	}
	if !strings.EqualFold(promotion.ValueOf(found.Project), promotion.ValueOf(t.Project)) {
		return promotion.Conflict(contract.ConfigurationPlanItemKindRouteTemplate, t.Name, "route_template_project_mismatch"), nil
	}
	same, err := sameTemplate(t, found)
	if err != nil {
		return promotion.Item{}, err
	}
	if same {
		return templateItem(t.Name, contract.ConfigurationPlanItemActionNoop), nil
	}
	return templateItem(t.Name, contract.ConfigurationPlanItemActionReplace), nil
}

func unknownTemplateProject(t Template, known func(string) bool) []promotion.Item {
	if t.Project == nil || known(*t.Project) {
		return nil
	}
	return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindRouteTemplate, t.Name, "project_unknown")}
}

func templateItem(name string, action contract.ConfigurationPlanItemAction) promotion.Item {
	return promotion.Item{Kind: contract.ConfigurationPlanItemKindRouteTemplate, Key: name, Action: action}
}

func sameTemplate(desired, current Template) (bool, error) {
	normal, err := normalized(current)
	if err != nil {
		return false, err
	}
	left, err := json.Marshal(templateOf(desired.input(nil), nil))
	if err != nil {
		return false, err
	}
	right, err := json.Marshal(templateOf(normal.input(nil), nil))
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func TemplateApplier(desired Desired) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		return promotion.Every(desired.Templates, func(t Template) error {
			projectID, err := projectIDOf(directory, t.Project)
			if err != nil {
				return err
			}
			stored, found, err := routes.LockTemplateNamed(ctx, tx, t.Name)
			if err != nil {
				return err
			}
			if found && promotion.ValueOf(stored.ProjectID) != promotion.ValueOf(projectID) {
				return access.Fail(409, "route_template_project_mismatch", "Route template "+t.Name+" belongs to a different project.")
			}
			in := t.input(projectID)
			if !found {
				return routes.WriteTemplate(ctx, tx, access.NewID(), access.NewID(), p.UserID(), &in, false)
			}
			return routes.WriteTemplate(ctx, tx, stored.ID, access.NewID(), p.UserID(), &in, true)
		})
	}
}
