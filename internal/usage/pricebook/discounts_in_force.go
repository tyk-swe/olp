package pricebook

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type DiscountInForce struct {
	ProviderKind string
	VendorID     *string
	ProviderID   *string
	Multiplier   string
}

func ReadDiscountsInForce(ctx context.Context, q access.Queryer) ([]DiscountInForce, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (provider_kind,COALESCE(vendor_id,''),COALESCE(provider_id::text,''))
 provider_kind,vendor_id,provider_id::text,multiplier::text
 FROM olp.pricing_discounts WHERE effective_at<=now()
 ORDER BY provider_kind,COALESCE(vendor_id,''),COALESCE(provider_id::text,''),effective_at DESC,created_at DESC,id DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DiscountInForce])
}
