package geminilive

import (
	"errors"
	"math"
	"testing"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/duplexrelay"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestModalitySharesSaturateOnOverflow(t *testing.T) {
	overflowing := &wire.Usage{}
	geminiLiveModality(overflowing, &geminilifecycle.LiveUsage{PromptDetails: []geminilifecycle.LiveUsageDetail{
		{Modality: "IMAGE", TokenCount: math.MaxInt64},
		{Modality: "VIDEO", TokenCount: math.MaxInt64},
	}})
	if overflowing.ImageInputTokens == nil || *overflowing.ImageInputTokens <= 0 {
		t.Errorf("overflowing image share = %v, want a saturated positive share", overflowing.ImageInputTokens)
	}
}

func TestStrictLiveRelayKeepsPolicyFailureWhenDeliveryStops(t *testing.T) {
	t.Parallel()
	x := &kernel.Call{}
	x.Route = &routing.Route{Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict}}
	st := liveRelay{x: x}
	_, complete, err := st.finalize(duplexrelay.Result{
		FirstErr:       errLiveAuthority,
		CloseCode:      websocket.StatusPolicyViolation,
		DeliveryFailed: true,
	})
	if complete || !errors.Is(err, errLiveAuthority) {
		t.Fatalf("session complete = %v, error = %v, want policy failure", complete, err)
	}
}
