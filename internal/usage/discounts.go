package usage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
)

// Discount is an immutable multiplier on provider cost at one scope. A scope
// ends when a newer discount at that scope supersedes it; nothing is updated
// or deleted, so already-settled attempts keep the multiplier they paid.
type Discount struct {
	ID           string    `json:"id"`
	ProviderKind string    `json:"provider_kind"`
	VendorID     *string   `json:"vendor_id"`
	ProviderID   *string   `json:"provider_id"`
	ProviderName *string   `json:"provider_name,omitempty"`
	Multiplier   string    `json:"multiplier"`
	EffectiveAt  time.Time `json:"effective_at"`
	CreatedAt    time.Time `json:"created_at"`
	CreatedBy    string    `json:"created_by"`
}

// DiscountInput is the creation payload: provider_kind is required, scope and
// vendor narrow it, and effective_at defaults to now.
type DiscountInput struct {
	ProviderKind string     `json:"provider_kind"`
	VendorID     *string    `json:"vendor_id"`
	ProviderID   *string    `json:"provider_id"`
	Multiplier   *string    `json:"multiplier"`
	EffectiveAt  *time.Time `json:"effective_at"`
}

const discountColumns = `SELECT d.id::text, d.provider_kind, d.vendor_id, d.provider_id::text,
        p.name, d.multiplier::text, d.effective_at, d.created_at, d.created_by::text
    FROM olp.pricing_discounts d LEFT JOIN olp.providers p ON p.id = d.provider_id`

func scanDiscount(row interface{ Scan(...any) error }) (Discount, error) {
	var d Discount
	err := row.Scan(&d.ID, &d.ProviderKind, &d.VendorID, &d.ProviderID,
		&d.ProviderName, &d.Multiplier, &d.EffectiveAt, &d.CreatedAt, &d.CreatedBy)
	return d, err
}

// NormalizeDiscount applies the same scoping rules prices do: a known
// connector kind, a vendor that matches it, a provider of the declared kind,
// and plugin providers priced only at their own scope.
func NormalizeDiscount(ctx context.Context, tx pgx.Tx, input DiscountInput,
	vendorKind func(vendor string) (string, bool)) (Discount, error) {
	var d Discount
	d.ProviderKind = strings.TrimSpace(input.ProviderKind)
	if !validProviderKind(d.ProviderKind) {
		return d, access.Invalid("provider_kind", "Use a supported provider kind.")
	}
	if input.VendorID != nil {
		if vendorKind == nil {
			return d, errors.New("pricing vendor catalogue is unavailable")
		}
		vendor := strings.TrimSpace(*input.VendorID)
		kind, known := vendorKind(vendor)
		if !known || kind != d.ProviderKind {
			return d, access.Invalid("vendor_id", "The discount vendor must match the connector.")
		}
		d.VendorID = &vendor
	}
	if input.ProviderID != nil {
		id, err := access.ParseUUID(*input.ProviderID)
		if err != nil {
			return d, access.Invalid("provider_id", "Use a valid provider identifier.")
		}
		var kind string
		err = tx.QueryRow(ctx, "SELECT kind FROM olp.providers WHERE id=$1", id).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && kind != d.ProviderKind) {
			return d, access.Invalid("provider_id",
				"A discount must reference a provider of the declared kind.")
		}
		if err != nil {
			return d, fmt.Errorf("read discount provider: %w", err)
		}
		d.ProviderID = &id
	} else if d.ProviderKind == "plugin" {
		return d, access.Invalid("provider_id",
			"Scope a plugin provider's discount to that provider.")
	}
	if input.Multiplier == nil {
		return d, access.Invalid("multiplier", "A discount needs a multiplier.")
	}
	multiplier, err := normalizeDecimal(*input.Multiplier)
	if err != nil {
		return d, access.Invalid("multiplier", "Use a non-negative decimal.")
	}
	d.Multiplier = multiplier
	d.EffectiveAt = time.Now().UTC()
	if input.EffectiveAt != nil {
		d.EffectiveAt = input.EffectiveAt.UTC()
		if d.EffectiveAt.Before(time.Now()) {
			return d, access.Invalid("effective_at",
				"Choose an effective time now or in the future.")
		}
	}
	return d, nil
}

// CreateDiscount stores a validated discount inside the caller's transaction.
func CreateDiscount(ctx context.Context, tx pgx.Tx, actor string,
	d Discount) (Discount, error) {
	d.ID = access.NewID()
	if _, err := tx.Exec(ctx, `INSERT INTO olp.pricing_discounts
            (id, provider_kind, vendor_id, provider_id, multiplier, effective_at, created_by)
         VALUES ($1::uuid, $2, $3, $4::uuid, $5::text::numeric, $6, $7::uuid)`,
		d.ID, d.ProviderKind, d.VendorID, d.ProviderID, d.Multiplier, d.EffectiveAt, actor); err != nil {
		return Discount{}, fmt.Errorf("store discount: %w", err)
	}
	row, err := scanDiscount(tx.QueryRow(ctx, discountColumns+" WHERE d.id=$1::uuid", d.ID))
	if err != nil {
		return Discount{}, fmt.Errorf("store discount: %w", err)
	}
	return row, nil
}

func (s *Server) listPricingDiscounts(r *http.Request, _ access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(),
		discountColumns+" WHERE d.id<$1::uuid ORDER BY d.id DESC LIMIT $2",
		page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []Discount{}
	for rows.Next() {
		d, err := scanDiscount(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, d)
	}
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(d Discount) string { return d.ID }), nil
}

func (s *Server) createPricingDiscount(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input DiscountInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	principal, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	discount, err := NormalizeDiscount(r.Context(), tx, input, s.VendorKind)
	if err != nil {
		return access.Reply{}, err
	}
	discount, err = CreateDiscount(r.Context(), tx, principal.UserID(), discount)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusCreated,
		Location: "/api/v1/pricing/discounts/" + discount.ID, Body: discount}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "pricing_discount.create",
		"pricing_discount", discount.ID, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}
