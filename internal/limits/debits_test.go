package limits

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDurableResolutionEvidenceIdentity(t *testing.T) {
	a, b := make([]byte, 32), make([]byte, 32)
	b[0] = 1
	one, err := debitResolutionDigest([][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	two, err := debitResolutionDigest([][]byte{b, a})
	if err != nil || !bytes.Equal(one, two) {
		t.Fatal("resolution identity depends on ordering")
	}
	if _, err = debitResolutionDigest([][]byte{a, a}); err == nil {
		t.Fatal("duplicate resolution accepted")
	}
	if _, err = debitResolutionDigest([][]byte{[]byte("runtime body")}); err == nil {
		t.Fatal("content accepted as result digest")
	}
}

const debitTestID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"

func TestDurableDebitIdentityAndOwners(t *testing.T) {
	i := DebitIntent{ID: debitTestID, APIKeyID: debitTestID, ProjectID: debitTestID, ClaimID: debitTestID, Kind: "api", Currency: "USD", CostBound: "0.125", ChargeBound: "0.25", PreparedUntil: time.Now().Add(time.Minute), Affinity: ExecutionAffinity{ReleaseID: debitTestID, RevisionID: debitTestID}}
	if err := i.validate(); err != nil {
		t.Fatal(err)
	}
	owners, err := normalizedDebitOwners([]DebitOwner{{ID: debitTestID, Kind: "key"}})
	if err != nil || len(owners) != 2 || owners[0].Basis != BasisCharge || owners[1].Basis != BasisCost {
		t.Fatalf("both accounting bases must survive policy changes: %+v %v", owners, err)
	}
	digest := debitDigest(i, owners)
	i.ClaimID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"
	if debitDigest(i, owners) != digest {
		t.Fatal("private fence changed logical identity")
	}
	i.CostBound = "0.126"
	if debitDigest(i, owners) == digest {
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
	if _, err = normalizedDebitOwners([]DebitOwner{{ID: debitTestID, Kind: "key"}, {ID: debitTestID, Kind: "key"}}); err == nil {
		t.Fatal("accepted duplicate budget boundary")
	}
	if _, err = normalizedDebitOwners([]DebitOwner{{ID: debitTestID, Kind: "key"}, {ID: debitTestID, Kind: "supply", Basis: BasisCharge}}); err == nil {
		t.Fatal("accepted conflicting account kind")
	}
	if !equalDebitAmount("0001.250000000000", "1.25") || equalDebitAmount("0.000000000001", "0") {
		t.Fatal("monetary evidence comparison lost precision")
	}
}

func TestDurableProjectionAndVerification(t *testing.T) {
	args, err := appendDebitProjection(make([]string, 15), &debitProjection{Version: 42, Amount: "0.000000000001"})
	if err != nil || len(args) != 18 || args[15] != "42" || args[16] != "0.000000000001" || args[17] != "0" {
		t.Fatal(args, err)
	}
	if _, err = appendDebitProjection(nil, &debitProjection{}); err == nil {
		t.Fatal("accepted projection without calendar evidence")
	}
	server := &scripted{answer: answering(t, map[string][2]any{"reserve_cost": {costGranted, nil}})}
	limiter := &Limiter{client: server, namespace: "olp:test"}
	cap := "1"
	if err = limiter.verifyDurableCost(t.Context(), Request{CostOwnerID: debitTestID, DailyCostLimit: &cap}); err != nil {
		t.Fatal(err)
	}
	commands := server.commands()
	last := commands[len(commands)-1]
	if last[2] != "6" || !strings.HasSuffix(last[8], ":durable") || last[12] != "verify" {
		t.Fatalf("verification acquired an expiring lease: %v", last)
	}
	for _, test := range []struct {
		detail string
		kind   resultKind
	}{{"uninitialized_durable_cost_state", resultUninitialized}, {"malformed_durable_cost_state", resultMalformed}} {
		result, err := parseCostReservation(reply(int64(1), int64(-1), test.detail, int64(0), int64(1), int64(2)))
		if err != nil || result.kind != test.kind {
			t.Fatal(result, err)
		}
	}
	server.answer = func([]string) (any, error) {
		return reply(int64(1), int64(-1), "uninitialized_durable_cost_state", int64(0), int64(1), int64(2)), nil
	}
	limiter.syncDurable = func(_ context.Context, _ string) error { return ErrDebitAuthority }
	_, err = limiter.reserveCost(t.Context(), Request{CostOwnerID: debitTestID, DailyCostLimit: &cap}, limiter.keysFor(Request{CostOwnerID: debitTestID}))
	if !errors.Is(err, ErrDebitAuthority) {
		t.Fatalf("failed synchronization admitted work: %v", err)
	}
	server.answer = func([]string) (any, error) { return costGranted, nil }
	limiter.syncDurable = func(context.Context, string) error { t.Fatal("warm admission queried PostgreSQL"); return nil }
	if _, err = limiter.reserveCost(t.Context(), Request{CostOwnerID: debitTestID, DailyCostLimit: &cap}, limiter.keysFor(Request{CostOwnerID: debitTestID})); err != nil {
		t.Fatal(err)
	}

}

func TestDurableGlobalNamespaceAndUnresolvedOverrun(t *testing.T) {
	i := DebitIntent{ID: debitTestID, APIKeyID: debitTestID, ClaimID: debitTestID, Kind: "api", Affinity: ExecutionAffinity{ReleaseID: debitTestID, RevisionID: debitTestID}, Currency: "USD", CostBound: "1", ChargeBound: "1", PreparedUntil: time.Now().Add(time.Minute)}
	if err := i.validate(); err != nil {
		t.Fatalf("global API namespace refused: %v", err)
	}
	i.Kind = "tool"
	if i.validate() == nil {
		t.Fatal("projectless tool accepted")
	}
	args, err := appendDebitProjection(make([]string, 15), &debitProjection{Version: 1, Amount: "0", Blocked: true})
	if err != nil || args[17] != "1" {
		t.Fatal(args, err)
	}
	server := &scripted{answer: func([]string) (any, error) {
		return reply(int64(1), int64(-1), "unsettled_durable_cost_state", int64(0), int64(1), int64(2)), nil
	}}
	limiter, _ := New(server, "limits")
	limiter.syncDurable = func(context.Context, string) error {
		t.Fatal("unresolved overrun triggered projection retry")
		return nil
	}
	cap := "10"
	r := Request{CostOwnerID: debitTestID, DailyCostLimit: &cap}
	if _, err = limiter.reserveCost(t.Context(), r, limiter.keysFor(r)); !errors.Is(err, ErrUninitializedCost) {
		t.Fatalf("zero remaining bound admitted unresolved overrun: %v", err)
	}
}
