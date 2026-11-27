package resources

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestLocalIDAndParseLocalID(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("0195b6e8-4f20-7c10-a91b-123456789abc")
	kinds := append([]string{
		KindFile, KindBatch, KindResponse, KindContinuation, KindStrictResponse, KindInteraction,
		KindStrictFile, KindStrictBatch, KindAnthropicFile, KindAnthropicBatch, KindGeminiUpload,
		KindGeminiFile, KindGeminiBatch, KindCloudBatch,
	}, retainedapi.Kinds()...)

	for _, kind := range kinds {
		local := LocalID(kind, id)
		gotKind, gotID, err := ParseLocalID(local)
		if err != nil {
			t.Fatalf("ParseLocalID(%q) unexpected error: %v", local, err)
		}
		if gotKind != kind || gotID != id {
			t.Fatalf("ParseLocalID(%q) = (%q, %v), want (%q, %v)", local, gotKind, gotID, kind, id)
		}
	}

	malformed := []string{
		"",
		"file",
		"_0195b6e84f207c10a91b123456789abc",
		"unknown_0195b6e84f207c10a91b123456789abc",
		"file_0195b6e84f207c10a91b123456789ab",
		"file_0195b6e84f207c10a91b123456789abcd",
		"file_0195b6e8-4f20-7c10-a91b-123456789abc",
		"file_0195B6E84F207C10A91B123456789ABC",
		"file_0195b6e84f207c10a91b123456789abg",
		"file_00000000000000000000000000000000",
		"file_0195b6e84f207c10a91b123456789abc ",
	}
	for _, raw := range malformed {
		gotKind, gotID, err := ParseLocalID(raw)
		if !errors.Is(err, ErrNotFound) || gotKind != "" || gotID != uuid.Nil {
			t.Errorf("ParseLocalID(%q) = (%q, %v, %v), want (\"\", Nil, ErrNotFound)", raw, gotKind, gotID, err)
		}
	}
}

func TestListCursorLocalUUID(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("0195b6e8-4f20-7c10-a91b-123456789abc")

	got, err := (ListCursor{}).localUUID([]string{KindFile})
	if err != nil || got != uuid.Nil {
		t.Fatalf("empty cursor = (%v, %v), want (Nil, nil)", got, err)
	}

	got, err = (ListCursor{ID: LocalID(KindStrictFile, id)}).localUUID([]string{KindFile, KindStrictFile})
	if err != nil || got != id {
		t.Fatalf("matching kind cursor = (%v, %v), want (%v, nil)", got, err, id)
	}

	for _, tc := range []struct {
		name  string
		id    string
		kinds []string
	}{
		{name: "kind mismatch", id: LocalID(KindBatch, id), kinds: []string{KindFile, KindStrictFile}},
		{name: "empty kinds", id: LocalID(KindFile, id), kinds: nil},
		{name: "malformed id", id: "file_notauuid", kinds: []string{KindFile}},
	} {
		got, err := (ListCursor{ID: tc.id}).localUUID(tc.kinds)
		if !errors.Is(err, ErrNotFound) || got != uuid.Nil {
			t.Errorf("%s: localUUID = (%v, %v), want (Nil, ErrNotFound)", tc.name, got, err)
		}
	}
}

func TestSubmissionIDAndValidateSubmission(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	id := uuid.MustParse("0195b6e8-4f20-7c10-a91b-123456789abc")

	validTimes := []time.Time{
		now,
		now.Add(-15 * time.Minute),
		now.Add(5 * time.Minute),
	}
	for _, at := range validTimes {
		sub := SubmissionID(at, id)
		if err := ValidateSubmission(sub, now); err != nil {
			t.Errorf("ValidateSubmission(%q) at %v = %v, want nil", sub, at, err)
		}
	}

	invalid := []string{
		SubmissionID(now.Add(-15*time.Minute-time.Millisecond), id),
		SubmissionID(now.Add(5*time.Minute+time.Millisecond), id),
		SubmissionID(now, uuid.Nil),
		"",
		"notadot",
		"abc." + id.String(),
		"+" + SubmissionID(now, id),
		"0" + SubmissionID(now, id),
		strings.ToUpper(SubmissionID(now, id)),
		SubmissionID(now, id) + ".extra",
	}
	for _, sub := range invalid {
		if err := ValidateSubmission(sub, now); !errors.Is(err, ErrSubmission) {
			t.Errorf("ValidateSubmission(%q) = %v, want ErrSubmission", sub, err)
		}
	}
}

func TestValidateContract(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	version := "v1"
	validExpires := now.Add(time.Hour)
	base := func() *Resource {
		v := version
		exp := validExpires
		return &Resource{
			Kind:            KindContinuation,
			ContractVersion: &v,
			ExpiresAt:       &exp,
			Metadata:        json.RawMessage(`{}`),
		}
	}

	unencrypted := New(nil)
	if err := unencrypted.validateContract(base(), []byte("x")); !errors.Is(err, ErrContract) {
		t.Fatalf("unencrypted store validateContract = %v, want ErrContract", err)
	}
	var nilStore *Store
	if nilStore.Encrypted() {
		t.Fatalf("nil Store.Encrypted() = true, want false")
	}
	if NewEncrypted(nil, "", &secrets.KeyRing{}).Encrypted() {
		t.Fatalf("empty installation Encrypted() = true, want false")
	}
	if !NewEncrypted(nil, "install-1", &secrets.KeyRing{}).Encrypted() {
		t.Fatalf("configured encrypted store Encrypted() = false, want true")
	}

	cases := []struct {
		name      string
		encrypted bool
		build     func() (*Resource, []byte)
		want      error
	}{
		{
			name:      "valid continuation",
			encrypted: true,
			build:     func() (*Resource, []byte) { return base(), []byte("payload") },
		},
		{
			name:      "valid strict response at exact bounds",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base()
				r.Kind = KindStrictResponse
				v := strings.Repeat("a", 128)
				exp := now.Add(ContinuationLifetime)
				r.ContractVersion = &v
				r.ExpiresAt = &exp
				r.Metadata = bytes.Repeat([]byte("m"), metadataLimit)
				return r, bytes.Repeat([]byte("p"), MaxContinuationBytes)
			},
		},
		{
			name:      "unencrypted",
			encrypted: false,
			build:     func() (*Resource, []byte) { return base(), []byte("payload") },
			want:      ErrContract,
		},
		{
			name:      "nil resource",
			encrypted: true,
			build:     func() (*Resource, []byte) { return nil, []byte("payload") },
			want:      ErrContract,
		},
		{
			name:      "wrong kind",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base()
				r.Kind = KindStrictFile
				return r, []byte("payload")
			},
			want: ErrContract,
		},
		{
			name:      "empty contract version",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base()
				empty := ""
				r.ContractVersion = &empty
				return r, []byte("payload")
			},
			want: ErrContract,
		},
		{
			name:      "contract version over 128 bytes",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base()
				long := strings.Repeat("a", 129)
				r.ContractVersion = &long
				return r, []byte("payload")
			},
			want: ErrContract,
		},
		{
			name:      "empty payload",
			encrypted: true,
			build:     func() (*Resource, []byte) { return base(), nil },
			want:      ErrPayloadTooLarge,
		},
		{
			name:      "payload over limit",
			encrypted: true,
			build:     func() (*Resource, []byte) { return base(), make([]byte, MaxContinuationBytes+1) },
			want:      ErrPayloadTooLarge,
		},
		{
			name:      "expires at now",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base()
				exp := now
				r.ExpiresAt = &exp
				return r, []byte("payload")
			},
			want: ErrContract,
		},
		{
			name:      "expires beyond lifetime",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base()
				exp := now.Add(ContinuationLifetime + time.Second)
				r.ExpiresAt = &exp
				return r, []byte("payload")
			},
			want: ErrContract,
		},
		{
			name:      "metadata over limit",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base()
				r.Metadata = make([]byte, metadataLimit+1)
				return r, []byte("payload")
			},
			want: ErrMetadataTooLarge,
		},
	}
	for _, tc := range cases {
		r, payload := tc.build()
		err := validateContractAt(tc.encrypted, r, payload, now)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: validateContractAt = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestValidateDurable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	base := func(kind string) *Resource {
		v := DurableContractVersion
		exp := now.Add(24 * time.Hour)
		return &Resource{
			Kind:            kind,
			ContractVersion: &v,
			ExpiresAt:       &exp,
			Metadata:        json.RawMessage(`{}`),
		}
	}

	if err := New(nil).validateDurable(base(KindStrictFile), []byte("x")); !errors.Is(err, ErrContract) {
		t.Fatalf("unencrypted validateDurable = %v, want ErrContract", err)
	}

	cases := []struct {
		name      string
		encrypted bool
		build     func() (*Resource, []byte)
		want      error
	}{
		{
			name:      "valid strict file",
			encrypted: true,
			build:     func() (*Resource, []byte) { return base(KindStrictFile), []byte("payload") },
		},
		{
			name:      "valid strict batch at exact lifetime",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base(KindStrictBatch)
				exp := now.Add(DurableLifetime)
				r.ExpiresAt = &exp
				return r, []byte("payload")
			},
		},
		{
			name:      "wrong kind",
			encrypted: true,
			build:     func() (*Resource, []byte) { return base(KindFile), []byte("payload") },
			want:      ErrContract,
		},
		{
			name:      "wrong version",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base(KindStrictFile)
				other := "v2"
				r.ContractVersion = &other
				return r, []byte("payload")
			},
			want: ErrContract,
		},
		{
			name:      "submission id forbidden",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base(KindStrictFile)
				sub := "sub-1"
				r.SubmissionID = &sub
				return r, []byte("payload")
			},
			want: ErrContract,
		},
		{
			name:      "expires beyond durable lifetime",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base(KindStrictBatch)
				exp := now.Add(DurableLifetime + time.Second)
				r.ExpiresAt = &exp
				return r, []byte("payload")
			},
			want: ErrContract,
		},
		{
			name:      "empty payload",
			encrypted: true,
			build:     func() (*Resource, []byte) { return base(KindStrictFile), nil },
			want:      ErrPayloadTooLarge,
		},
		{
			name:      "metadata over limit",
			encrypted: true,
			build: func() (*Resource, []byte) {
				r := base(KindStrictFile)
				r.Metadata = make([]byte, metadataLimit+1)
				return r, []byte("payload")
			},
			want: ErrMetadataTooLarge,
		},
	}
	for _, tc := range cases {
		r, payload := tc.build()
		err := validateDurableAt(tc.encrypted, r, payload, now)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: validateDurableAt = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestPrepareResourceForPutDoesNotMutateCaller(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)

	input := &Resource{
		Kind:     KindResponse,
		TargetID: "target-1",
		Metadata: json.RawMessage(`{"pending_usage":{"id":"gen-1"}}`),
	}
	prepared, err := prepareResourceForPut(input, now)
	if err != nil {
		t.Fatalf("prepareResourceForPut valid response: %v", err)
	}
	if prepared.UUID == uuid.Nil {
		t.Fatalf("prepared UUID was not generated")
	}
	if prepared.ExpiresAt == nil || !prepared.ExpiresAt.Equal(now.Add(ContinuationLifetime)) {
		t.Fatalf("prepared ExpiresAt = %v, want %v", prepared.ExpiresAt, now.Add(ContinuationLifetime))
	}
	if input.UUID != uuid.Nil || input.ExpiresAt != nil {
		t.Fatalf("caller resource was mutated: UUID=%v ExpiresAt=%v", input.UUID, input.ExpiresAt)
	}

	emptyMeta := &Resource{Kind: KindFile, TargetID: "target-1"}
	preparedFile, err := prepareResourceForPut(emptyMeta, now)
	if err != nil {
		t.Fatalf("prepareResourceForPut empty metadata: %v", err)
	}
	if string(preparedFile.Metadata) != "{}" {
		t.Fatalf("prepared metadata = %q, want {}", preparedFile.Metadata)
	}
	if emptyMeta.Metadata != nil || emptyMeta.UUID != uuid.Nil {
		t.Fatalf("caller resource with empty metadata was mutated: %+v", emptyMeta)
	}

	nullPending := &Resource{
		Kind:     KindResponse,
		TargetID: "target-1",
		Metadata: json.RawMessage(`{"pending_usage":null}`),
	}
	preparedNull, err := prepareResourceForPut(nullPending, now)
	if err != nil {
		t.Fatalf("prepareResourceForPut null pending_usage: %v", err)
	}
	if preparedNull.ExpiresAt != nil {
		t.Fatalf("null pending_usage should not assign ExpiresAt, got %v", preparedNull.ExpiresAt)
	}

	nonOwnerPending := &Resource{
		Kind:     KindFile,
		TargetID: "target-1",
		Metadata: json.RawMessage(`{"pending_usage":{"opaque":true}}`),
	}
	preparedNonOwner, err := prepareResourceForPut(nonOwnerPending, now)
	if err != nil {
		t.Fatalf("prepareResourceForPut non-owner pending_usage: %v", err)
	}
	if preparedNonOwner.ExpiresAt != nil {
		t.Fatalf("file kind should not assign pending usage ExpiresAt, got %v", preparedNonOwner.ExpiresAt)
	}

	if _, err := prepareResourceForPut(nil, now); !errors.Is(err, ErrContract) {
		t.Errorf("nil resource = %v, want ErrContract", err)
	}
	if _, err := prepareResourceForPut(&Resource{Kind: KindFile}, now); !errors.Is(err, ErrContract) {
		t.Errorf("missing TargetID = %v, want ErrContract", err)
	}
	for _, sealedKind := range []string{KindContinuation, KindStrictResponse, KindStrictFile, KindStrictBatch} {
		if _, err := prepareResourceForPut(&Resource{Kind: sealedKind, TargetID: "t"}, now); !errors.Is(err, ErrContract) {
			t.Errorf("sealed kind %q = %v, want ErrContract", sealedKind, err)
		}
	}
	oversized := &Resource{Kind: KindFile, TargetID: "t", Metadata: make([]byte, metadataLimit+1)}
	if _, err := prepareResourceForPut(oversized, now); !errors.Is(err, ErrMetadataTooLarge) {
		t.Errorf("oversized metadata = %v, want ErrMetadataTooLarge", err)
	}
	badJSON := &Resource{Kind: KindResponse, TargetID: "t", Metadata: json.RawMessage(`{invalid`)}
	if _, err := prepareResourceForPut(badJSON, now); err == nil {
		t.Errorf("invalid response metadata succeeded, want JSON error")
	}
	if badJSON.UUID != uuid.Nil || badJSON.ExpiresAt != nil {
		t.Errorf("failing prepareResourceForPut mutated caller: %+v", badJSON)
	}
}

func TestRecoverBatchContract(t *testing.T) {
	t.Parallel()

	effective, err := json.Marshal(map[string]string{"input_file_id": "file-upstream-1", "endpoint": "/v1/responses"})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := json.Marshal(map[string]any{
		"version":   DurableContractVersion,
		"effective": effective,
		"binding":   "model-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":"batch_1","status":"in_progress"}`)

	merged, err := recoverBatchContract(staged, "file-upstream-1", body)
	if err != nil {
		t.Fatalf("recoverBatchContract valid: %v", err)
	}
	var doc struct {
		Version string `json:"version"`
		Binding string `json:"binding"`
		Result  []byte `json:"result"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("unmarshal merged contract: %v", err)
	}
	if doc.Version != DurableContractVersion || doc.Binding != "model-a" || !bytes.Equal(doc.Result, body) {
		t.Fatalf("merged contract = %+v, want version=%q binding=%q result=%q", doc, DurableContractVersion, "model-a", body)
	}

	if _, err := recoverBatchContract(staged, "file-other", body); !errors.Is(err, ErrContract) {
		t.Errorf("mismatched input_file_id = %v, want ErrContract", err)
	}
	if _, err := recoverBatchContract([]byte(`not json`), "file-upstream-1", body); err == nil {
		t.Errorf("invalid staged JSON succeeded, want error")
	}
	if _, err := recoverBatchContract([]byte(`{"effective":123}`), "file-upstream-1", body); err == nil {
		t.Errorf("non-bytes effective field succeeded, want error")
	}
	badEffective, err := json.Marshal(map[string]any{"effective": []byte("{bad")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recoverBatchContract(badEffective, "file-upstream-1", body); !errors.Is(err, ErrContract) {
		t.Errorf("malformed effective JSON = %v, want ErrContract", err)
	}
}

func TestInteractionState(t *testing.T) {
	t.Parallel()

	for _, status := range []string{"created", StateDeleted, "pending", "queued", "in_progress", "completed", "failed", "cancelled"} {
		if !interactionState(status) {
			t.Errorf("interactionState(%q) = false, want true", status)
		}
	}
	for _, status := range []string{"", "unknown", "CREATED", "running"} {
		if interactionState(status) {
			t.Errorf("interactionState(%q) = true, want false", status)
		}
	}
}

func TestGeminiResultScope(t *testing.T) {
	t.Parallel()

	credA := "cred-a"
	credB := "cred-b"
	base := &Resource{
		APIKeyID:   "key-1",
		ProviderID: "prov-1",
		SlotID:     "slot-1",
	}
	output := *base
	output.UpstreamID = "files/abc#output"
	errView := *base
	errView.UpstreamID = "files/abc#error"

	scopeNil := geminiResultScope(&output, "files/abc")
	if scopeNil != geminiResultScope(&errView, "files/abc") {
		t.Fatalf("sibling views produced different scopes: %q vs %q", scopeNil, geminiResultScope(&errView, "files/abc"))
	}

	withCredA := *base
	withCredA.CredentialID = &credA
	withCredB := *base
	withCredB.CredentialID = &credB

	scopeA := geminiResultScope(&withCredA, "files/abc")
	scopeB := geminiResultScope(&withCredB, "files/abc")
	scopeOtherFile := geminiResultScope(&withCredA, "files/def")
	if scopeNil == scopeA || scopeA == scopeB || scopeA == scopeOtherFile {
		t.Fatalf("distinct scopes collided: nil=%q credA=%q credB=%q otherFile=%q", scopeNil, scopeA, scopeB, scopeOtherFile)
	}
}

func TestExpiryFailures(t *testing.T) {
	t.Parallel()

	first := errors.New("first accounting refusal")
	second := errors.New("second accounting refusal")

	var failures ExpiryFailures
	failures.record(first)
	failures.record(second)

	if failures.Count != 2 {
		t.Fatalf("Count = %d, want 2", failures.Count)
	}
	if !errors.Is(&failures, first) {
		t.Fatalf("Unwrap did not match first error: %v", failures.Unwrap())
	}
	if errors.Is(&failures, second) {
		t.Fatalf("Unwrap unexpectedly matched second error")
	}
	if got := failures.Error(); got != "2 resource expiry owners refused accounting" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestBatchUpdateRegressesAndMatchPinnedTarget(t *testing.T) {
	t.Parallel()

	if batchUpdateRegresses(KindStrictFile, "completed", "in_progress") {
		t.Errorf("strict_file should never be treated as regressing")
	}
	if !batchUpdateRegresses(KindStrictBatch, "completed", "in_progress") {
		t.Errorf("terminal strict_batch must not regress to in_progress")
	}
	if !batchUpdateRegresses(KindStrictBatch, "completed", "failed") {
		t.Errorf("terminal strict_batch must not transition to another terminal state")
	}
	if !batchUpdateRegresses(KindStrictBatch, "finalizing", "in_progress") {
		t.Errorf("strict_batch must not move backward from finalizing to in_progress")
	}
	if batchUpdateRegresses(KindStrictBatch, "in_progress", "finalizing") {
		t.Errorf("strict_batch forward progress unexpectedly rejected")
	}
	if batchUpdateRegresses(KindStrictBatch, "in_progress", "in_progress") {
		t.Errorf("strict_batch same non-terminal stage unexpectedly rejected")
	}

	targets := []routing.Target{
		{ID: "t1", ProviderID: "p1", ProviderModel: "model-1"},
		{ID: "t2", ProviderID: "p1", ProviderModel: ""},
		{ID: "dup", ProviderID: "p1", ProviderModel: "m1"},
		{ID: "dup", ProviderID: "p1", ProviderModel: "m2"},
	}
	got, ok := matchPinnedTarget(targets, "t1", "p1")
	if !ok || got.ProviderModel != "model-1" {
		t.Fatalf("matchPinnedTarget(t1, p1) = (%+v, %v), want model-1", got, ok)
	}
	for _, tc := range []struct {
		targetID   string
		providerID string
	}{
		{"", "p1"},
		{"t1", "p2"},
		{"t2", "p1"},
		{"missing", "p1"},
		{"dup", "p1"},
	} {
		if _, ok := matchPinnedTarget(targets, tc.targetID, tc.providerID); ok {
			t.Errorf("matchPinnedTarget(%q, %q) = true, want false", tc.targetID, tc.providerID)
		}
	}
}
