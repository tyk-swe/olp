package money

import (
	"cmp"
	"slices"
	"testing"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

const (
	BudgetGroup = "0198a8c0-0000-7000-8000-00000000b6d1"
	CostHeader  = "X-Olp-Cost"
	cardID      = "0198a8c0-0000-7000-8000-00000000ca4d"
	priceID     = "0198a8c0-0000-7000-8000-0000000041ce"
	limit       = "1000"
)

var Operations = []string{
	"generation", "embeddings", "moderation", "passthrough", "realtime",
	"image_generation", "image_edit", "image_variation", "speech", "transcription", "translation",
}

type Price struct {
	Cost         *string `json:"cost"`
	CardRevision int     `json:"card_revision"`
	CardMarkup   string  `json:"card_markup"`
	CardPinned   bool    `json:"card_pinned"`
}

func AttemptPrice(operation string, fact *events.AttemptFact) *Price {
	if fact.Price == nil {
		return nil
	}
	price := Price{CardPinned: fact.CardPinned}
	if fact.CardRev != nil {
		price.CardRevision, price.CardMarkup = fact.CardRev.Revision, fact.CardRev.Markup
	}
	if cost, priced := fact.Price.Cost(events.AttemptUsage(fact, operation), fact.StartedAt, events.AttemptSuccessful(fact), fact.Discount); priced && fact.UsageObserved {
		price.Cost = new(cost.String())
	}
	return &price
}

func (l *Ledger) Budget(authority keyauthority.Authority) keyauthority.Authority {
	l.Name(authority.ID, "key")
	budgeted := authority
	budgeted.LookupID = cmp.Or(authority.LookupID, "lookupgolden")
	budgeted.Policy.ResponseMetadata = true
	budgeted.Policy.DailyCostLimit = new(limit)
	budgeted.BudgetGroupID = new(BudgetGroup)
	budgeted.BudgetGroupDailyCostLimit = new(limit)
	budgeted.BudgetGroupCostBasis = string(limits.BasisCharge)
	return budgeted
}

func (l *Ledger) Cap(snapshot *routing.Snapshot, slug string) {
	route := snapshot.Routes[slug]
	route.Budget = &routing.CostLimits{DailyCostLimit: new(limit)}
	snapshot.Routes[slug] = route
	l.Name(route.ID, "route")
	for id, provider := range snapshot.Providers {
		capped := routing.Limits{}
		if provider.Limits != nil {
			capped = *provider.Limits
		}
		capped.DailyCostLimit = new(limit)
		provider.Limits = &capped
		snapshot.Providers[id] = provider
		l.Name(provider.ID, "provider "+provider.Name)
	}
}

func Inputs(t testing.TB, snapshot *routing.Snapshot, operations ...string) *routinginputs.Inputs {
	t.Helper()
	effective := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	prices := []rates.RoutingPrice{}
	for _, provider := range snapshot.Providers {
		models := []string{}
		for _, capability := range provider.Capabilities {
			if !slices.Contains(models, capability.Model) {
				models = append(models, capability.Model)
			}
		}
		for _, model := range models {
			for _, operation := range operations {
				prices = append(prices, *rates.NewRoutingPrice(rates.RoutingPrice{
					RevisionID: priceID, Revision: 1, EffectiveAt: effective,
					Price: rates.Price{
						ProviderKind: string(provider.Kind), Model: model, Operation: operation,
						InputPerMillion: new("2000"), OutputPerMillion: new("8000"),
						UnitPrice: new("0.04"), PerRequest: new("0.01"), StoragePerGiBDay: new("0.10"), Currency: "USD",
					},
				}))
			}
		}
	}
	revision, err := rates.NewRoutingCardRevision(rates.RoutingCardRevision{ID: cardID, Revision: 3, Markup: "1.5", EffectiveAt: effective})
	if err != nil {
		t.Fatal(err)
	}
	return &routinginputs.Inputs{
		RefreshedAt: time.Now(), Prices: prices,
		RateCards: []routinginputs.RoutingRateCard{{ID: cardID, Revisions: []rates.RoutingCardRevision{*revision}}},
	}
}
