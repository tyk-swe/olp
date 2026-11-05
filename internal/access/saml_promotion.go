package access

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"
)

func LoadSAMLDefinition(ctx context.Context, q Queryer) (*SAMLDefinition, error) {
	var definition SAMLDefinition
	err := q.QueryRow(ctx, "SELECT document FROM olp.saml_configuration WHERE singleton").Scan(&definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
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
	_, err = s.StoreSAMLDefinition(r, tx, p, *definition, false)
	return err
}
