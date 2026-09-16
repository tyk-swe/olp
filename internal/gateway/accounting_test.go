package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/usage"
)

// accountingID is a fresh identifier for a fixture field that must be one.
func accountingID(t *testing.T) string {
	t.Helper()
	return uuid.Must(uuid.NewV7()).String()
}

// accountingEnvelope is a streamed request that failed over once and then
// succeeded, the shape most of the accounting pipeline sees.
func accountingEnvelope(t *testing.T) Envelope {
	t.Helper()
	start := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	firstByte, attemptByte := 120*time.Millisecond, 90*time.Millisecond
	reasoning := int64(2)
	retryAfter := 2 * time.Second
	provider, revision := accountingID(t), accountingID(t)
	return Envelope{
		RequestID:           "client-named-request",
		AccountingID:        accountingID(t),
		Actor:               "api_key",
		KeyID:               accountingID(t),
		Route:               "team-chat",
		RouteRevisionID:     accountingID(t),
		ReleaseSequence:     7,
		Family:              string(openai.FamilyChat),
		Mode:                "streaming",
		Operation:           operationGeneration,
		Surface:             surfaceOpenAI,
		Outcome:             "success",
		Status:              200,
		Committed:           true,
		StartedAt:           start,
		CompletedAt:         start.Add(400 * time.Millisecond),
		Duration:            400 * time.Millisecond,
		FirstByte:           &firstByte,
		RuntimeGenerationID: accountingID(t),
		Usage:               &openai.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10},
		Attempts: []AttemptFact{
			{
				Ordinal: 1, TargetID: accountingID(t), ProviderID: provider,
				ProviderRevisionID: revision, UpstreamModel: "fixture-model",
				SlotID: accountingID(t), CredentialID: accountingID(t), CredentialVersion: 2,
				Mode: "streaming", Status: 429, Class: classRateLimit,
				StartedAt: start, Duration: 15 * time.Millisecond, RetryAfter: &retryAfter,
				UsageComplete: true,
			},
			{
				Ordinal: 2, TargetID: accountingID(t), ProviderID: provider,
				ProviderRevisionID: revision, UpstreamModel: "fixture-model",
				SlotID: accountingID(t), CredentialID: accountingID(t), CredentialVersion: 1,
				Mode: "streaming", Status: 200, Class: classSuccess, Committed: true,
				StartedAt: start.Add(20 * time.Millisecond), Duration: 380 * time.Millisecond,
				FirstByte:     &attemptByte,
				Usage:         &openai.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10, ReasoningTokens: &reasoning},
				UsageObserved: true, UsageComplete: true,
			},
		},
	}
}

func TestAccountingEventIsAccountable(t *testing.T) {
	envelope := accountingEnvelope(t)
	firstOutput := 100 * time.Millisecond
	envelope.Attempts[1].FirstOutput = &firstOutput
	event := accountingEvent(envelope)
	if event == nil {
		t.Fatal("no event for a served request")
	}
	if _, err := usage.Validate(event); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := uuid.Parse(event.EventID); err != nil {
		t.Fatalf("event id %q is not an identifier the pipeline can store: %v", event.EventID, err)
	}
	if event.RequestID != envelope.AccountingID {
		t.Fatalf("request id = %q, want the durable identity %q", event.RequestID, envelope.AccountingID)
	}
	if event.APIKeyID != envelope.KeyID || event.RuntimeGenerationID != envelope.RuntimeGenerationID || event.RouteSlug != envelope.Route {
		t.Fatalf("ownership = %s/%s/%s", event.APIKeyID, event.RuntimeGenerationID, event.RouteSlug)
	}
	if event.Operation != operationGeneration || event.Surface != surfaceOpenAI {
		t.Fatalf("operation/surface = %s/%s", event.Operation, event.Surface)
	}
	final := envelope.Attempts[1]
	if event.ProviderID == nil || *event.ProviderID != final.ProviderID {
		t.Fatalf("provider = %v, want %s", event.ProviderID, final.ProviderID)
	}
	if event.UpstreamModel == nil || *event.UpstreamModel != final.UpstreamModel {
		t.Fatalf("upstream model = %v", event.UpstreamModel)
	}
	if event.StatusCode == nil || *event.StatusCode != 200 || event.ErrorClass != nil {
		t.Fatalf("status = %v error class = %v", event.StatusCode, event.ErrorClass)
	}
	if event.LatencyMS != 400 || event.FirstByteMS == nil || *event.FirstByteMS != 120 {
		t.Fatalf("latency = %d first byte = %v", event.LatencyMS, event.FirstByteMS)
	}
	if !event.UsageComplete || event.Unpriced {
		t.Fatalf("usage complete = %v unpriced = %v", event.UsageComplete, event.Unpriced)
	}
	if event.InputTokens == nil || *event.InputTokens != 4 || event.OutputTokens == nil || *event.OutputTokens != 6 {
		t.Fatalf("tokens = %v/%v", event.InputTokens, event.OutputTokens)
	}
	if len(event.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(event.Attempts))
	}
	rejected, served := event.Attempts[0], event.Attempts[1]
	if rejected.Ordinal != 1 || served.Ordinal != 2 {
		t.Fatalf("ordinals = %d,%d", rejected.Ordinal, served.Ordinal)
	}
	if rejected.ErrorClass == nil || *rejected.ErrorClass != classRateLimit {
		t.Fatalf("first attempt error class = %v", rejected.ErrorClass)
	}
	if rejected.Usage == nil || rejected.Usage.Observed || !rejected.Usage.Complete || rejected.Usage.BillingUncertain {
		t.Fatalf("first attempt usage = %+v", rejected.Usage)
	}
	if rejected.Usage.InputTokens != nil || rejected.Usage.OutputTokens != nil {
		t.Fatal("an attempt that reported no usage carries no tokens")
	}
	if served.Usage == nil || !served.Usage.Observed || served.Usage.InputTokens == nil || *served.Usage.InputTokens != 4 {
		t.Fatalf("serving attempt usage = %+v", served.Usage)
	}
	if served.CompletedAt.Sub(served.StartedAt) != 380*time.Millisecond || served.LatencyMS != 380 {
		t.Fatalf("serving attempt window = %s latency = %d", served.CompletedAt.Sub(served.StartedAt), served.LatencyMS)
	}
	routing := served.Routing
	if routing == nil || routing.ProviderRevisionID != envelope.Attempts[1].ProviderRevisionID {
		t.Fatalf("routing = %+v", routing)
	}
	if routing.CredentialSlotID == nil || *routing.CredentialSlotID != envelope.Attempts[1].SlotID {
		t.Fatalf("credential slot = %v", routing.CredentialSlotID)
	}
	if routing.CredentialVersionID == nil || *routing.CredentialVersionID != envelope.Attempts[1].CredentialID {
		t.Fatalf("credential version = %v", routing.CredentialVersionID)
	}
	if routing.Mode == nil || *routing.Mode != "streaming" {
		t.Fatalf("mode = %v", routing.Mode)
	}
	// Meaningful output is measured independently of protocol setup frames.
	if routing.FirstOutputMS == nil || *routing.FirstOutputMS != 100 {
		t.Fatalf("first output = %v", routing.FirstOutputMS)
	}
	if event.Attempts[0].Routing == nil || event.Attempts[0].Routing.FirstOutputMS != nil {
		t.Fatal("an attempt that committed nothing produced no output")
	}
	// Streaming throughput is measured against what the client actually
	// received: the output the provider billed, less the reasoning it kept to
	// itself. Without it the console has no denominator to divide by.
	if routing.StreamedOutputTokens == nil || *routing.StreamedOutputTokens != 4 {
		t.Fatalf("streamed output tokens = %v, want the six billed less the two reasoned", routing.StreamedOutputTokens)
	}
	if event.Attempts[0].Routing.StreamedOutputTokens != nil {
		t.Fatal("an attempt that reported no usage streamed no tokens")
	}
}

func TestAccountingEventWithoutAttempts(t *testing.T) {
	envelope := accountingEnvelope(t)
	envelope.Attempts = nil
	envelope.Usage = nil
	envelope.FirstByte = nil
	envelope.Outcome, envelope.Status, envelope.ErrorClass, envelope.Committed = "failure", 403, "route_forbidden", false
	event := accountingEvent(envelope)
	if event == nil {
		t.Fatal("no event for a rejected request")
	}
	if _, err := usage.Validate(event); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if event.ProviderID != nil || event.UpstreamModel != nil || event.FirstByteMS != nil {
		t.Fatalf("a request that reached no provider described one: %+v", event)
	}
	if event.Committed || event.UsageComplete || event.InputTokens != nil {
		t.Fatalf("a request that reached no provider carried usage: %+v", event)
	}
	if event.ErrorClass == nil || *event.ErrorClass != "route_forbidden" || event.StatusCode == nil || *event.StatusCode != 403 {
		t.Fatalf("failure = %v %v", event.StatusCode, event.ErrorClass)
	}
}

func TestAccountingEventCancelledRequest(t *testing.T) {
	envelope := accountingEnvelope(t)
	envelope.Outcome, envelope.Status, envelope.ErrorClass = "cancelled", 0, "client_cancelled"
	event := accountingEvent(envelope)
	if event == nil {
		t.Fatal("no event for a cancelled request")
	}
	if _, err := usage.Validate(event); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if event.StatusCode != nil {
		t.Fatalf("status = %v, want none for a response the client never took", event.StatusCode)
	}
}

func TestAccountingSkipsRequestsWithoutAnOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(e *Envelope)
	}{
		{"playground traffic has no key", func(e *Envelope) { e.KeyID = "" }},
		{"a request that resolved no route", func(e *Envelope) { e.Route = "" }},
		{"a request that pinned no generation", func(e *Envelope) { e.RuntimeGenerationID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := accountingEnvelope(t)
			tc.apply(&envelope)
			if event := accountingEvent(envelope); event != nil {
				t.Fatalf("event emitted for an unaccountable request: %+v", event)
			}
		})
	}
}

func TestAccountingSinkDropsInvalidEvents(t *testing.T) {
	emitter := usage.NewEmitter(4)
	next := &capture{}
	sink := &AccountingSink{Emitter: emitter, Next: next}
	good := accountingEnvelope(t)
	sink.Terminal(good)
	if got := emitter.Snapshot().Accepted; got != 1 {
		t.Fatalf("accepted = %d, want 1", got)
	}
	// A request that ended before it started cannot be reconciled with
	// anything, and one bad event must not travel with the good ones.
	broken := accountingEnvelope(t)
	broken.CompletedAt = broken.StartedAt.Add(-time.Second)
	sink.Terminal(broken)
	if got := emitter.Snapshot().Accepted; got != 1 {
		t.Fatalf("accepted = %d after an invalid event, want 1", got)
	}
	sink.Terminal(Envelope{RequestID: "no-key"})
	if got := emitter.Snapshot().Accepted; got != 1 {
		t.Fatalf("accepted = %d after an unaccountable request, want 1", got)
	}
	if got := emitter.Snapshot().Dropped; got != 1 {
		t.Fatalf("invalid event loss = %d, want 1", got)
	}
	if len(next.envs) != 3 {
		t.Fatalf("diagnostic envelopes = %d, want all three", len(next.envs))
	}
}

// TestAccountingEventFromServedRequests maps the envelopes real traffic
// produces: the fixtures above are only as good as their resemblance to what
// the gateway actually emits.
func TestAccountingEventFromServedRequests(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_error"}}`))
	resp, body := h.chat(fullKey, map[string]string{"X-Request-Id": "client-named"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	served := h.sink.last(t)
	event := accountingEvent(served)
	if event == nil {
		t.Fatal("a served request produced no accounting event")
	}
	if _, err := usage.Validate(event); err != nil {
		t.Fatalf("served request is not accountable: %v", err)
	}
	if !event.Committed || !event.Attempts[len(event.Attempts)-1].Committed {
		t.Fatal("delivered unary response was recorded as uncommitted")
	}
	if event.RequestID == served.RequestID {
		t.Error("a caller-named request id became the durable identity")
	}
	if _, err := uuid.Parse(event.RequestID); err != nil {
		t.Errorf("durable request id %q is not an identifier: %v", event.RequestID, err)
	}
	if len(event.Attempts) != 2 {
		t.Fatalf("attempts = %d, want the rejected and the serving attempt", len(event.Attempts))
	}
	if event.Attempts[0].ErrorClass == nil || *event.Attempts[0].ErrorClass != classRateLimit {
		t.Errorf("first attempt error class = %v, want %s", event.Attempts[0].ErrorClass, classRateLimit)
	}
	if event.Attempts[0].Usage.Observed || event.Attempts[0].Usage.InputTokens != nil {
		t.Error("a rejected attempt reported usage")
	}
	if event.UpstreamModel == nil || *event.UpstreamModel != modelB {
		t.Errorf("upstream model = %v, want the model that served", event.UpstreamModel)
	}
	if event.StatusCode == nil || *event.StatusCode != http.StatusOK {
		t.Errorf("status = %v, want 200", event.StatusCode)
	}
	if !event.UsageComplete || event.InputTokens == nil || *event.InputTokens != 2 || event.OutputTokens == nil || *event.OutputTokens != 3 {
		t.Errorf("usage = %v/%v complete=%v, want the tokens the upstream reported", event.InputTokens, event.OutputTokens, event.UsageComplete)
	}
	if event.Attempts[1].Routing == nil || event.Attempts[1].Routing.ProviderRevisionID == "" {
		t.Error("the serving attempt carries no routing provenance")
	}

	// A request the gateway named itself keeps that name in the ledger, so the
	// response header and the stored record agree.
	if _, body := h.chat(fullKey, nil); body == nil {
		t.Fatal("no response body")
	}
	own := accountingEvent(h.sink.last(t))
	if own == nil {
		t.Fatal("no accounting event")
	}
	if own.RequestID != h.sink.last(t).RequestID {
		t.Errorf("durable id %q, request id %q: a gateway-minted id must be kept", own.RequestID, h.sink.last(t).RequestID)
	}
	if _, err := usage.Validate(own); err != nil {
		t.Fatalf("second request is not accountable: %v", err)
	}
}
