package kernel

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

const (
	goldenBudgetGroup = "0198a8c0-0000-7000-8000-00000000b6d1"
	goldenCardID      = "0198a8c0-0000-7000-8000-00000000ca4d"
	goldenPriceID     = "0198a8c0-0000-7000-8000-0000000041ce"
)

var goldenPricedOperations = []string{
	"generation", "embeddings", "moderation", "passthrough", "realtime",
	"image_generation", "image_edit", "image_variation", "speech", "transcription", "translation",
}

type recordedPrice struct {
	Cost         *string `json:"cost"`
	CardRevision int     `json:"card_revision"`
	CardMarkup   string  `json:"card_markup"`
	CardPinned   bool    `json:"card_pinned"`
}

type recordedHold struct {
	Owner    string   `json:"owner"`
	Basis    string   `json:"basis"`
	Reserved []string `json:"reserved"`
	Settled  []string `json:"settled"`
	Released bool     `json:"released"`
}

type ledgerEntry struct {
	owner, basis, amount string
	settles              bool
}

type moneyLedger struct {
	t       *testing.T
	mu      sync.Mutex
	owners  map[string]string
	entries []ledgerEntry
}

var ledgerKey = regexp.MustCompile(`^test:\{([^}]+)\}:([a-z]+):([a-z]+)$`)

func (l *moneyLedger) Do(_ context.Context, command ...string) (any, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.answer(command)
}

func (l *moneyLedger) Pipeline(_ context.Context, commands ...[]string) ([]any, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	replies := make([]any, len(commands))
	for index, command := range commands {
		reply, err := l.answer(command)
		if err != nil {
			return nil, err
		}
		replies[index] = reply
	}
	return replies, nil
}

func (l *moneyLedger) answer(command []string) (any, error) {
	if command[0] == "EXISTS" || command[0] == "DEL" {
		return int64(0), nil
	}
	if len(command) < 4 || command[0] != "EVALSHA" {
		return l.unknown(command)
	}
	count, err := strconv.Atoi(command[2])
	if err != nil || count < 1 || len(command) < 3+count {
		return l.unknown(command)
	}
	keys, args := command[3:3+count], command[3+count:]
	if count == 1 && strings.HasPrefix(keys[0], "test:provider-cooldown:") {
		return int64(1), nil
	}
	last := ledgerKey.FindStringSubmatch(keys[len(keys)-1])
	if last == nil {
		return l.unknown(command)
	}
	owner, named := l.owners[last[1]]
	if !named {
		return l.unknown(command)
	}
	switch {
	case count == 6 && last[3] == "durable" && len(args) == 7:
		if args[3] != "" {
			l.entries = append(l.entries, ledgerEntry{owner: owner, basis: last[2], amount: args[2]})
		}
		return []any{int64(1), int64(1), "ok", int64(0), int64(1), int64(1)}, nil
	case count == 2 && last[3] == "expiry" && len(args) == 3:
		l.entries = append(l.entries, ledgerEntry{owner: owner, basis: last[2], amount: args[1], settles: true})
		return []any{int64(1), int64(1), "ok", int64(1), int64(0)}, nil
	}
	return l.unknown(command)
}

func (l *moneyLedger) unknown(command []string) (any, error) {
	l.t.Errorf("money ledger cannot answer %q", command)
	return nil, context.DeadlineExceeded
}

func (l *moneyLedger) holds() []recordedHold {
	l.mu.Lock()
	defer l.mu.Unlock()
	names := make([]string, 0, len(l.entries))
	for _, entry := range l.entries {
		if name := entry.owner + " " + entry.basis; !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	holds := make([]recordedHold, 0, len(names))
	for _, name := range names {
		hold := recordedHold{Reserved: []string{}, Settled: []string{}}
		for _, entry := range l.entries {
			if entry.owner+" "+entry.basis != name {
				continue
			}
			hold.Owner, hold.Basis = entry.owner, entry.basis
			switch {
			case !entry.settles:
				hold.Reserved = append(hold.Reserved, entry.amount)
			case entry.amount == "0":
				hold.Released = true
			default:
				hold.Settled = append(hold.Settled, entry.amount)
			}
		}
		holds = append(holds, hold)
	}
	return holds
}

func moneyAdmission(t *testing.T) (*quota.Admission, *moneyLedger) {
	t.Helper()
	ledger := &moneyLedger{t: t, owners: map[string]string{limits.SimpleUUID(goldenBudgetGroup): "group"}}
	runner, err := scripts.NewRunner(ledger, "test")
	if err != nil {
		t.Fatal(err)
	}
	return quota.NewAdmission(limitadmission.NewDurable(runner, nil), fleet.New(runner), nil, slog.New(slog.DiscardHandler)), ledger
}

func (l *moneyLedger) name(id, owner string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.owners[limits.SimpleUUID(id)] = owner
}

func (h *harness) budget(ledger *moneyLedger) {
	group := goldenBudgetGroup
	ledger.name(h.keyID, "key")
	authority := h.rt.keys[fullKey]
	authority.LookupID = "lookupgolden"
	authority.Policy.ResponseMetadata = true
	authority.Policy.DailyCostLimit = costText("1000")
	authority.BudgetGroupID = &group
	authority.BudgetGroupDailyCostLimit = costText("1000")
	authority.BudgetGroupCostBasis = string(limits.BasisCharge)
	h.rt.keys[fullKey] = authority
}

func (l *moneyLedger) cap(snapshot *routing.Snapshot) {
	route := snapshot.Routes[routeSlug]
	route.Budget = &routing.CostLimits{DailyCostLimit: costText("1000")}
	snapshot.Routes[routeSlug] = route
	l.name(route.ID, "route")
	for id, provider := range snapshot.Providers {
		capped := routing.Limits{}
		if provider.Limits != nil {
			capped = *provider.Limits
		}
		capped.DailyCostLimit = costText("1000")
		provider.Limits = &capped
		snapshot.Providers[id] = provider
		l.name(provider.ID, "provider "+provider.Name)
	}
}

func pricedInputs(t *testing.T, snapshot *routing.Snapshot) *routinginputs.Inputs {
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
			for _, operation := range goldenPricedOperations {
				prices = append(prices, *rates.NewRoutingPrice(rates.RoutingPrice{
					RevisionID: goldenPriceID, Revision: 1, EffectiveAt: effective,
					Price: rates.Price{
						ProviderKind: string(provider.Kind), Model: model, Operation: operation,
						InputPerMillion: costText("2000"), OutputPerMillion: costText("8000"),
						UnitPrice: costText("0.04"), PerRequest: costText("0.01"), Currency: "USD",
					},
				}))
			}
		}
	}
	return &routinginputs.Inputs{
		RefreshedAt: time.Now(), Prices: prices,
		RateCards: []routinginputs.RoutingRateCard{{ID: goldenCardID, Revisions: []rates.RoutingCardRevision{
			*loadedCardRevision(t, rates.RoutingCardRevision{ID: goldenCardID, Revision: 3, Markup: "1.5", EffectiveAt: effective}),
		}}},
	}
}

func attemptPrice(operation string, fact *events.AttemptFact) *recordedPrice {
	if fact.Price == nil {
		return nil
	}
	price := recordedPrice{CardPinned: fact.CardPinned}
	if fact.CardRev != nil {
		price.CardRevision, price.CardMarkup = fact.CardRev.Revision, fact.CardRev.Markup
	}
	if cost, priced := fact.Price.Cost(events.AttemptUsage(fact, operation), fact.StartedAt, events.AttemptSuccessful(fact), fact.Discount); priced && fact.UsageObserved {
		price.Cost = new(cost.String())
	}
	return &price
}
