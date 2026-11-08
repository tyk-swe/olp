package pricing

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/usage"
)

const maxPrices = 10000

type VendorKind func(vendor string) (string, bool)

type Pricing struct {
	EffectiveAt time.Time
	Prices      []Price
}

type Desired struct {
	Pricing   *Pricing
	Discounts []Discount
	RateCards []RateCard
}

type Fragment struct {
	Pricing   nullable.Nullable[contract.ConfigurationPricingEntry]
	Discounts *[]contract.ConfigurationDiscountEntry
	RateCards *[]contract.ConfigurationRateCardEntry
}

func Parse(document contract.ConfigurationDocument) (Desired, error) {
	pricing, err := parsePricing(document.Pricing)
	if err != nil {
		return Desired{}, err
	}
	discounts, discountErr := parseDiscounts(promotion.ValueOf(document.Discounts))
	if discountErr != nil {
		return Desired{}, discountErr
	}
	cards, cardErr := parseCards(promotion.ValueOf(document.RateCards))
	if cardErr != nil {
		return Desired{}, cardErr
	}
	return Desired{Pricing: pricing, Discounts: discounts, RateCards: cards}, nil
}

func Read(ctx context.Context, q access.Queryer) (Desired, error) {
	providers, err := readDirectory(ctx, q, "SELECT id::text,name FROM olp.providers")
	if err != nil {
		return Desired{}, err
	}
	pricing, pricingErr := readPricing(ctx, q, providers)
	if pricingErr != nil {
		return Desired{}, pricingErr
	}
	discounts, discountErr := readDiscounts(ctx, q, providers)
	if discountErr != nil {
		return Desired{}, discountErr
	}
	cards, cardErr := readCards(ctx, q)
	if cardErr != nil {
		return Desired{}, cardErr
	}
	return Desired{Pricing: pricing, Discounts: discounts, RateCards: liveCards(cards)}, nil
}

func Render(state Desired) Fragment {
	return Fragment{
		Pricing:   promotion.Nullable(renderPricing(state.Pricing)),
		Discounts: promotion.Optional(renderDiscounts(state.Discounts)),
		RateCards: promotion.Optional(renderCards(state.RateCards)),
	}
}

func Planner(desired Desired, names promotion.Names) promotion.Planner {
	return promotion.Planner{
		Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
			destination, err := readDestination(ctx, q)
			if err != nil {
				return nil, err
			}
			if err := checkNames(desired, names, destination); err != nil {
				return nil, err
			}
			pricing, pricingErr := planPricing(ctx, q, desired.Pricing, destination.providers, time.Now())
			if pricingErr != nil {
				return nil, pricingErr
			}
			discounts, discountErr := planDiscounts(ctx, q, desired.Discounts, destination.providers)
			if discountErr != nil {
				return nil, discountErr
			}
			cards, cardErr := planCards(ctx, q, desired.RateCards)
			if cardErr != nil {
				return nil, cardErr
			}
			return slices.Concat(pricing, discounts, cards), nil
		},
	}
}

func Applier(desired Desired, vendorKind VendorKind) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, principal access.Principal) error {
		destination, err := readDestination(ctx, tx)
		if err != nil {
			return err
		}
		if err := applyPricing(ctx, tx, principal, desired.Pricing, destination.providers, vendorKind); err != nil {
			return err
		}
		if err := applyDiscounts(ctx, tx, principal, desired.Discounts, destination.providers, vendorKind); err != nil {
			return err
		}
		return applyCards(ctx, tx, principal, desired.RateCards, destination, vendorKind)
	}
}

func parsePricing(value nullable.Nullable[contract.ConfigurationPricingEntry]) (*Pricing, error) {
	entry, ok := promotion.Present(value)
	if !ok {
		return nil, nil
	}
	if len(entry.Prices) > maxPrices {
		return nil, access.Invalid("pricing.prices", "Declare at most "+strconv.Itoa(maxPrices)+" prices.")
	}
	return &Pricing{EffectiveAt: entry.EffectiveAt, Prices: promotion.Map(entry.Prices, parsePrice)}, nil
}

func readPricing(ctx context.Context, q access.Queryer, providers directory) (*Pricing, error) {
	revisions, _, err := usage.ListRevisions(ctx, q, nil, 1)
	if err != nil || len(revisions) == 0 {
		return nil, err
	}
	return &Pricing{
		EffectiveAt: revisions[0].EffectiveAt,
		Prices: promotion.Map(revisions[0].Prices, func(price usage.Price) Price {
			return Price{Provider: providers.nameOf(price.ProviderID), Rate: scoped(price, nil)}
		}),
	}, nil
}

func renderPricing(pricing *Pricing) *contract.ConfigurationPricingEntry {
	if pricing == nil {
		return nil
	}
	return &contract.ConfigurationPricingEntry{
		EffectiveAt: pricing.EffectiveAt.UTC().Truncate(time.Second),
		Prices:      renderPrices(pricing.Prices),
	}
}

func pricingChanged(desired, current *Pricing) bool {
	return current == nil || len(desired.Prices) != len(current.Prices) || !samePrices(desired.Prices, current.Prices, "")
}

func planPricing(ctx context.Context, q access.Queryer, desired *Pricing, providers directory, now time.Time) ([]promotion.Item, error) {
	if desired == nil {
		return nil, nil
	}
	current, err := readPricing(ctx, q, providers)
	if err != nil {
		return nil, err
	}
	switch {
	case !pricingChanged(desired, current):
		return []promotion.Item{{Kind: contract.ConfigurationPlanItemKindPricing, Key: "pricing", Action: contract.ConfigurationPlanItemActionNoop}}, nil
	case desired.EffectiveAt.After(now):
		return []promotion.Item{{Kind: contract.ConfigurationPlanItemKindPricing, Key: "pricing", Action: contract.ConfigurationPlanItemActionCreate, Detail: "revision"}}, nil
	default:
		return []promotion.Item{{Kind: contract.ConfigurationPlanItemKindPricing, Key: "pricing", Action: contract.ConfigurationPlanItemActionCreate, Detail: "effective_at_rebased"}}, nil
	}
}

func applyPricing(ctx context.Context, tx pgx.Tx, principal access.Principal, desired *Pricing, providers directory, vendorKind VendorKind) error {
	if desired == nil {
		return nil
	}
	current, err := readPricing(ctx, tx, providers)
	if err != nil || !pricingChanged(desired, current) {
		return err
	}
	if err := principal.Authorize(access.Settings); err != nil {
		return err
	}
	prices, resolveErr := resolvePrices(desired.Prices, providers)
	if resolveErr != nil {
		return resolveErr
	}
	if _, err := usage.CreateRevision(ctx, tx, principal.UserID(), rebased(desired.EffectiveAt, time.Now()), prices, vendorKind); err != nil {
		return err
	}
	_, publishErr := release.Publish(ctx, tx, principal.UserID())
	return publishErr
}

func resolvePrices(prices []Price, providers directory) ([]usage.Price, error) {
	if i := slices.IndexFunc(prices, func(price Price) bool { return !providers.resolves(price.Provider) }); i >= 0 {
		return nil, undeclaredPrice(i)
	}
	return promotion.Map(prices, func(price Price) usage.Price {
		return scoped(price.Rate, providers.idOf(price.Provider))
	}), nil
}

func rebased(effectiveAt, now time.Time) time.Time {
	if effectiveAt.After(now) {
		return effectiveAt
	}
	return now.UTC()
}

func undeclaredPrice(i int) error {
	return undeclaredProvider("pricing.prices."+strconv.Itoa(i)+".provider", "Price "+strconv.Itoa(i))
}

func undeclaredProvider(field, subject string) error {
	return access.Invalid(field, subject+" names a provider neither the document nor this installation declares.")
}
