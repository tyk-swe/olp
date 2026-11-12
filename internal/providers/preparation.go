package providers

import (
	"context"
	"errors"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

type installedPlugins struct {
	q          access.Queryer
	unconfined *plugins.Unconfined
}

func (p installedPlugins) Profile(ctx context.Context, digest, profileID string) (*connectors.PluginProfile, error) {
	plugin, err := plugins.Profile(ctx, p.q, p.unconfined, digest, profileID)
	if refusal, ok := errors.AsType[*plugins.Error](err); ok {
		return nil, &access.Problem{Status: 422, Code: refusal.Code, Detail: refusal.Message, Field: profileField(refusal.Code)}
	}
	return plugin, err
}

func profileField(code string) string {
	if code == plugins.CodeProfileUnknown {
		return "configuration.profile_id"
	}
	return "configuration.profile_revision"
}

func (s *Catalogue) ConnectionPolicy(q access.Queryer) connection.Policy {
	return connection.Policy{Egress: s.Egress, Plugins: installedPlugins{q, s.Unconfined}}
}
