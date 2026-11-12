package notifications

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

const subscribedRulesSQL = `SELECT r.id::text FROM olp.notification_rules r
 JOIN olp.notification_destinations d ON d.id=r.destination_id WHERE r.event=$1 AND r.enabled AND d.enabled`

const insertGrantLapseDeliverySQL = `INSERT INTO olp.notification_deliveries (id, rule_id, credential_id, payload, status)
 VALUES ($1, $2, $3, $4, 'pending') ON CONFLICT DO NOTHING`

func NotifyGrantLapsed(ctx context.Context, tx pgx.Tx, lapse GrantLapse) error {
	rows, err := tx.Query(ctx, subscribedRulesSQL, contract.NotificationEventProviderGrantLapsed)
	if err != nil {
		return err
	}
	rules, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(rules) == 0 {
		return err
	}
	payload, err := json.Marshal(lapse)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if _, err := tx.Exec(ctx, insertGrantLapseDeliverySQL, access.NewID(), rule, lapse.CredentialID, payload); err != nil {
			return err
		}
	}
	return nil
}

type GrantLapse struct {
	CredentialID      string      `json:"credential_version_id"`
	CredentialVersion int         `json:"credential_version"`
	ObservedPrincipal *string     `json:"observed_principal"`
	ProviderID        string      `json:"provider_id"`
	ProviderName      string      `json:"provider_name"`
	LapsedAt          time.Time   `json:"lapsed_at"`
	CredentialSlots   []GrantSlot `json:"credential_slots"`
}

type GrantSlot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
