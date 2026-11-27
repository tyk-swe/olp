package manage

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func TestListCursorRoundTripAndMalformed(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("UTC+2", 2*3600)
	wantTime := time.Date(2026, 4, 5, 14, 30, 15, 123456789, loc)
	wantID := uuid.MustParse("019605f0-1234-7abc-8def-0123456789ab")

	encoded := listCursor{at: wantTime, id: wantID}.encode()
	decoded, err := decodeListCursor(encoded)
	if err != nil {
		t.Fatalf("decodeListCursor(%q) unexpected error: %v", encoded, err)
	}
	if !decoded.at.Equal(wantTime) || decoded.id != wantID {
		t.Fatalf("decodeListCursor(%q) = %+v, want at=%s id=%s", encoded, decoded, wantTime.UTC(), wantID)
	}
	if encoded != decoded.encode() {
		t.Fatalf("cursor encode not canonical: %q != %q", encoded, decoded.encode())
	}

	malformed := []string{
		"",
		"no-separator",
		"not-a-timestamp|" + wantID.String(),
		wantTime.UTC().Format(time.RFC3339Nano) + "|not-a-uuid",
		wantTime.UTC().Format(time.RFC3339Nano) + "|",
		"|" + wantID.String(),
		wantTime.UTC().Format(time.RFC3339Nano) + "|" + wantID.String() + "|extra",
		wantID.String() + "|" + wantTime.UTC().Format(time.RFC3339Nano),
	}
	for _, raw := range malformed {
		if got, err := decodeListCursor(raw); err == nil {
			t.Errorf("decodeListCursor(%q) = %+v, want error", raw, got)
		}
	}
}

func TestContractsMatchesRetainedAPI(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/provider-resource-contracts", nil)
	reply, err := New(nil).contracts(req, authz.Principal{})
	if err != nil {
		t.Fatalf("contracts returned error: %v", err)
	}
	if reply.Status != http.StatusOK {
		t.Fatalf("contracts status = %d, want 200", reply.Status)
	}
	body, ok := reply.Body.(contract.ProviderResourceContractList)
	if !ok {
		t.Fatalf("contracts body type = %T, want contract.ProviderResourceContractList", reply.Body)
	}
	if body.Revision != retainedapi.Revision {
		t.Fatalf("revision = %q, want %q", body.Revision, retainedapi.Revision)
	}
	families := retainedapi.Families()
	if len(body.Items) != len(families) {
		t.Fatalf("len(items) = %d, want %d", len(body.Items), len(families))
	}
	for i, family := range families {
		got := body.Items[i]
		wantImplemented := slices.ContainsFunc(family.Providers, func(provider string) bool {
			return retainedapi.Supports(provider, family.Operation, family.Surface, "unary")
		})
		if got.Operation != family.Operation || got.Surface != family.Surface || got.Kind != family.Kind ||
			got.Path != family.Path || got.Metered != family.Metered || got.Implemented != wantImplemented ||
			!slices.Equal(got.Methods, family.Methods) || !slices.Equal(got.Providers, family.Providers) {
			t.Errorf("item[%d] = %+v, does not match family %+v", i, got, family)
		}
	}
	if body.Endpoints == nil {
		t.Fatal("endpoints pointer is nil")
	}
	wantEndpoints := retainedapi.Endpoints()
	if len(*body.Endpoints) != len(wantEndpoints) {
		t.Fatalf("len(endpoints) = %d, want %d", len(*body.Endpoints), len(wantEndpoints))
	}
	for i, want := range wantEndpoints {
		got := (*body.Endpoints)[i]
		if got.Ancestors == nil {
			t.Errorf("endpoint[%d] (%s) has nil ancestors slice", i, got.Pattern)
		}
		if got.Operation != want.Operation || got.Surface != want.Surface || got.Kind != want.Kind ||
			got.Action != want.Action || got.Method != want.Method || got.Pattern != want.Pattern ||
			got.Metered != want.Metered || !slices.Equal(got.Ancestors, want.Ancestors) {
			t.Errorf("endpoint[%d] = %+v, does not match %+v", i, got, want)
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal contracts: %v", err)
	}
	var wire struct {
		Revision  string            `json:"revision"`
		Items     []json.RawMessage `json:"items"`
		Endpoints []struct {
			Ancestors json.RawMessage `json:"ancestors"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal contracts wire json: %v", err)
	}
	if wire.Revision != retainedapi.Revision || len(wire.Items) != len(families) || len(wire.Endpoints) != len(wantEndpoints) {
		t.Fatalf("wire payload mismatch: revision=%q items=%d endpoints=%d", wire.Revision, len(wire.Items), len(wire.Endpoints))
	}
	for i, ep := range wire.Endpoints {
		if string(ep.Ancestors) == "null" {
			t.Errorf("wire endpoint[%d] serialized ancestors as null", i)
		}
	}
}

func TestRetainedEvidenceLines(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	t.Run("single-component operations", func(t *testing.T) {
		t.Parallel()
		invocations := int64(4)
		inputTokens := int64(100)
		outputTokens := int64(50)
		cachedTokens := int64(25)
		cases := []struct {
			operation string
			wantComp  string
			input     contract.RetainedUsageEvidenceRequest
		}{
			{
				operation: "fine_tuning",
				wantComp:  "training",
				input: contract.RetainedUsageEvidenceRequest{
					Start: start, End: end, TrainingTokens: nullable.NewNullableWithValue(int64(4096)),
				},
			},
			{
				operation: "vector_store",
				wantComp:  "storage",
				input: contract.RetainedUsageEvidenceRequest{
					Start: start, End: end, StorageByteSeconds: nullable.NewNullableWithValue("819200"),
				},
			},
			{
				operation: "context_cache",
				wantComp:  "cache_storage",
				input: contract.RetainedUsageEvidenceRequest{
					Start: start, End: end, CacheTokenSeconds: nullable.NewNullableWithValue("12345.5"),
				},
			},
			{
				operation: "eval",
				wantComp:  "generation",
				input: contract.RetainedUsageEvidenceRequest{
					Start: start, End: end,
					InvocationCount:   &invocations,
					InputTokens:       &inputTokens,
					OutputTokens:      &outputTokens,
					CachedInputTokens: &cachedTokens,
				},
			},
		}
		for _, tc := range cases {
			lines := retainedEvidenceLines(tc.operation, "pinned-model", tc.input)
			if len(lines) != 1 {
				t.Fatalf("operation %q produced %d lines, want 1", tc.operation, len(lines))
			}
			line := lines[0]
			if line.Component != tc.wantComp || line.Model != "pinned-model" || line.MemoryTier != "" ||
				!line.Start.Equal(start) || !line.End.Equal(end) {
				t.Fatalf("operation %q line = %+v", tc.operation, line)
			}
			switch tc.operation {
			case "fine_tuning":
				if line.Usage.TrainingTokens == nil || *line.Usage.TrainingTokens != 4096 {
					t.Fatalf("fine_tuning training tokens = %v, want 4096", line.Usage.TrainingTokens)
				}
			case "vector_store":
				if line.Usage.StorageByteSeconds == nil || *line.Usage.StorageByteSeconds != "819200" {
					t.Fatalf("vector_store storage_byte_seconds = %v, want 819200", line.Usage.StorageByteSeconds)
				}
			case "context_cache":
				if line.Usage.CacheTokenSeconds == nil || *line.Usage.CacheTokenSeconds != "12345.5" {
					t.Fatalf("context_cache cache_token_seconds = %v, want 12345.5", line.Usage.CacheTokenSeconds)
				}
			case "eval":
				if line.Usage.InvocationCount == nil || *line.Usage.InvocationCount != invocations ||
					line.Usage.InputTokens == nil || *line.Usage.InputTokens != inputTokens ||
					line.Usage.OutputTokens == nil || *line.Usage.OutputTokens != outputTokens ||
					line.Usage.CachedInputTokens == nil || *line.Usage.CachedInputTokens != cachedTokens {
					t.Fatalf("eval usage = %+v", line.Usage)
				}
			}
		}
	})

	t.Run("container splits multiple tiers deterministically", func(t *testing.T) {
		t.Parallel()
		sessions := map[string]int64{"4g": 3, "1g": 2, "16g": 1}
		input := contract.RetainedUsageEvidenceRequest{
			Start:             start,
			End:               end,
			ContainerSessions: nullable.NewNullableWithValue(sessions),
		}
		lines := retainedEvidenceLines("container", "container-model", input)
		if len(lines) != 3 {
			t.Fatalf("container produced %d lines, want 3", len(lines))
		}
		wantOrder := []struct {
			tier  string
			count int64
		}{
			{tier: "16g", count: 1},
			{tier: "1g", count: 2},
			{tier: "4g", count: 3},
		}
		for i, want := range wantOrder {
			line := lines[i]
			if line.Component != "container" || line.Model != "container-model" || line.MemoryTier != want.tier {
				t.Fatalf("line[%d] = %+v, want tier %q", i, line, want.tier)
			}
			if len(line.Usage.ContainerSessions) != 1 || line.Usage.ContainerSessions[want.tier] != want.count {
				t.Fatalf("line[%d] sessions = %v, want {%q:%d}", i, line.Usage.ContainerSessions, want.tier, want.count)
			}
		}
		if len(sessions) != 3 || sessions["4g"] != 3 || sessions["1g"] != 2 || sessions["16g"] != 1 {
			t.Fatalf("caller sessions map was mutated: %v", sessions)
		}
	})

	t.Run("empty container sessions and unknown operations return no lines", func(t *testing.T) {
		t.Parallel()
		if lines := retainedEvidenceLines("container", "m", contract.RetainedUsageEvidenceRequest{Start: start, End: end}); len(lines) != 0 {
			t.Fatalf("unspecified container sessions produced %d lines, want 0", len(lines))
		}
		if lines := retainedEvidenceLines("container", "m", contract.RetainedUsageEvidenceRequest{
			Start: start, End: end, ContainerSessions: nullable.NewNullNullable[map[string]int64](),
		}); len(lines) != 0 {
			t.Fatalf("null container sessions produced %d lines, want 0", len(lines))
		}
		if lines := retainedEvidenceLines("conversation", "m", contract.RetainedUsageEvidenceRequest{Start: start, End: end}); len(lines) != 0 {
			t.Fatalf("unmetered operation produced %d lines, want 0", len(lines))
		}
	})
}

func TestListQueryValidation(t *testing.T) {
	t.Parallel()
	api := New(nil)
	principal := authz.Principal{AllProjects: false, Projects: map[string]string{uuid.NewString(): "viewer"}}

	invalidCases := []struct {
		name       string
		params     url.Values
		wantStatus int
		wantCode   string
		wantField  string
	}{
		{
			name:       "invalid kind",
			params:     url.Values{"kind": {"not_a_real_kind"}},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
			wantField:  "kind",
		},
		{
			name:       "invalid api_key_id",
			params:     url.Values{"api_key_id": {"not-a-uuid"}},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
			wantField:  "api_key_id",
		},
		{
			name:       "invalid provider_id",
			params:     url.Values{"provider_id": {"12345"}},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
			wantField:  "provider_id",
		},
		{
			name:       "limit zero",
			params:     url.Values{"limit": {"0"}},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
			wantField:  "limit",
		},
		{
			name:       "limit over max",
			params:     url.Values{"limit": {"201"}},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
			wantField:  "limit",
		},
		{
			name:       "limit non-integer",
			params:     url.Values{"limit": {"ten"}},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "validation_failed",
			wantField:  "limit",
		},
		{
			name:       "malformed cursor",
			params:     url.Values{"cursor": {"bad-cursor"}},
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_cursor",
		},
	}
	for _, tc := range invalidCases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/provider-resources?"+tc.params.Encode(), nil)
		_, err := api.list(req, principal)
		problem, ok := errors.AsType[*httpkit.Problem](err)
		if !ok {
			t.Fatalf("%s: err = %v, want *httpkit.Problem", tc.name, err)
		}
		if problem.Status != tc.wantStatus || problem.Code != tc.wantCode || problem.Field != tc.wantField {
			t.Errorf("%s: problem = %+v, want status=%d code=%q field=%q", tc.name, problem, tc.wantStatus, tc.wantCode, tc.wantField)
		}
	}

	apiKeyID := uuid.New()
	providerID := uuid.New()
	cursorID := uuid.New()
	cursorTime := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	validParams := url.Values{
		"kind":        {string(contract.ListProviderResourcesParamsKindFineTuningJob)},
		"api_key_id":  {apiKeyID.String()},
		"provider_id": {providerID.String()},
		"route":       {"my-route"},
		"state":       {"running"},
		"limit":       {"25"},
		"cursor":      {listCursor{at: cursorTime, id: cursorID}.encode()},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/provider-resources?"+validParams.Encode(), nil)
	got, err := parseListQuery(req, principal)
	if err != nil {
		t.Fatalf("parseListQuery valid query failed: %v", err)
	}
	if got.limit != 25 || got.filters.kind == nil || *got.filters.kind != "fine_tuning_job" ||
		got.filters.apiKeyID == nil || *got.filters.apiKeyID != apiKeyID ||
		got.filters.providerID == nil || *got.filters.providerID != providerID ||
		got.filters.route == nil || *got.filters.route != "my-route" ||
		got.filters.state == nil || *got.filters.state != "running" ||
		got.cursor == nil || !got.cursor.at.Equal(cursorTime) || got.cursor.id != cursorID {
		t.Fatalf("parseListQuery parsed unexpected values: %+v", got)
	}
}

func TestResolveEvidenceModel(t *testing.T) {
	t.Parallel()
	single := []byte(`{"only-model":{"version":1}}`)
	multi := []byte(`{"model-a":{"version":1},"model-b":{"version":1}}`)

	model, err := resolveEvidenceModel(single, "")
	if err != nil || model != "only-model" {
		t.Fatalf("single-model default = (%q, %v), want (only-model, nil)", model, err)
	}
	model, err = resolveEvidenceModel(multi, "model-b")
	if err != nil || model != "model-b" {
		t.Fatalf("multi-model explicit = (%q, %v), want (model-b, nil)", model, err)
	}
	for _, tc := range []struct {
		name      string
		templates []byte
		requested string
	}{
		{name: "multi-model omitted", templates: multi, requested: ""},
		{name: "unpinned model", templates: single, requested: "other-model"},
	} {
		_, err := resolveEvidenceModel(tc.templates, tc.requested)
		problem, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || problem.Status != http.StatusUnprocessableEntity || problem.Field != "model" {
			t.Errorf("%s: err = %v, want 422 model problem", tc.name, err)
		}
	}
}

func TestToProviderResourceItemWireJSON(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	updated := created.Add(5 * time.Minute)
	expires := created.Add(24 * time.Hour)
	closeAfter := created.Add(48 * time.Hour)
	closedAt := created.Add(12 * time.Hour)
	terminalAt := created.Add(2 * time.Hour)
	nextObs := created.Add(3 * time.Hour)

	t.Run("empty list and minimal record nullable fields", func(t *testing.T) {
		t.Parallel()
		emptyWire, err := json.Marshal(contract.ProviderResourceListResponse{
			Items:      make([]contract.ProviderResourceItem, 0),
			NextCursor: nullableValue[string](nil),
		})
		if err != nil {
			t.Fatalf("marshal empty list: %v", err)
		}
		var emptyFields map[string]json.RawMessage
		if err := json.Unmarshal(emptyWire, &emptyFields); err != nil {
			t.Fatalf("unmarshal empty list: %v", err)
		}
		if string(emptyFields["items"]) != "[]" || string(emptyFields["next_cursor"]) != "null" {
			t.Fatalf("empty list wire = %s, want items=[] next_cursor=null", emptyWire)
		}

		resUUID := uuid.MustParse("019605f0-0001-7000-8000-000000000001")
		keyUUID := uuid.MustParse("019605f0-0002-7000-8000-000000000002")
		provUUID := uuid.MustParse("019605f0-0003-7000-8000-000000000003")
		item := toProviderResourceItem(listRecord{
			uuid:         resUUID,
			kind:         resources.KindFile,
			apiKeyID:     keyUUID,
			apiKeyName:   "test-key",
			route:        "files-route",
			providerID:   provUUID,
			providerName: "test-provider",
			upstreamID:   "file-abc",
			state:        "ready",
			createdAt:    created,
			updatedAt:    updated,
		})
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal minimal item: %v", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("unmarshal minimal item: %v", err)
		}
		if string(fields["expires_at"]) != "null" {
			t.Errorf("minimal expires_at = %s, want null", fields["expires_at"])
		}
		if _, present := fields["settlement"]; present {
			t.Errorf("minimal settlement unexpectedly present: %s", fields["settlement"])
		}
		if _, present := fields["retained_settlement"]; present {
			t.Errorf("minimal retained_settlement unexpectedly present: %s", fields["retained_settlement"])
		}
		var roundTripped contract.ProviderResourceItem
		if err := json.Unmarshal(raw, &roundTripped); err != nil {
			t.Fatalf("round-trip minimal item: %v", err)
		}
		if roundTripped.Id != resources.LocalID(resources.KindFile, resUUID) ||
			roundTripped.ApiKeyId != keyUUID || roundTripped.ProviderId != provUUID {
			t.Fatalf("round-tripped minimal item IDs mismatch: %+v", roundTripped)
		}

		partialSettlement := toProviderResourceItem(listRecord{
			uuid:         resUUID,
			kind:         resources.KindBatch,
			apiKeyID:     keyUUID,
			apiKeyName:   "test-key",
			route:        "batch-route",
			providerID:   provUUID,
			providerName: "test-provider",
			state:        "submitting",
			createdAt:    created,
			updatedAt:    updated,
			settlement: &settlementRecord{
				state:      "submitting",
				closeAfter: closeAfter,
			},
		})
		partialRaw, err := json.Marshal(partialSettlement)
		if err != nil {
			t.Fatalf("marshal partial settlement item: %v", err)
		}
		var partialWire struct {
			Settlement map[string]json.RawMessage `json:"settlement"`
		}
		if err := json.Unmarshal(partialRaw, &partialWire); err != nil {
			t.Fatalf("unmarshal partial settlement wire: %v", err)
		}
		for _, field := range []string{"expected_results", "last_error_class", "closed_at"} {
			if string(partialWire.Settlement[field]) != "null" {
				t.Errorf("partial settlement %s = %s, want null", field, partialWire.Settlement[field])
			}
		}
	})

	t.Run("populated settlement and retained settlement wire JSON", func(t *testing.T) {
		t.Parallel()
		resUUID := uuid.MustParse("019605f0-0011-7000-8000-000000000011")
		keyUUID := uuid.MustParse("019605f0-0012-7000-8000-000000000012")
		provUUID := uuid.MustParse("019605f0-0013-7000-8000-000000000013")
		expected := int64(42)
		lastErr := "upstream_timeout"
		models := []string{"model-a"}
		cursor := listCursor{at: created, id: resUUID}.encode()

		item := toProviderResourceItem(listRecord{
			uuid:          resUUID,
			kind:          "fine_tuning_job",
			apiKeyID:      keyUUID,
			apiKeyName:    "ft-key",
			route:         "ft-route",
			providerID:    provUUID,
			providerName:  "ft-provider",
			upstreamID:    "ftjob-123",
			upstreamModel: "model-a",
			state:         "running",
			expiresAt:     &expires,
			createdAt:     created,
			updatedAt:     updated,
			settlement: &settlementRecord{
				state:            "settling",
				expectedResults:  &expected,
				settledResults:   40,
				uncertainResults: 2,
				lastErrorClass:   &lastErr,
				closeAfter:       closeAfter,
				closedAt:         &closedAt,
			},
			retained: &contract.RetainedSettlementStatus{
				State:             "evidence_missing",
				MissingEvidence:   true,
				RemoteTerminated:  true,
				TerminalAt:        nullable.NewNullableWithValue(terminalAt),
				LastError:         nullable.NewNullableWithValue("usage_evidence_missing"),
				NextObservationAt: nextObs,
				Models:            &models,
			},
		})
		raw, err := json.Marshal(contract.ProviderResourceListResponse{
			Items:      []contract.ProviderResourceItem{item},
			NextCursor: nullableValue(&cursor),
		})
		if err != nil {
			t.Fatalf("marshal populated list: %v", err)
		}
		var decoded contract.ProviderResourceListResponse
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal populated list: %v", err)
		}
		if len(decoded.Items) != 1 || !decoded.NextCursor.IsSpecified() || decoded.NextCursor.MustGet() != cursor {
			t.Fatalf("decoded populated list envelope mismatch: %+v", decoded)
		}
		got := decoded.Items[0]
		if !got.ExpiresAt.IsSpecified() || !got.ExpiresAt.MustGet().Equal(expires) {
			t.Errorf("expires_at = %+v, want %s", got.ExpiresAt, expires)
		}
		if !got.Settlement.IsSpecified() || got.Settlement.IsNull() {
			t.Fatalf("settlement missing in wire JSON: %s", raw)
		}
		st := got.Settlement.MustGet()
		if st.State != "settling" || st.SettledResults != 40 || st.UncertainResults != 2 ||
			!st.ExpectedResults.IsSpecified() || st.ExpectedResults.MustGet() != expected ||
			!st.LastErrorClass.IsSpecified() || st.LastErrorClass.MustGet() != lastErr ||
			!st.CloseAfter.Equal(closeAfter) || !st.ClosedAt.IsSpecified() || !st.ClosedAt.MustGet().Equal(closedAt) {
			t.Errorf("settlement wire mismatch: %+v", st)
		}
		if got.RetainedSettlement == nil || got.RetainedSettlement.State != "evidence_missing" ||
			!got.RetainedSettlement.MissingEvidence || !got.RetainedSettlement.RemoteTerminated ||
			!got.RetainedSettlement.TerminalAt.IsSpecified() || !got.RetainedSettlement.TerminalAt.MustGet().Equal(terminalAt) ||
			!got.RetainedSettlement.LastError.IsSpecified() || got.RetainedSettlement.LastError.MustGet() != "usage_evidence_missing" ||
			!got.RetainedSettlement.NextObservationAt.Equal(nextObs) ||
			got.RetainedSettlement.Models == nil || !slices.Equal(*got.RetainedSettlement.Models, models) {
			t.Errorf("retained_settlement wire mismatch: %+v", got.RetainedSettlement)
		}
	})
}
