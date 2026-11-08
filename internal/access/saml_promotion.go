package access

import (
	"context"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"
)

func LoadSAMLDefinition(ctx context.Context, q Queryer) (*SAMLDefinition, error) {
	definition, ok, err := ReadSAMLDefinition(ctx, q)
	if !ok {
		return nil, err
	}
	return &definition, nil
}

func (s *Server) ApplySAMLDefinition(r *http.Request, tx pgx.Tx, p Principal, definition *SAMLDefinition) error {
	if definition == nil {
		return nil
	}
	current, err := LoadSAMLDefinition(r.Context(), tx)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(current, definition) {
		return nil
	}
	c, err := s.StoreSAMLDefinition(r.Context(), tx, p, *definition, false)
	if err != nil {
		return err
	}
	return Audit(r.Context(), tx, r, p.Actor(), "saml.configuration.update", "saml_configuration", c.ID, "success")
}
