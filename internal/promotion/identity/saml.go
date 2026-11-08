package identity

import (
	"context"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

func parseSAML(fragment nullable.Nullable[contract.ConfigurationSAML]) (*access.SAMLDefinition, error) {
	declared, ok := promotion.Present(fragment)
	if !ok {
		return nil, nil
	}
	definition, err := promotion.Convert[access.SAMLDefinition](declared)
	if err != nil {
		return nil, err
	}
	if err := access.ValidateSAMLDefinition(definition); err != nil {
		return nil, err
	}
	return &definition, nil
}

func readSAML(ctx context.Context, q access.Queryer) (*access.SAMLDefinition, error) {
	definition, ok, err := access.ReadSAMLDefinition(ctx, q)
	if !ok {
		return nil, err
	}
	return &definition, nil
}

func renderSAML(definition *access.SAMLDefinition) (nullable.Nullable[contract.ConfigurationSAML], error) {
	if definition == nil {
		return nullable.Nullable[contract.ConfigurationSAML]{}, nil
	}
	rendered, err := promotion.Convert[contract.ConfigurationSAML](*definition)
	if err != nil {
		return nullable.Nullable[contract.ConfigurationSAML]{}, err
	}
	return nullable.NewNullableWithValue(rendered), nil
}

func planSAML(ctx context.Context, q access.Queryer, desired *access.SAMLDefinition) ([]promotion.Item, error) {
	if desired == nil {
		return nil, nil
	}
	current, exists, err := access.ReadSAMLDefinition(ctx, q)
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
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		if desired.saml == nil {
			return nil
		}
		current, exists, err := access.ReadSAMLDefinition(ctx, tx)
		if err != nil || exists && sameSAML(current, *desired.saml) {
			return err
		}
		stored, err := deps.Access.StoreSAMLDefinition(ctx, tx, p, *desired.saml, false)
		if err != nil {
			return err
		}
		return access.Audit(ctx, tx, request, p.Actor(), "saml.configuration.update", "saml_configuration", stored.ID, "success")
	}
}

func sameSAML(a, b access.SAMLDefinition) bool {
	return reflect.DeepEqual(canonicalSAML(a), canonicalSAML(b))
}

func canonicalSAML(d access.SAMLDefinition) access.SAMLDefinition {
	return access.SAMLDefinition{
		Enabled:         d.Enabled,
		Metadata:        d.Metadata,
		EmailAttribute:  d.EmailAttribute,
		GroupsAttribute: d.GroupsAttribute,
		NameAttribute:   d.NameAttribute,
		DefaultRole:     d.DefaultRole,
		EmailMappings:   promotion.NonNil(d.EmailMappings),
		GroupMappings:   promotion.NonNil(d.GroupMappings),
	}
}
