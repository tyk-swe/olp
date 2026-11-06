package access

import (
	"context"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
)

func CheckLimitTemplates(ctx context.Context, q Queryer, project *string, names ...*string) error {
	used := false
	for _, name := range names {
		if name != nil {
			used = true
		}
	}
	if !used {
		return nil
	}
	if project == nil {
		return Invalid("limit_template", "Templates require a project.")
	}
	var templates keyauthority.LimitTemplates
	if err := q.QueryRow(ctx, "SELECT limit_templates FROM olp.projects WHERE id=$1", *project).Scan(&templates); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := templates.Resolve(name, keyauthority.AdmissionLimits{}); err != nil {
			return err
		}
	}
	return nil
}

func endUserTemplate(p *keyauthority.EndUserPolicy) *string {
	if p == nil {
		return nil
	}
	return p.LimitTemplate
}
