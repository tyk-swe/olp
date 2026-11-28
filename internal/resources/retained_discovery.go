package resources

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/providers/tunedmodels"
)

func isTuningDiscoveryKind(kind string) bool {
	return kind == "fine_tuning_job" || kind == "vertex_tuning_job" || kind == "fine_tuning_checkpoint"
}

func (s *Store) discoverRetainedModel(ctx context.Context, parent *Resource, model string) error {
	if parent == nil || parent.UUID == uuid.Nil || !isTuningDiscoveryKind(parent.Kind) || model == "" || len(model) > 512 || strings.ContainsAny(model, "\x00\r\n") {
		return ErrContract
	}
	id, err := newUUIDv7()
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		return tunedmodels.Record(ctx, tx, tunedmodels.Model{
			ID: id, UpstreamModel: model, JobID: parent.UUID, APIKeyID: parent.APIKeyID, ProviderID: parent.ProviderID,
		})
	})
}

func (s *Store) DiscoverRetainedModels(ctx context.Context, parent *Resource, source oif.Document) error {
	if parent == nil || !isTuningDiscoveryKind(parent.Kind) {
		return nil
	}
	for _, pointer := range [][]string{{"fine_tuned_model"}, {"fine_tuned_model_checkpoint"}, {"tunedModel", "model"}} {
		value := retainedPath(source.Root(), pointer...)
		if model, ok := value.Text(); ok && model != "" {
			if err := s.discoverRetainedModel(ctx, parent, strings.TrimSpace(model)); err != nil {
				return err
			}
		}
	}
	return nil
}
