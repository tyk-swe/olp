package usage

import (
	"testing"
	"time"
)

func TestRetainedPricingExactUnitsAndDeferredFees(t *testing.T) {
	p := Price{TrainingPerMillion: new("2.5"), StoragePerGiBDay: new("0.1"), CacheStoragePerMillionTokenHour: new("0.2"), ContainerPerSession: map[string]string{"4g": "0.12"}, PerRequest: new("99")}
	u := AttemptUsage{Observed: true, Complete: true, Deferred: true, TrainingTokens: new(int64(1_000_000)), StorageByteSeconds: new("92771293593600"), CacheTokenSeconds: new("3600000000"), ContainerSessions: map[string]int64{"4g": 2}}
	got, ok := p.Cost(u, time.Now(), true)
	if !ok || got.String() != "3.04" {
		t.Fatalf("exact retained units =%s %v", got.String(), ok)
	}
	// Both terms individually round to zero at monetary precision. Their sum
	// rounds once to one monetary unit; no component loses its fractional charge.
	p = Price{TrainingPerMillion: new("0.00000049"), CacheStoragePerMillionTokenHour: new("0.001764")}
	u = AttemptUsage{Complete: true, Observed: true, Deferred: true, TrainingTokens: new(int64(1)), CacheTokenSeconds: new("1")}
	got, ok = p.Cost(u, time.Now(), true)
	if !ok || got.String() != "0.000000000001" {
		t.Fatalf("single monetary rounding =%s %v", got.String(), ok)
	}
}

func TestRetainedPricingMissingRatesAndExactQuantities(t *testing.T) {
	for _, p := range []Price{{}, {StoragePerGiBDay: new("0.10000000000001")}, {ContainerPerSession: map[string]string{"wrong": "1"}}} {
		if _, ok := p.Cost(AttemptUsage{Complete: true, StorageByteSeconds: new("1")}, time.Now(), true); ok {
			t.Fatal("unrepresentable retained rate was priced")
		}
	}
	exact := AttemptUsage{StorageByteSeconds: new("18446744073709551616000000000.123456789"), ContainerSessions: map[string]int64{"1g": 0}}
	if err := validateRetainedUsage(exact); err != nil {
		t.Fatalf("exact retained quantity rejected: %v", err)
	}
	exact.CacheTokenSeconds = new("1e9")
	if validateRetainedUsage(exact) == nil {
		t.Fatal("exponent quantity accepted")
	}
}

func TestRetainedEvalInvocationsPriceModelRequestsWithoutRepeatingSubmissionFee(t *testing.T) {
	p := Price{InputPerMillion: new("0"), OutputPerMillion: new("0"), PerRequest: new("7")}
	u := AttemptUsage{Complete: true, Observed: true, Deferred: true, InputTokens: new(int64(100)), OutputTokens: new(int64(10)), InvocationCount: new(int64(2))}
	got, ok := p.Cost(u, time.Now(), true)
	if !ok || got.String() != "14" {
		t.Fatalf("actual model invocations=%s %v", got.String(), ok)
	}
	u.InvocationCount = new(int64(0))
	got, ok = p.Cost(u, time.Now(), true)
	if !ok || !got.IsZero() {
		t.Fatalf("zero invocations=%s %v", got.String(), ok)
	}
}
