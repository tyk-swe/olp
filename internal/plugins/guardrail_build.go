package plugins

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type GuardrailBuild struct {
	Name      string
	Approved  bool
	Confined  bool
	Guardrail bool
}

func FindGuardrailBuild(ctx context.Context, q access.Queryer, digest string) (*GuardrailBuild, error) {
	var b GuardrailBuild
	err := q.QueryRow(ctx, "SELECT manifest->>'name',approved_at IS NOT NULL,module IS NOT NULL,COALESCE((manifest->>'guardrail')::boolean,false) FROM olp.plugins WHERE digest=$1", digest).
		Scan(&b.Name, &b.Approved, &b.Confined, &b.Guardrail)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}
