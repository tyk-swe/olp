package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/runtime"
)

// The reservation transaction already holds the provider row lock. Revisions
// are immutable, so the source documents checked here remain the INSERT's
// authority. Limits are the existing explicitly compatible live adjustment.
func compatibleReservationConfiguration(ctx context.Context, q Querier, generationID, providerID string) (bool, error) {
	var pinned, current []byte
	err := q.QueryRow(ctx, `SELECT pinned.configuration, current.configuration
		FROM olp.runtime_releases release
		JOIN olp.providers provider ON provider.id=$2::uuid
		JOIN olp.provider_revisions pinned
		  ON pinned.id=(release.snapshot#>>ARRAY['providers',$2::text,'revision_id'])::uuid
		 AND pinned.provider_id=provider.id
		JOIN olp.provider_revisions current ON current.id=provider.active_revision_id
		WHERE release.id=$1::uuid`, generationID, providerID).Scan(&pinned, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	left, err := reservationConfiguration(pinned)
	if err != nil {
		return false, err
	}
	right, err := reservationConfiguration(current)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func reservationConfiguration(source []byte) ([]byte, error) {
	var config runtime.Configuration
	if err := json.Unmarshal(source, &config); err != nil {
		return nil, err
	}
	config.Options.Limits = nil
	return json.Marshal(config)
}
