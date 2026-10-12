package configuration

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

// decimalEqual compares two decimal texts numerically: "1" equals "1.00".
func decimalEqual(a, b string) bool {
	x, okX := new(big.Rat).SetString(strings.TrimSpace(a))
	y, okY := new(big.Rat).SetString(strings.TrimSpace(b))
	return okX && okY && x.Cmp(y) == 0
}

// neutralMultiplier reports whether a multiplier is numerically one.
func neutralMultiplier(multiplier string) bool {
	return decimalEqual(multiplier, "1")
}

// scopeKey identifies a discount or price scope canonically: kind, provider
// (name for documents, id for stored rows), vendor.
func discountScope(kind string, provider, vendor *string) string {
	return kind + "\x00" + lower(provider) + "\x00" + lower(vendor)
}

// inForceDiscounts reads the newest in-force multiplier per stored scope.
func inForceDiscounts(ctx context.Context, q access.Queryer) (map[string]usage.Discount, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (provider_kind,COALESCE(vendor_id,''),COALESCE(provider_id::text,''))
 id::text,provider_kind,vendor_id,provider_id::text,multiplier::text,effective_at
 FROM olp.pricing_discounts WHERE effective_at<=now()
 ORDER BY provider_kind,COALESCE(vendor_id,''),COALESCE(provider_id::text,''),
 effective_at DESC,created_at DESC,id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]usage.Discount{}
	for rows.Next() {
		var d usage.Discount
		if err = rows.Scan(&d.ID, &d.ProviderKind, &d.VendorID, &d.ProviderID, &d.Multiplier, &d.EffectiveAt); err != nil {
			return nil, err
		}
		out[discountScope(d.ProviderKind, d.ProviderID, d.VendorID)] = d
	}
	return out, rows.Err()
}

// applyDiscounts writes a new effective-now discount for each listed scope
// whose multiplier differs from the one in force — including an explicit
// multiplier-one ending — and leaves scopes the document omits untouched.
func (s *Server) applyDiscounts(ctx context.Context, tx pgx.Tx, p access.Principal,
	doc *Document, providerIDs map[string]string) error {
	inForce, err := inForceDiscounts(ctx, tx)
	if err != nil {
		return err
	}
	desired := map[string]usage.Discount{}
	names := map[string]string{}
	for name, id := range providerIDs {
		names[id] = name
	}
	for i, entry := range doc.Discounts {
		field := "discounts." + strconv.Itoa(i)
		var providerID *string
		if entry.Provider != nil {
			id, ok := providerIDs[strings.ToLower(*entry.Provider)]
			if !ok {
				return access.Invalid(field+".provider", "Discount "+strconv.Itoa(i)+" names a provider neither the document nor this installation declares.")
			}
			providerID = &id
		}
		d := usage.Discount{ProviderKind: strings.TrimSpace(entry.ProviderKind),
			VendorID: entry.VendorID, ProviderID: providerID, Multiplier: entry.Multiplier}
		key := discountScope(d.ProviderKind, d.ProviderID, d.VendorID)
		if _, duplicate := desired[key]; duplicate {
			return access.Invalid(field, "The document scopes this discount twice.")
		}
		desired[key] = d
	}
	scopeNames := make([]string, 0, len(desired))
	for scope := range desired {
		scopeNames = append(scopeNames, scope)
	}
	slices.Sort(scopeNames)
	for _, scope := range scopeNames {
		d := desired[scope]
		current, ok := inForce[scope]
		switch {
		case ok && decimalEqual(current.Multiplier, d.Multiplier):
			continue
		case !ok && neutralMultiplier(d.Multiplier):
			// A multiplier-one discount for a scope that prices at one
			// anyway changes nothing.
			continue
		}
		if _, err := s.storeDiscount(ctx, tx, p, d); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) storeDiscount(ctx context.Context, tx pgx.Tx, p access.Principal, d usage.Discount) (usage.Discount, error) {
	input := usage.DiscountInput{ProviderKind: d.ProviderKind, VendorID: d.VendorID,
		ProviderID: d.ProviderID, Multiplier: &d.Multiplier}
	normalized, err := usage.NormalizeDiscount(ctx, tx, input, s.VendorKind)
	if err != nil {
		return usage.Discount{}, err
	}
	return usage.CreateDiscount(ctx, tx, p.UserID(), normalized)
}

// inForceCards reads each card's id, owner and in-force revision for diffs.
func inForceCards(ctx context.Context, q access.Queryer) (map[string]RateCardEntry, map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT c.id::text,
 (SELECT name FROM olp.organizations WHERE id=c.organization_id),
 (SELECT name FROM olp.projects WHERE id=c.project_id),
 (SELECT r.id::text FROM olp.rate_card_revisions r
  WHERE r.rate_card_id=c.id AND r.effective_at<=now()
  ORDER BY r.effective_at DESC,r.revision DESC LIMIT 1)
 FROM olp.rate_cards c`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	entries := map[string]RateCardEntry{}
	cardIDs := map[string]string{}
	type pending struct{ key, revisionID string }
	var load []pending
	for rows.Next() {
		var cardID string
		var organization, project, revisionID *string
		if err = rows.Scan(&cardID, &organization, &project, &revisionID); err != nil {
			return nil, nil, err
		}
		var entry RateCardEntry
		entry.Owner.Kind = "installation"
		key := "installation"
		if organization != nil {
			entry.Owner.Kind = "organization"
			entry.Owner.Organization = organization
			key = "organization\x00" + strings.ToLower(*organization)
		}
		if project != nil {
			entry.Owner.Kind = "project"
			entry.Owner.Project = project
			key = "project\x00" + strings.ToLower(*project)
		}
		cardIDs[key] = cardID
		if revisionID != nil {
			load = append(load, pending{key, *revisionID})
		}
		entries[key] = entry
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	for _, hold := range load {
		entry := entries[hold.key]
		if err = q.QueryRow(ctx, "SELECT markup::text FROM olp.rate_card_revisions WHERE id=$1::uuid", hold.revisionID).Scan(&entry.Markup); err != nil {
			return nil, nil, err
		}
		prices, err := usage.LoadCardPricesForExport(ctx, q, hold.revisionID)
		if err != nil {
			return nil, nil, err
		}
		for _, price := range prices {
			entry.Prices = append(entry.Prices, priceEntryFromUsage(price))
		}
		entries[hold.key] = entry
	}
	return entries, cardIDs, nil
}

// cardKey is the document- and storage-side owner key.
func cardKey(owner CardOwnerEntry) string {
	switch owner.Kind {
	case "organization":
		return "organization\x00" + lower(owner.Organization)
	case "project":
		return "project\x00" + lower(owner.Project)
	default:
		return "installation"
	}
}

// cardEntryEqual reports whether the desired entry repeats the in-force one.
func cardEntryEqual(desired, current RateCardEntry) bool {
	if !decimalEqual(desired.Markup, current.Markup) {
		return false
	}
	render := func(e RateCardEntry) string {
		copy := e
		copy.Owner.Kind = ""
		copy.Markup = ""
		for i := range copy.Prices {
			price := &copy.Prices[i]
			price.Model = strings.TrimSpace(price.Model)
			price.Currency = strings.ToUpper(strings.TrimSpace(price.Currency))
			if price.VendorID != nil {
				v := strings.TrimSpace(*price.VendorID)
				price.VendorID = &v
			}
			price.Provider = nil
		}
		slices.SortFunc(copy.Prices, func(a, b PriceEntry) int {
			key := func(p *PriceEntry) string {
				return p.ProviderKind + "\x00" + lower(p.VendorID) + "\x00" + p.Model + "\x00" + p.Operation
			}
			return strings.Compare(key(&a), key(&b))
		})
		data, _ := json.Marshal(copy)
		return string(data)
	}
	return render(desired) == render(current)
}

// organizationIDs resolves organization references in card owners.
func organizationIDs(ctx context.Context, q access.Queryer) (map[string]string, error) {
	rows, err := q.Query(ctx, "SELECT id::text,name FROM olp.organizations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = id
	}
	return out, rows.Err()
}

// applyRateCards creates missing cards and publishes a new effective-now
// revision wherever the listed revision differs from the one in force; cards
// the document omits are left untouched.
func (s *Server) applyRateCards(ctx context.Context, tx pgx.Tx, p access.Principal,
	doc *Document, organizationIDs, projectIDs map[string]string) error {
	inForce, cardIDs, err := inForceCards(ctx, tx)
	if err != nil {
		return err
	}
	desired := map[string]RateCardEntry{}
	for i, entry := range doc.RateCards {
		field := "rate_cards." + strconv.Itoa(i)
		if entry.Markup == "" {
			return access.Invalid(field+".markup", "A rate card needs a markup.")
		}
		key := cardKey(entry.Owner)
		if _, duplicate := desired[key]; duplicate {
			return access.Invalid(field+".owner", "The document owns this card twice.")
		}
		desired[key] = entry
	}
	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		entry := desired[key]
		cardID, exists := cardIDs[key]
		if !exists {
			var organizationID, projectID *string
			switch entry.Owner.Kind {
			case "organization":
				id, ok := organizationIDs[strings.ToLower(deref(entry.Owner.Organization))]
				if !ok {
					return access.Invalid("rate_cards.owner.organization", "The card names an organization neither the document nor this installation declares.")
				}
				organizationID = &id
			case "project":
				id, ok := projectIDs[strings.ToLower(deref(entry.Owner.Project))]
				if !ok {
					return access.Invalid("rate_cards.owner.project", "The card names a project neither the document nor this installation declares.")
				}
				projectID = &id
			case "installation":
			default:
				return access.Invalid("rate_cards.owner.kind", "Use installation, organization or project.")
			}
			cardID = access.NewID()
			if _, err = tx.Exec(ctx, `INSERT INTO olp.rate_cards
                (id, organization_id, project_id, created_by) VALUES ($1::uuid,$2::uuid,$3::uuid,$4::uuid)`,
				cardID, organizationID, projectID, p.UserID()); err != nil {
				return fmt.Errorf("store rate card: %w", err)
			}
		}
		if exists && cardEntryEqual(entry, inForce[key]) {
			continue
		}
		currency := ""
		if err = tx.QueryRow(ctx, "SELECT COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton), 'USD')").Scan(&currency); err != nil {
			return err
		}
		prices := make([]usage.Price, 0, len(entry.Prices))
		for i, price := range entry.Prices {
			if price.Provider != nil {
				return access.Invalid("rate_cards."+strconv.Itoa(i)+".prices.provider", "Rate card prices cannot scope a provider.")
			}
			prices = append(prices, price.usagePrice())
		}
		if _, err = usage.CreateCardRevision(ctx, tx, p.UserID(), cardID, entry.Markup,
			time.Now().UTC(), prices, currency, s.VendorKind); err != nil {
			return err
		}
	}
	return nil
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// planDiscounts diffs the document's scopes against those in force: a new or
// changed scope creates a discount; scopes the document omits are untouched.
func (s *Server) planDiscounts(ctx context.Context, q access.Queryer, doc *Document, result *planResult) error {
	inForce, err := inForceDiscounts(ctx, q)
	if err != nil {
		return err
	}
	names, err := providerNameMap(ctx, q)
	if err != nil {
		return err
	}
	desired := map[string]DiscountEntry{}
	for _, entry := range doc.Discounts {
		key := discountScope(strings.TrimSpace(entry.ProviderKind), entry.Provider, entry.VendorID)
		desired[key] = entry
	}
	for _, current := range inForce {
		var provider *string
		if current.ProviderID != nil {
			name := names[*current.ProviderID]
			provider = &name
		}
		key := discountScope(current.ProviderKind, provider, current.VendorID)
		entry, declared := desired[key]
		switch {
		case !declared:
		case decimalEqual(entry.Multiplier, current.Multiplier):
			result.item("discount", key, "noop", "")
		default:
			result.item("discount", key, "create", "")
		}
		delete(desired, key)
	}
	for scope, entry := range desired {
		if neutralMultiplier(entry.Multiplier) {
			result.item("discount", scope, "noop", "")
			continue
		}
		result.item("discount", scope, "create", "")
	}
	return nil
}

// planRateCards diffs each document card's revision against the in-force one:
// a new card is created and a differing revision publishes; cards the
// document omits are untouched.
func (s *Server) planRateCards(ctx context.Context, q access.Queryer, doc *Document, result *planResult) error {
	inForce, _, err := inForceCards(ctx, q)
	if err != nil {
		return err
	}
	desired := map[string]RateCardEntry{}
	for _, entry := range doc.RateCards {
		desired[cardKey(entry.Owner)] = entry
	}
	for key, current := range inForce {
		entry, declared := desired[key]
		switch {
		case !declared:
		case cardEntryEqual(entry, current):
			result.item("rate_card", key, "noop", "")
		default:
			result.item("rate_card", key, "revision", "")
		}
		delete(desired, key)
	}
	for key := range desired {
		result.item("rate_card", key, "create", "")
	}
	return nil
}
