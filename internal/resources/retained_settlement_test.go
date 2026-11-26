package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

func int64Ptr(v int64) *int64    { return &v }
func stringPtr(v string) *string { return &v }

func TestNormalizeRetainedEvidenceNoCallerMutation(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)

	t.Run("storage byte-seconds pointer is never mutated in place", func(t *testing.T) {
		t.Parallel()
		rawStorage := "00123.4500"
		line := RetainedEvidenceLine{
			Component: "storage",
			Model:     "m-storage",
			Start:     start,
			End:       end,
			Usage:     usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{StorageByteSeconds: &rawStorage}},
		}
		out, err := normalizeRetainedEvidence(line)
		if err != nil {
			t.Fatalf("normalizeRetainedEvidence failed: %v", err)
		}
		if rawStorage != "00123.4500" {
			t.Fatalf("caller StorageByteSeconds mutated in place to %q", rawStorage)
		}
		if out.Usage.StorageByteSeconds == &rawStorage {
			t.Fatalf("returned StorageByteSeconds aliases caller pointer")
		}
		if *out.Usage.StorageByteSeconds != "123.45" {
			t.Fatalf("got normalized %q, want 123.45", *out.Usage.StorageByteSeconds)
		}
	})

	t.Run("cache token-seconds pointer is never mutated during digest calculation", func(t *testing.T) {
		t.Parallel()
		rawCache := "0000.5000"
		obs := RetainedObservation{
			Lines: []RetainedEvidenceLine{{
				Component: "cache_storage",
				Model:     "m-cache",
				Start:     start,
				End:       end,
				Usage:     usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{CacheTokenSeconds: &rawCache}},
			}},
		}
		if _, err := retainedObservationDigest(obs); err != nil {
			t.Fatalf("retainedObservationDigest failed: %v", err)
		}
		if rawCache != "0000.5000" {
			t.Fatalf("caller CacheTokenSeconds mutated during digest to %q", rawCache)
		}
	})

	t.Run("container sessions map is cloned", func(t *testing.T) {
		t.Parallel()
		sessions := map[string]int64{"1g": 2}
		line := RetainedEvidenceLine{
			Component:  "container",
			Model:      "m-container",
			MemoryTier: "1g",
			Start:      start,
			End:        end,
			Usage:      usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{ContainerSessions: sessions}},
		}
		out, err := normalizeRetainedEvidence(line)
		if err != nil {
			t.Fatalf("normalizeRetainedEvidence failed: %v", err)
		}
		out.Usage.ContainerSessions["1g"] = 999
		if sessions["1g"] != 2 {
			t.Fatalf("caller ContainerSessions map was aliased and mutated to %d", sessions["1g"])
		}
	})

	t.Run("failing normalization never mutates caller pointers", func(t *testing.T) {
		t.Parallel()
		rawStorage := "00123.4500"
		subMicroStart := time.Date(2026, 5, 1, 10, 0, 0, 100, time.UTC)
		subMicroEnd := time.Date(2026, 5, 1, 10, 0, 0, 900, time.UTC)
		line := RetainedEvidenceLine{
			Component: "storage",
			Model:     "m-storage",
			Start:     subMicroStart,
			End:       subMicroEnd,
			Usage:     usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{StorageByteSeconds: &rawStorage}},
		}
		if _, err := normalizeRetainedEvidence(line); !errors.Is(err, ErrContract) {
			t.Fatalf("got err=%v, want ErrContract", err)
		}
		if rawStorage != "00123.4500" {
			t.Fatalf("caller StorageByteSeconds mutated on failing normalization to %q", rawStorage)
		}

		rawCache := "0009.5000"
		inconsistent := RetainedEvidenceLine{
			Component: "cache_storage",
			Model:     "m-cache",
			Start:     start,
			End:       end,
			Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{
				CacheTokenSeconds: &rawCache,
				CachedInputTokens: int64Ptr(-1),
			}},
		}
		if _, err := normalizeRetainedEvidence(inconsistent); !errors.Is(err, ErrContract) {
			t.Fatalf("got err=%v, want ErrContract", err)
		}
		if rawCache != "0009.5000" {
			t.Fatalf("caller CacheTokenSeconds mutated on inconsistent usage failure to %q", rawCache)
		}
	})
}

func TestRetainedObservationDigestDeterminism(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(2 * time.Minute)

	t.Run("same start different end hashes identically regardless of line order", func(t *testing.T) {
		t.Parallel()
		lineShort := RetainedEvidenceLine{
			Component: "storage",
			Model:     "model:v1",
			Start:     t0,
			End:       t1,
			Usage:     usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{StorageByteSeconds: stringPtr("0010.50")}},
		}
		lineLong := RetainedEvidenceLine{
			Component: "storage",
			Model:     "model:v1",
			Start:     t0,
			End:       t2,
			Usage:     usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{StorageByteSeconds: stringPtr("20")}},
		}
		d1, err := retainedObservationDigest(RetainedObservation{
			Lines:      []RetainedEvidenceLine{lineShort, lineLong},
			ObservedAt: t1,
		})
		if err != nil {
			t.Fatalf("digest 1 failed: %v", err)
		}
		d2, err := retainedObservationDigest(RetainedObservation{
			Lines:      []RetainedEvidenceLine{lineLong, lineShort},
			ObservedAt: t2,
		})
		if err != nil {
			t.Fatalf("digest 2 failed: %v", err)
		}
		if d1 != d2 {
			t.Fatalf("digest depended on input order or ObservedAt: %s vs %s", d1, d2)
		}
	})

	t.Run("colon in model name does not cause sort delimiter collision", func(t *testing.T) {
		t.Parallel()
		a := RetainedEvidenceLine{Component: "container", Model: "m", MemoryTier: "4g", Start: t0, End: t1}
		b := RetainedEvidenceLine{Component: "container", Model: "m:4g", MemoryTier: "", Start: t0, End: t1}
		if compareEvidenceLines(a, b) >= 0 || compareEvidenceLines(b, a) <= 0 {
			t.Fatalf("expected strict antisymmetric ordering between %+v and %+v", a, b)
		}
	})

	t.Run("equivalent unnormalized quantities produce identical digest", func(t *testing.T) {
		t.Parallel()
		d1, err := retainedObservationDigest(RetainedObservation{
			Lines: []RetainedEvidenceLine{{
				Component: "storage", Model: "m", Start: t0, End: t1,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{StorageByteSeconds: stringPtr("001.2500")}},
			}},
		})
		if err != nil {
			t.Fatalf("digest 1: %v", err)
		}
		d2, err := retainedObservationDigest(RetainedObservation{
			Lines: []RetainedEvidenceLine{{
				Component: "storage", Model: "m", Start: t0, End: t1,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{StorageByteSeconds: stringPtr("1.25")}},
			}},
		})
		if err != nil {
			t.Fatalf("digest 2: %v", err)
		}
		if d1 != d2 {
			t.Fatalf("equivalent quantities produced different digests: %s vs %s", d1, d2)
		}
	})
}

func TestNormalizeRetainedEvidenceMatrix(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 1, 10, 0, 0, 123456789, time.UTC)
	end := start.Add(time.Second)
	nonUTC := time.FixedZone("UTC+2", 2*3600)

	validGeneration := usage.AttemptUsage{
		UsageQuantities: usage.UsageQuantities{
			InputTokens:       int64Ptr(100),
			OutputTokens:      int64Ptr(25),
			CachedInputTokens: int64Ptr(10),
			InvocationCount:   int64Ptr(2),
		},
	}

	tests := []struct {
		name    string
		line    RetainedEvidenceLine
		wantErr bool
	}{
		{
			name: "valid training",
			line: RetainedEvidenceLine{
				Component: "training", Model: "m", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{TrainingTokens: int64Ptr(500)}},
			},
		},
		{
			name: "valid storage",
			line: RetainedEvidenceLine{
				Component: "storage", Model: "m", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{StorageByteSeconds: stringPtr("42.5")}},
			},
		},
		{
			name: "valid cache_storage",
			line: RetainedEvidenceLine{
				Component: "cache_storage", Model: "m", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{CacheTokenSeconds: stringPtr("0")}},
			},
		},
		{
			name: "valid container",
			line: RetainedEvidenceLine{
				Component: "container", Model: "m", MemoryTier: "4g", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{ContainerSessions: map[string]int64{"4g": 1}}},
			},
		},
		{
			name: "valid generation",
			line: RetainedEvidenceLine{
				Component: "generation", Model: "m", Start: start, End: end,
				Usage: validGeneration,
			},
		},
		{
			name: "rejects non-UTC timestamp",
			line: RetainedEvidenceLine{
				Component: "training", Model: "m", Start: start.In(nonUTC), End: end.In(nonUTC),
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{TrainingTokens: int64Ptr(10)}},
			},
			wantErr: true,
		},
		{
			name: "rejects sub-microsecond interval collapsing after truncation",
			line: RetainedEvidenceLine{
				Component: "training", Model: "m",
				Start: time.Date(2026, 5, 1, 10, 0, 0, 100, time.UTC),
				End:   time.Date(2026, 5, 1, 10, 0, 0, 900, time.UTC),
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{TrainingTokens: int64Ptr(10)}},
			},
			wantErr: true,
		},
		{
			name: "rejects cross-component contamination",
			line: RetainedEvidenceLine{
				Component: "training", Model: "m", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{
					TrainingTokens:     int64Ptr(10),
					StorageByteSeconds: stringPtr("5"),
				}},
			},
			wantErr: true,
		},
		{
			name: "rejects negative training tokens",
			line: RetainedEvidenceLine{
				Component: "training", Model: "m", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{TrainingTokens: int64Ptr(-1)}},
			},
			wantErr: true,
		},
		{
			name: "rejects scientific notation quantity",
			line: RetainedEvidenceLine{
				Component: "storage", Model: "m", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{StorageByteSeconds: stringPtr("1e5")}},
			},
			wantErr: true,
		},
		{
			name: "rejects memory tier on non-container component",
			line: RetainedEvidenceLine{
				Component: "storage", Model: "m", MemoryTier: "1g", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{StorageByteSeconds: stringPtr("1")}},
			},
			wantErr: true,
		},
		{
			name: "rejects container tier mismatch",
			line: RetainedEvidenceLine{
				Component: "container", Model: "m", MemoryTier: "4g", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{ContainerSessions: map[string]int64{"1g": 1}}},
			},
			wantErr: true,
		},
		{
			name: "rejects batch flag",
			line: RetainedEvidenceLine{
				Component: "generation", Model: "m", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{
					InputTokens: int64Ptr(10), OutputTokens: int64Ptr(5), CachedInputTokens: int64Ptr(0), InvocationCount: int64Ptr(1), Batch: true,
				}},
			},
			wantErr: true,
		},
		{
			name: "rejects cached tokens exceeding input tokens",
			line: RetainedEvidenceLine{
				Component: "generation", Model: "m", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{
					InputTokens: int64Ptr(10), OutputTokens: int64Ptr(5), CachedInputTokens: int64Ptr(11), InvocationCount: int64Ptr(1),
				}},
			},
			wantErr: true,
		},
		{
			name: "rejects unknown component",
			line: RetainedEvidenceLine{
				Component: "unknown", Model: "m", Start: start, End: end,
				Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{TrainingTokens: int64Ptr(1)}},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := normalizeRetainedEvidence(tc.line)
			if tc.wantErr {
				if !errors.Is(err, ErrContract) {
					t.Fatalf("got err=%v, want ErrContract", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.Usage.Evidence != usage.EvidenceObservedComplete || !out.Usage.Deferred {
				t.Fatalf("expected observed complete deferred usage, got %+v", out.Usage)
			}
			if !out.Start.Equal(tc.line.Start.UTC().Truncate(time.Microsecond)) {
				t.Fatalf("start not microsecond-truncated: got %v", out.Start)
			}
		})
	}
}

func TestValidateEvidenceSubmissionAndJSONHelpers(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("payload"))
	validDigest := hex.EncodeToString(sum[:])

	t.Run("validateEvidenceSubmission", func(t *testing.T) {
		t.Parallel()
		valid := EvidenceSubmission{Key: "k1", Digest: validDigest, Provenance: "provider_verified"}
		if err := validateEvidenceSubmission(valid); err != nil {
			t.Fatalf("expected valid submission, got %v", err)
		}
		badCases := []EvidenceSubmission{
			{Key: "", Digest: validDigest, Provenance: "provider_verified"},
			{Key: strings.Repeat("a", 129), Digest: validDigest, Provenance: "provider_verified"},
			{Key: "k1", Digest: strings.ToUpper(validDigest), Provenance: "provider_verified"},
			{Key: "k1", Digest: "short", Provenance: "provider_verified"},
			{Key: "k1", Digest: strings.Repeat("z", 64), Provenance: "provider_verified"},
			{Key: "k1", Digest: validDigest, Provenance: "self_asserted"},
		}
		for i, sub := range badCases {
			if err := validateEvidenceSubmission(sub); !errors.Is(err, ErrContract) {
				t.Fatalf("case %d: got %v, want ErrContract", i, err)
			}
		}
	})

	t.Run("sameJSON preserves large integer precision and ignores key order", func(t *testing.T) {
		t.Parallel()
		if !sameJSON([]byte(`{"b":1,"a":9007199254740993}`), []byte(`{"a":9007199254740993,"b":1}`)) {
			t.Fatalf("expected equivalent JSON with reordered keys to match")
		}
		if sameJSON([]byte(`{"a":9007199254740992}`), []byte(`{"a":9007199254740993}`)) {
			t.Fatalf("distinct large integers must not compare equal")
		}
		if sameJSON([]byte(`{invalid`), []byte(`{}`)) {
			t.Fatalf("invalid JSON must return false")
		}
	})

	t.Run("normalizeRetainedLines enforces pinned component tier and templates", func(t *testing.T) {
		t.Parallel()
		t0 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
		t1 := t0.Add(time.Minute)
		templates := map[string]*usage.Event{"pinned-model": {}}

		good := []RetainedEvidenceLine{{
			Component: "container", Model: "pinned-model", MemoryTier: "4g", Start: t0, End: t1,
			Usage: usage.AttemptUsage{UsageQuantities: usage.UsageQuantities{ContainerSessions: map[string]int64{"4g": 1}}},
		}}
		if _, err := normalizeRetainedLines(good, "container", "4g", templates); err != nil {
			t.Fatalf("valid line failed: %v", err)
		}
		if _, err := normalizeRetainedLines(good, "container", "1g", templates); !errors.Is(err, ErrContract) {
			t.Fatalf("wrong pinned memory tier should fail with ErrContract, got %v", err)
		}
		if _, err := normalizeRetainedLines(good, "storage", "4g", templates); !errors.Is(err, ErrContract) {
			t.Fatalf("wrong component should fail with ErrContract, got %v", err)
		}
		if _, err := normalizeRetainedLines(good, "container", "4g", map[string]*usage.Event{}); !errors.Is(err, ErrContract) {
			t.Fatalf("unpinned model should fail with ErrContract, got %v", err)
		}
	})
}
