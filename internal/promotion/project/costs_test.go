package project_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

type promotionSnapshotCommands struct {
	installed int
	fail      bool
}

func (c *promotionSnapshotCommands) Do(context.Context, ...string) (any, error) {
	c.installed++
	if c.fail {
		return nil, errors.New("Valkey unavailable")
	}
	return []any{int64(1), int64(1), "ok", int64(1), int64(1)}, nil
}

func (*promotionSnapshotCommands) Pipeline(context.Context, ...[]string) ([]any, error) {
	return nil, errors.New("Valkey unavailable")
}

func TestPromotionStopsSnapshotInstallationOnFailure(t *testing.T) {
	t.Parallel()
	commands := &promotionSnapshotCommands{fail: true}
	runner, err := scripts.NewRunner(commands, "olp:promotion:")
	if err != nil {
		t.Fatal(err)
	}
	costs := project.CostsOf(admission.NewDurable(runner, nil))
	snapshots := []limits.Snapshot{
		{OwnerID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607", Basis: limits.BasisCost, DailyAccrued: "1", MonthlyAccrued: "1"},
		{OwnerID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607", Basis: limits.BasisCharge, DailyAccrued: "1", MonthlyAccrued: "1"},
	}
	if err := costs.Install(t.Context(), snapshots); err == nil {
		t.Fatal("accepted failed cost install")
	}
	if commands.installed != 1 {
		t.Fatal("published charge snapshot after failed cost install")
	}
	commands.fail = false
	if err := costs.Install(t.Context(), snapshots[1:]); err != nil {
		t.Fatal(err)
	}
	if commands.installed != 2 {
		t.Fatal("did not install valid snapshot")
	}
}
