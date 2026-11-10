package account

import (
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
)

type Input struct {
	ProjectID         string   `json:"project_id"`
	ProviderID        string   `json:"provider_id"`
	CredentialID      string   `json:"credential_id"`
	Name              string   `json:"name"`
	Enabled           bool     `json:"enabled"`
	Models            []string `json:"models"`
	ExtraUsageEnabled *bool    `json:"extra_usage_enabled"`
	CycleAnchor       *string  `json:"cycle_anchor,omitempty"`
}

type Draft struct {
	projectID         string
	providerID        string
	credentialID      string
	name              string
	enabled           bool
	models            []string
	extraUsageEnabled *bool
	cycleAnchor       *string
}

func ParseDraft(in Input) (Draft, error) {
	ids, err := access.ParseUUIDs([]string{in.ProjectID, in.ProviderID, in.CredentialID})
	if err != nil {
		return Draft{}, err
	}
	if err := access.ValidText("name", in.Name, 100); err != nil {
		return Draft{}, err
	}
	if err := code.ValidateModels(in.Models); err != nil {
		return Draft{}, access.Invalid("models", err.Error())
	}
	if in.CycleAnchor != nil {
		if _, ok := code.ParseCycleAnchor(*in.CycleAnchor); !ok {
			return Draft{}, access.Invalid("cycle_anchor", "Use a calendar date as the subscription cycle anchor.")
		}
	}
	return Draft{
		projectID:         ids[0],
		providerID:        ids[1],
		credentialID:      ids[2],
		name:              in.Name,
		enabled:           in.Enabled,
		models:            in.Models,
		extraUsageEnabled: in.ExtraUsageEnabled,
		cycleAnchor:       in.CycleAnchor,
	}, nil
}

func (in Input) Validate() error {
	_, err := ParseDraft(in)
	return err
}

func (d Draft) ProjectID() string    { return d.projectID }
func (d Draft) ProviderID() string   { return d.providerID }
func (d Draft) CredentialID() string { return d.credentialID }
