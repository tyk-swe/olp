package pricing

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/usage"
)

type Owner interface{ owner() }

type Installation struct{}

type Organization struct{ Name string }

type Project struct{ Name string }

func (Installation) owner() {}
func (Organization) owner() {}
func (Project) owner()      {}

type RateCard struct {
	Owner  Owner
	Markup string
	Prices []usage.Price
}

type storedCard struct {
	id   string
	live bool
	card RateCard
}

func foldedOwner(owner Owner) Owner {
	switch owner := owner.(type) {
	case Organization:
		return Organization{Name: strings.ToLower(owner.Name)}
	case Project:
		return Project{Name: strings.ToLower(owner.Name)}
	default:
		return Installation{}
	}
}

func ownerKey(owner Owner) string {
	switch owner := foldedOwner(owner).(type) {
	case Organization:
		return "organization\x00" + owner.Name
	case Project:
		return "project\x00" + owner.Name
	default:
		return "installation"
	}
}

func parseCards(entries []contract.ConfigurationRateCardEntry) ([]RateCard, error) {
	cards := promotion.Map(entries, parseCard)
	first := promotion.FirstIndex(cards, func(card RateCard) Owner { return foldedOwner(card.Owner) })
	for i, card := range cards {
		if err := validateCard(i, entries[i], card, first[foldedOwner(card.Owner)] != i); err != nil {
			return nil, err
		}
	}
	return cards, nil
}

func parseCard(entry contract.ConfigurationRateCardEntry) RateCard {
	return RateCard{
		Owner:  parseOwner(entry.Owner),
		Markup: strings.TrimSpace(entry.Markup),
		Prices: promotion.Map(promotion.ValueOf(entry.Prices), func(price contract.ConfigurationPriceEntry) usage.Price { return parsePrice(price).Rate }),
	}
}

func parseOwner(owner contract.ConfigurationRateCardOwner) Owner {
	switch owner.Kind {
	case contract.ConfigurationRateCardOwnerKindOrganization:
		return Organization{Name: strings.TrimSpace(promotion.ValueOf(owner.Organization))}
	case contract.ConfigurationRateCardOwnerKindProject:
		return Project{Name: strings.TrimSpace(promotion.ValueOf(owner.Project))}
	default:
		return Installation{}
	}
}

func validateCard(i int, entry contract.ConfigurationRateCardEntry, card RateCard, duplicate bool) error {
	field := "rate_cards." + strconv.Itoa(i)
	if card.Markup == "" || !nonNegative(card.Markup) {
		return access.Invalid(field+".markup", "A rate card needs a markup.")
	}
	if err := validateOwner(field, entry.Owner, card.Owner); err != nil {
		return err
	}
	if duplicate {
		return access.Invalid(field+".owner", "The document owns this card twice.")
	}
	if j := slices.IndexFunc(promotion.ValueOf(entry.Prices), func(price contract.ConfigurationPriceEntry) bool { return promotion.Pointer(price.Provider) != nil }); j >= 0 {
		return access.Invalid(field+".prices."+strconv.Itoa(j)+".provider", "Rate card prices cannot scope a provider.")
	}
	return nil
}

func validateOwner(field string, entry contract.ConfigurationRateCardOwner, owner Owner) error {
	switch {
	case entry.Kind == contract.ConfigurationRateCardOwnerKindInstallation && (entry.Organization != nil || entry.Project != nil):
		return access.Invalid(field+".owner", "The installation card takes no owner name.")
	case entry.Kind == contract.ConfigurationRateCardOwnerKindOrganization && owner == Organization{}:
		return access.Invalid(field+".owner.organization", "Name the organization.")
	case entry.Kind == contract.ConfigurationRateCardOwnerKindProject && owner == Project{}:
		return access.Invalid(field+".owner.project", "Name the project.")
	case !entry.Kind.Valid():
		return access.Invalid("rate_cards.owner.kind", "Use installation, organization or project.")
	default:
		return nil
	}
}

func readCards(ctx context.Context, q access.Queryer) ([]storedCard, error) {
	type row struct {
		id                                      string
		organization, project, revision, markup *string
	}
	rows, err := q.Query(ctx, `SELECT c.id::text,
 (SELECT name FROM olp.organizations WHERE id=c.organization_id),
 (SELECT name FROM olp.projects WHERE id=c.project_id),
 rev.id::text, rev.markup::text
 FROM olp.rate_cards c
 LEFT JOIN LATERAL (
  SELECT r.id, r.markup FROM olp.rate_card_revisions r
  WHERE r.rate_card_id=c.id AND r.effective_at<=now()
  ORDER BY r.effective_at DESC,r.revision DESC LIMIT 1
 ) rev ON true
 ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	cards, collectErr := pgx.CollectRows(rows, func(scanned pgx.CollectableRow) (row, error) {
		var card row
		err := scanned.Scan(&card.id, &card.organization, &card.project, &card.revision, &card.markup)
		return card, err
	})
	if collectErr != nil {
		return nil, collectErr
	}
	prices, priceErr := usage.LoadCardPricesByRevisions(ctx, q, slices.Collect(func(yield func(string) bool) {
		for _, card := range cards {
			if card.revision != nil && !yield(*card.revision) {
				return
			}
		}
	}))
	if priceErr != nil {
		return nil, priceErr
	}
	return promotion.Map(cards, func(card row) storedCard {
		return storedCard{
			id:   card.id,
			live: card.revision != nil,
			card: RateCard{Owner: storedOwner(card.organization, card.project), Markup: promotion.ValueOf(card.markup), Prices: prices[promotion.ValueOf(card.revision)]},
		}
	}), nil
}

func storedOwner(organization, project *string) Owner {
	switch {
	case project != nil:
		return Project{Name: *project}
	case organization != nil:
		return Organization{Name: *organization}
	default:
		return Installation{}
	}
}

func liveCards(stored []storedCard) []RateCard {
	return promotion.Map(promotion.Filter(stored, func(card storedCard) bool { return card.live }), func(card storedCard) RateCard { return card.card })
}

func renderCards(cards []RateCard) []contract.ConfigurationRateCardEntry {
	sorted := slices.SortedFunc(slices.Values(cards), func(a, b RateCard) int { return strings.Compare(ownerKey(a.Owner), ownerKey(b.Owner)) })
	return promotion.Map(sorted, func(card RateCard) contract.ConfigurationRateCardEntry {
		prices := unscoped(card.Prices)
		return contract.ConfigurationRateCardEntry{Owner: renderOwner(card.Owner), Markup: card.Markup, Prices: promotion.Optional(renderPrices(prices))}
	})
}

func renderOwner(owner Owner) contract.ConfigurationRateCardOwner {
	switch owner := owner.(type) {
	case Organization:
		return contract.ConfigurationRateCardOwner{Kind: contract.ConfigurationRateCardOwnerKindOrganization, Organization: &owner.Name}
	case Project:
		return contract.ConfigurationRateCardOwner{Kind: contract.ConfigurationRateCardOwnerKindProject, Project: &owner.Name}
	default:
		return contract.ConfigurationRateCardOwner{Kind: contract.ConfigurationRateCardOwnerKindInstallation}
	}
}

func unscoped(prices []usage.Price) []Price {
	return promotion.Map(prices, func(rate usage.Price) Price { return Price{Rate: rate} })
}

func indexCards(stored []storedCard) map[Owner]storedCard {
	return promotion.Index(stored, func(card storedCard) Owner { return foldedOwner(card.card.Owner) })
}

func cardAction(card RateCard, existing map[Owner]storedCard, currency string) contract.ConfigurationPlanItemAction {
	current, exists := existing[foldedOwner(card.Owner)]
	switch {
	case !exists:
		return contract.ConfigurationPlanItemActionCreate
	case current.live && decimalEqual(card.Markup, current.card.Markup) && len(card.Prices) == len(current.card.Prices) &&
		samePrices(unscoped(card.Prices), unscoped(current.card.Prices), currency):
		return contract.ConfigurationPlanItemActionNoop
	default:
		return contract.ConfigurationPlanItemActionRevision
	}
}

func readCardState(ctx context.Context, q access.Queryer) (map[Owner]storedCard, string, error) {
	stored, err := readCards(ctx, q)
	if err != nil {
		return nil, "", err
	}
	currency, currencyErr := usage.InstallationCurrency(ctx, q)
	return indexCards(stored), currency, currencyErr
}

func planCards(ctx context.Context, q access.Queryer, desired []RateCard) ([]promotion.Item, error) {
	existing, currency, err := readCardState(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(desired, func(card RateCard) promotion.Item {
		return promotion.Item{Kind: contract.ConfigurationPlanItemKindRateCard, Key: ownerKey(card.Owner), Action: cardAction(card, existing, currency)}
	}), nil
}

func applyCards(ctx context.Context, tx pgx.Tx, principal access.Principal, desired []RateCard, at destination, vendorKind VendorKind) error {
	existing, currency, err := readCardState(ctx, tx)
	if err != nil {
		return err
	}
	for _, card := range desired {
		if cardAction(card, existing, currency) == contract.ConfigurationPlanItemActionNoop {
			continue
		}
		if err := principal.Authorize(access.Settings); err != nil {
			return err
		}
		cardID, idErr := cardIDFor(ctx, tx, principal, card.Owner, existing[foldedOwner(card.Owner)].id, at)
		if idErr != nil {
			return idErr
		}
		if _, err := usage.CreateCardRevision(ctx, tx, principal.UserID(), cardID, card.Markup, time.Now().UTC(), card.Prices, currency, vendorKind); err != nil {
			return err
		}
	}
	return nil
}

func cardIDFor(ctx context.Context, tx pgx.Tx, principal access.Principal, owner Owner, existing string, at destination) (string, error) {
	if existing != "" {
		return existing, nil
	}
	switch owner := owner.(type) {
	case Organization:
		id, ok := at.organizations.id(owner.Name)
		if !ok {
			return "", undeclaredOrganization()
		}
		return usage.CreateRateCard(ctx, tx, principal.UserID(), &id, nil)
	case Project:
		id, ok := at.projects.ID(owner.Name)
		if !ok {
			return "", undeclaredProject()
		}
		return usage.CreateRateCard(ctx, tx, principal.UserID(), nil, &id)
	default:
		return usage.CreateRateCard(ctx, tx, principal.UserID(), nil, nil)
	}
}
