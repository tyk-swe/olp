package resources

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/oif"
)

// DiscoverRetainedModel exposes a tuning result as a disabled candidate on its
// original connection. Discovery grants no certified capability or publication.
func (s *Store) DiscoverRetainedModel(ctx context.Context, parent *Resource, model string) error {
	if parent == nil || parent.UUID == uuid.Nil || (parent.Kind != "fine_tuning_job" && parent.Kind != "vertex_tuning_job" && parent.Kind != "fine_tuning_checkpoint") || model == "" || len(model) > 512 || strings.ContainsAny(model, "\x00\r\n") {
		return ErrContract
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name,enabled,capabilities,discovered_at)
SELECT $1,provider_id,$2,$2,false,'[]',now() FROM olp.provider_resources
WHERE id=$3 AND api_key_id=$4 AND provider_id=$5 AND kind IN ('fine_tuning_job','vertex_tuning_job','fine_tuning_checkpoint')
ON CONFLICT(provider_id,upstream_model) DO NOTHING`, uuid.Must(uuid.NewV7()), model, parent.UUID, parent.APIKeyID, parent.ProviderID)
	return err
}

func (s *Store) DiscoverRetainedModels(ctx context.Context, parent *Resource, source oif.Document) error {
	if parent == nil || (parent.Kind != "fine_tuning_job" && parent.Kind != "vertex_tuning_job" && parent.Kind != "fine_tuning_checkpoint") {
		return nil
	}
	for _, pointer := range [][]string{{"fine_tuned_model"}, {"fine_tuned_model_checkpoint"}, {"tunedModel", "model"}} {
		value := source.Root()
		for _, field := range pointer {
			value, _ = value.Lookup(field)
		}
		if model, ok := value.Text(); ok && model != "" {
			if err := s.DiscoverRetainedModel(ctx, parent, strings.TrimSpace(model)); err != nil {
				return err
			}
		}
	}
	return nil
}
