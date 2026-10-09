package configuration

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

func loadSAMLDefinition(ctx context.Context, q access.Queryer) (*access.SAMLDefinition, error) {
	var definition access.SAMLDefinition
	err := q.QueryRow(ctx, "SELECT document FROM olp.saml_configuration WHERE singleton").Scan(&definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &definition, nil
}

func (s *Server) applySAMLDefinition(r *http.Request, tx pgx.Tx, p access.Principal, definition *access.SAMLDefinition) error {
	if definition == nil {
		return nil
	}
	current, err := loadSAMLDefinition(r.Context(), tx)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(current, definition) {
		return nil
	}
	_, err = s.Access.StoreSAMLDefinition(r, tx, p, *definition, false)
	return err
}
