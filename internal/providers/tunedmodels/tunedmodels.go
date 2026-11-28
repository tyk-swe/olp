package tunedmodels

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Model struct {
	ID            uuid.UUID
	UpstreamModel string
	JobID         uuid.UUID
	APIKeyID      string
	ProviderID    string
}

func Record(ctx context.Context, tx pgx.Tx, model Model) error {
	_, err := tx.Exec(ctx, `INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities,discovered_at)
SELECT $1,provider_id,$2,$2,false,'[]',now() FROM olp.provider_resources
WHERE id=$3 AND api_key_id=$4 AND provider_id=$5 AND kind IN ('fine_tuning_job','vertex_tuning_job','fine_tuning_checkpoint')
ON CONFLICT(provider_id,upstream_model) DO NOTHING`, model.ID, model.UpstreamModel, model.JobID, model.APIKeyID, model.ProviderID)
	return err
}
