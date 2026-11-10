package budget

import "github.com/tyk-swe/olp/internal/access"

type Input struct {
	ProjectID     string  `json:"project_id"`
	RouteID       *string `json:"route_id"`
	APIKeyID      *string `json:"api_key_id"`
	DailyTokens   *int64  `json:"daily_tokens"`
	MonthlyTokens *int64  `json:"monthly_tokens"`
	Enabled       bool    `json:"enabled"`
}

type Draft struct {
	projectID     string
	routeID       *string
	apiKeyID      *string
	dailyTokens   *int64
	monthlyTokens *int64
	enabled       bool
}

func ParseDraft(in Input) (Draft, error) {
	project, err := access.ParseUUID(in.ProjectID)
	if err != nil {
		return Draft{}, err
	}
	route, err := access.ParseOptionalUUID(in.RouteID)
	if err != nil {
		return Draft{}, err
	}
	key, err := access.ParseOptionalUUID(in.APIKeyID)
	if err != nil {
		return Draft{}, err
	}
	if in.DailyTokens == nil && in.MonthlyTokens == nil {
		return Draft{}, access.Invalid("tokens", "Set a daily or monthly token cap.")
	}
	for _, n := range []*int64{in.DailyTokens, in.MonthlyTokens} {
		if n != nil && (*n < 1 || *n > 1<<53-1) {
			return Draft{}, access.Invalid("tokens", "Use a positive safe integer.")
		}
	}
	return Draft{
		projectID:     project,
		routeID:       route,
		apiKeyID:      key,
		dailyTokens:   in.DailyTokens,
		monthlyTokens: in.MonthlyTokens,
		enabled:       in.Enabled,
	}, nil
}

func (in Input) Validate() error {
	_, err := ParseDraft(in)
	return err
}

func (d Draft) ProjectID() string { return d.projectID }
