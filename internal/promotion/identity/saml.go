package identity

import (
	"context"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

func parseSAML(fragment nullable.Nullable[contract.ConfigurationSAML]) error {
	declared, ok := promotion.Present(fragment)
	if !ok {
		return nil
	}
	definition, err := promotion.Convert[access.SAMLDefinition](declared)
	if err != nil {
		return err
	}
	return access.ValidateSAMLDefinition(definition)
}

func readSAML(ctx context.Context, q access.Queryer) (nullable.Nullable[contract.ConfigurationSAML], error) {
	definition, ok, err := access.ReadSAMLDefinition(ctx, q)
	if !ok {
		return nullable.Nullable[contract.ConfigurationSAML]{}, err
	}
	rendered, err := promotion.Convert[contract.ConfigurationSAML](definition)
	if err != nil {
		return nullable.Nullable[contract.ConfigurationSAML]{}, err
	}
	return nullable.NewNullableWithValue(rendered), nil
}

type samlChange struct {
	definition access.SAMLDefinition
	exists     bool
	same       bool
}

func compareSAML(ctx context.Context, q access.Queryer, declared contract.ConfigurationSAML) (samlChange, error) {
	definition, err := promotion.Convert[access.SAMLDefinition](declared)
	if err != nil {
		return samlChange{}, err
	}
	current, exists, err := access.ReadSAMLDefinition(ctx, q)
	if err != nil {
		return samlChange{}, err
	}
	return samlChange{definition: definition, exists: exists, same: exists && sameSAML(current, definition)}, nil
}

func planSAML(ctx context.Context, q access.Queryer, desired nullable.Nullable[contract.ConfigurationSAML]) ([]promotion.Item, error) {
	declared, ok := promotion.Present(desired)
	if !ok {
		return nil, nil
	}
	change, err := compareSAML(ctx, q, declared)
	if err != nil {
		return nil, err
	}
	return []promotion.Item{{
		Kind:   contract.ConfigurationPlanItemKindSaml,
		Key:    "saml",
		Action: action(change.exists, change.same),
		Detail: "Identity trust is portable; signing material and linked identities stay local.",
	}}, nil
}

func SAMLApplier(d Desired, deps Deps) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		declared, ok := promotion.Present(d.saml)
		if !ok {
			return nil
		}
		change, err := compareSAML(ctx, tx, declared)
		if err != nil || change.same {
			return err
		}
		stored, err := deps.Access.StoreSAMLDefinition(ctx, tx, p, change.definition, false)
		if err != nil {
			return err
		}
		return access.Audit(ctx, tx, (&http.Request{}).WithContext(ctx), p.Actor(), "saml.configuration.update", "saml_configuration", stored.ID, "success")
	}
}

func sameSAML(a, b access.SAMLDefinition) bool {
	return a.Enabled == b.Enabled &&
		a.Metadata == b.Metadata &&
		a.EmailAttribute == b.EmailAttribute &&
		a.GroupsAttribute == b.GroupsAttribute &&
		a.NameAttribute == b.NameAttribute &&
		samePointer(a.DefaultRole, b.DefaultRole) &&
		slices.Equal(a.EmailMappings, b.EmailMappings) &&
		slices.Equal(a.GroupMappings, b.GroupMappings)
}
