package debits

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/scripts"
)

const (
	testID  = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	otherID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
)

func intent(kind Kind) Intent {
	return Intent{
		ID: testID, APIKeyID: testID, ProjectID: testID, ClaimID: testID, Kind: kind, Currency: "USD",
		CostBound: "1", ChargeBound: "1", PreparedUntil: time.Now().Add(time.Minute),
		Affinity: Affinity{ReleaseID: testID, RevisionID: testID},
	}
}

func unreachablePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://olp:olp@127.0.0.1:1/olp?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestResolutionEvidenceIdentity(t *testing.T) {
	a, b := make([]byte, 32), make([]byte, 32)
	b[0] = 1
	one, err := resolutionDigest([][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	two, err := resolutionDigest([][]byte{b, a})
	if err != nil || !bytes.Equal(one, two) {
		t.Fatal("resolution identity depends on ordering")
	}
	if _, err = resolutionDigest([][]byte{a, a}); err == nil {
		t.Fatal("duplicate resolution accepted")
	}
	if _, err = resolutionDigest([][]byte{[]byte("runtime body")}); err == nil {
		t.Fatal("content accepted as result digest")
	}
}

func TestIntentCurrencySyntax(t *testing.T) {
	for _, tc := range []struct {
		currency string
		valid    bool
	}{{"ABC", true}, {"", false}, {"AB", false}, {"ABCD", false}, {"abc", false}, {"AB1", false}} {
		i := intent(KindTool)
		i.Currency = tc.currency
		if err := i.validate(); (err == nil) != tc.valid {
			t.Errorf("currency %q, valid %t, error %v", tc.currency, tc.valid, err)
		}
	}
}

func TestIntentAcceptsPublishedToolAliases(t *testing.T) {
	alias := agentcontract.ToolAlias(testID, "lookup")
	for _, tc := range []struct {
		alias string
		valid bool
	}{{alias, true}, {"", true}, {strings.ToUpper(alias), false}, {alias[:len(alias)-1], false}, {"invalid_" + alias, false}} {
		i := intent(KindTool)
		i.Affinity.ToolAlias = tc.alias
		if err := i.validate(); (err == nil) != tc.valid {
			t.Errorf("alias %q, valid %t, error %v", tc.alias, tc.valid, err)
		}
	}
}

func TestIntentKindsAndProjectScope(t *testing.T) {
	global := intent(KindAPI)
	global.ProjectID = ""
	if err := global.validate(); err != nil {
		t.Fatalf("global API namespace refused: %v", err)
	}
	global.Kind = KindTool
	if global.validate() == nil {
		t.Fatal("projectless tool accepted")
	}
	unknown := intent(Kind("batch"))
	if unknown.validate() == nil {
		t.Fatal("unknown debit kind accepted")
	}
}

func TestIntentIdentityAndOwners(t *testing.T) {
	i := intent(KindAPI)
	i.CostBound, i.ChargeBound = "0.125", "0.25"
	if err := i.validate(); err != nil {
		t.Fatal(err)
	}
	owners, err := normalizedOwners([]limits.Owner{{ID: testID, Kind: limits.OwnerKey}})
	if err != nil || len(owners) != 2 || owners[0].Basis != limits.BasisCharge || owners[1].Basis != limits.BasisCost {
		t.Fatalf("both accounting bases must survive policy changes: %+v %v", owners, err)
	}
	digest := func(i Intent) [32]byte {
		sum, err := intentDigest(i, owners)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	original := digest(i)
	i.ClaimID = otherID
	if digest(i) != original {
		t.Fatal("private fence changed logical identity")
	}
	i.CostBound = "0.126"
	if digest(i) == original {
		t.Fatal("bound change reused logical identity")
	}
	for _, bad := range []string{"-1", "1e3", "1.0000000000001", strings.Repeat("9", 17)} {
		i.CostBound = bad
		if i.validate() == nil {
			t.Fatalf("accepted invalid finite amount %s", bad)
		}
	}
	i.CostBound = "1"
	i.Affinity.InputDigest = "raw arguments"
	if i.validate() == nil {
		t.Fatal("accepted runtime body in digest metadata")
	}
	for name, refused := range map[string][]limits.Owner{
		"duplicate budget boundary": {{ID: testID, Kind: limits.OwnerKey}, {ID: testID, Kind: limits.OwnerKey}},
		"conflicting account kind":  {{ID: testID, Kind: limits.OwnerKey}, {ID: testID, Kind: limits.OwnerSupply, Basis: limits.BasisCharge}},
		"unknown account kind":      {{ID: testID, Kind: limits.OwnerKind("tenant")}},
	} {
		if _, err := normalizedOwners(refused); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestSettlementRequiresEveryOwnerBasis(t *testing.T) {
	owners := []limits.Owner{
		{ID: testID, Kind: limits.OwnerKey, Basis: limits.BasisCost}, {ID: testID, Kind: limits.OwnerKey, Basis: limits.BasisCharge},
		{ID: otherID, Kind: limits.OwnerSupply, Basis: limits.BasisCost}, {ID: otherID, Kind: limits.OwnerSupply, Basis: limits.BasisCharge},
	}
	accounting := []limits.Snapshot{{OwnerID: testID, Basis: limits.BasisCost}, {OwnerID: testID, Basis: limits.BasisCharge}, {OwnerID: otherID}}
	if !ownersAccounted(owners, accounting) {
		t.Fatal("complete accounting refused")
	}
	if ownersAccounted(owners, accounting[:2]) {
		t.Fatal("unaccounted supply exposure released")
	}
	accounting[1].Basis = limits.BasisCost
	if ownersAccounted(owners, accounting) {
		t.Fatal("consumer charge exposure released with cost-only accounting")
	}
}

func TestHasUnknownDebitRejectsNonCanonicalKeyBeforeQuery(t *testing.T) {
	store := NewStore(nil)
	for _, id := range []string{"", "not-a-uuid", strings.ToUpper(testID), "00000000-0000-0000-0000-000000000000", testID + " "} {
		held, err := store.HasUnknownDebit(t.Context(), id, time.Hour)
		if held || !errors.Is(err, ErrConflict) {
			t.Fatalf("non-canonical key %q was answered instead of refused: %v %v", id, held, err)
		}
	}
}

func TestUnreadableLedgerNeverClearsExposure(t *testing.T) {
	store := NewStore(unreachablePool(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if held, err := store.HasUnknownDebit(ctx, testID, time.Hour); held || err == nil {
		t.Fatalf("an unreadable ledger cleared the key's exposure: %v %v", held, err)
	}
	if err := store.MarkNotSent(ctx, testID, testID); err == nil {
		t.Fatal("an unreadable ledger accepted a not-sent debit")
	}
	runner, err := scripts.NewRunner(refusing{}, "limits")
	if err != nil {
		t.Fatal(err)
	}
	syncer := NewSyncer(store, runner)
	if err := syncer.SyncOwner(ctx, testID); !errors.Is(err, context.Canceled) {
		t.Fatalf("an unreadable ledger rebuilt a projection: %v", err)
	}
	if err := syncer.CheckOwnerAccounting(ctx, testID); !errors.Is(err, context.Canceled) {
		t.Fatalf("unreadable accounting was considered current: %v", err)
	}
	if err := syncer.ReconcileDebits(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("reconciliation silently accepted an unreadable ledger: %v", err)
	}
}

type refusing struct{}

func (refusing) Do(context.Context, ...string) (any, error) {
	return nil, errors.New("connection refused")
}

func pricedResult() Result {
	return Result{
		DispatchID: testID, EventID: testID,
		ResultDigest: bytes.Repeat([]byte{1}, 32), EvidenceDigest: bytes.Repeat([]byte{2}, 32),
		Cost: "0", Charge: "0", Final: true, Complete: true,
	}
}

func TestPricedSettlementRefusesIncompleteEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*Result){
		"not final":        func(r *Result) { r.Final = false },
		"incomplete":       func(r *Result) { r.Complete = false },
		"resolutions":      func(r *Result) { r.ResolvesResultDigests = [][]byte{bytes.Repeat([]byte{3}, 32)} },
		"malformed cost":   func(r *Result) { r.Cost = "-1" },
		"malformed charge": func(r *Result) { r.Charge = "1e3" },
	} {
		result := pricedResult()
		mutate(&result)
		if err := SettleToolTx(t.Context(), nil, result); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s tool evidence settled instead of conflicting: %v", name, err)
		}
		if err := SettleAgentTx(t.Context(), nil, result); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s agent evidence settled instead of conflicting: %v", name, err)
		}
	}
	priced := pricedResult()
	priced.Cost, priced.Charge = "0.25", "0.25"
	if !pricedResult().pricedOutcome() || !priced.pricedOutcome() {
		t.Fatal("complete final evidence refused")
	}
}

func TestPricedSettlementComparesObservedAgainstBounds(t *testing.T) {
	result := pricedResult()
	result.Cost, result.Charge = "0.25", "0.5"
	head := func(bounds [4]string) settleHead {
		return settleHead{costBound: bounds[0], chargeBound: bounds[1], observedCost: bounds[2], observedCharge: bounds[3]}
	}
	for name, bounds := range map[string][4]string{
		"exactly at both bounds":           {"0.25", "0.5", "0", "0"},
		"reaching both bounds after prior": {"1", "1", "0.75", "0.5"},
	} {
		if !head(bounds).admits(result) {
			t.Fatalf("settlement %s refused", name)
		}
	}
	for name, bounds := range map[string][4]string{
		"cost over bound":                 {"0.24", "0.5", "0", "0"},
		"charge over bound":               {"0.25", "0.49", "0", "0"},
		"prior cost pushes over":          {"1", "1", "0.76", "0"},
		"prior charge pushes over":        {"1", "1", "0", "0.51"},
		"zero bound with priced evidence": {"0", "0", "0", "0"},
	} {
		if head(bounds).admits(result) {
			t.Fatalf("%s settled beyond its bound", name)
		}
	}
	if !head([4]string{"0", "0", "0", "0"}).admits(pricedResult()) {
		t.Fatal("explicit zero settlement against zero bounds refused")
	}
}

func TestResultReplayMatchesOnlyIdenticalEvidence(t *testing.T) {
	result := pricedResult()
	resolution, err := resolutionDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, event, cost, charge := result.EvidenceDigest, result.EventID, "0.00", "0.000"
	if !resultMatches(result, resolution, evidence, resolution, event, cost, charge, true, true) {
		t.Fatal("identical settlement evidence was not recognized as a replay")
	}
	changed := bytes.Repeat([]byte{9}, 32)
	for name, matches := range map[string]bool{
		"evidence":    resultMatches(result, resolution, changed, resolution, event, cost, charge, true, true),
		"resolutions": resultMatches(result, resolution, evidence, changed, event, cost, charge, true, true),
		"event":       resultMatches(result, resolution, evidence, resolution, otherID, cost, charge, true, true),
		"cost":        resultMatches(result, resolution, evidence, resolution, event, "0.01", charge, true, true),
		"charge":      resultMatches(result, resolution, evidence, resolution, event, cost, "2", true, true),
		"final":       resultMatches(result, resolution, evidence, resolution, event, cost, charge, false, true),
		"complete":    resultMatches(result, resolution, evidence, resolution, event, cost, charge, true, false),
	} {
		if matches {
			t.Fatalf("changed %s was accepted as a settlement replay", name)
		}
	}
}
