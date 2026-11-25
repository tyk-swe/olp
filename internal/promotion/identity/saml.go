package identity

import (
	"context"
	"net/http"
	"reflect"

	"github.com/tyk-swe/olp/internal/domain/ptr"
	signin "github.com/tyk-swe/olp/internal/identity"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/promotion"
)

func parseSAML(fragment nullable.Nullable[contract.ConfigurationSAML]) (*signin.SAMLDefinition, error) {
	declared, ok := promotion.Present(fragment)
	if !ok {
		return nil, nil
	}
	definition, err := promotion.Convert[signin.SAMLDefinition](declared)
	if err != nil {
		return nil, err
	}
	if err := signin.ValidateSAMLDefinition(definition); err != nil {
		return nil, err
	}
	return &definition, nil
}

func readSAML(ctx context.Context, q database.Queryer) (*signin.SAMLDefinition, error) {
	definition, ok, err := signin.ReadSAMLDefinition(ctx, q)
	if !ok {
		return nil, err
	}
	return &definition, nil
}

func renderSAML(definition *signin.SAMLDefinition) (nullable.Nullable[contract.ConfigurationSAML], error) {
	if definition == nil {
		return nullable.Nullable[contract.ConfigurationSAML]{}, nil
	}
	rendered, err := promotion.Convert[contract.ConfigurationSAML](*definition)
	if err != nil {
		return nullable.Nullable[contract.ConfigurationSAML]{}, err
	}
	return nullable.NewNullableWithValue(rendered), nil
}

func planSAML(ctx context.Context, q database.Queryer, desired *signin.SAMLDefinition) ([]promotion.Item, error) {
	if desired == nil {
		return nil, nil
	}
	current, exists, err := signin.ReadSAMLDefinition(ctx, q)
	if err != nil {
		return nil, err
	}
	return []promotion.Item{{
		Kind:   contract.ConfigurationPlanItemKindSaml,
		Key:    "saml",
		Action: action(exists, exists && sameSAML(current, *desired)),
		Detail: "Identity trust is portable; signing material and linked identities stay local.",
	}}, nil
}

func SAMLApplier(desired Desired, deps Deps, request *http.Request) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p authz.Principal) error {
		if desired.saml == nil {
			return nil
		}
		current, exists, err := signin.ReadSAMLDefinition(ctx, tx)
		if err != nil || exists && sameSAML(current, *desired.saml) {
			return err
		}
		stored, err := deps.SignIn.StoreSAMLDefinition(ctx, tx, p, *desired.saml, false)
		if err != nil {
			return err
		}
		return access.Audit(ctx, tx, request, p.Actor(), "saml.configuration.update", "saml_configuration", stored.ID, "success")
	}
}

func sameSAML(a, b signin.SAMLDefinition) bool {
	return reflect.DeepEqual(canonicalSAML(a), canonicalSAML(b))
}

func canonicalSAML(d signin.SAMLDefinition) signin.SAMLDefinition {
	return signin.SAMLDefinition{
		Enabled:         d.Enabled,
		Metadata:        d.Metadata,
		EmailAttribute:  d.EmailAttribute,
		GroupsAttribute: d.GroupsAttribute,
		NameAttribute:   d.NameAttribute,
		DefaultRole:     d.DefaultRole,
		EmailMappings:   ptr.NonNil(d.EmailMappings),
		GroupMappings:   ptr.NonNil(d.GroupMappings),
	}
}
