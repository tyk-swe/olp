package routing

import (
	"time"

	"github.com/google/uuid"
)

func quotaLimit(value int64) *int64 { return &value }

func quotaSnapshot(provider Provider) *Snapshot {
	return &Snapshot{
		Generation: Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now().UTC()},
		Providers:  map[string]Provider{provider.ID: provider},
		Routes:     map[string]Route{},
	}
}
