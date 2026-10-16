package usage

import (
	"io"
	"log/slog"
	"time"
)

func ingestTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ingestSampleEvent() Event {
	observed := time.Now().UTC()
	provider := "1a2b3c4d-0000-7000-8000-000000000001"
	model := "mock-model"
	status := 200
	return Event{
		EventID: "1a2b3c4d-0000-7000-8000-00000000000a", RequestID: "1a2b3c4d-0000-7000-8000-00000000000b",
		RuntimeGenerationID: "1a2b3c4d-0000-7000-8000-00000000000c",
		APIKeyID:            "1a2b3c4d-0000-7000-8000-00000000000d",
		ProviderID:          &provider, RouteSlug: "team-chat", UpstreamModel: &model,
		Operation: "generation", Surface: "openai",
		RequestStartedAt: observed.Add(-10 * time.Millisecond), RequestCompletedAt: observed,
		ObservedAt: observed, StatusCode: &status, Committed: true, LatencyMS: 10,
		UsageComplete: true,
	}
}

// Draining twice must not close the channel twice or double count.

// Emit counts an event only after the writer may already have delivered it.

// A cancelled context stops the writer before it takes anything: every
// buffered event is accounted for as lost rather than forgotten.
