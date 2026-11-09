package batches

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

type ResultKind string

const (
	ResultSucceeded ResultKind = "succeeded"
	ResultErrored   ResultKind = "errored"
	ResultCancelled ResultKind = "cancelled"
	ResultExpired   ResultKind = "expired"
	ResultAggregate ResultKind = "aggregate"
	ResultGap       ResultKind = "gap"
)

var observedKinds = []ResultKind{ResultSucceeded, ResultErrored, ResultCancelled, ResultExpired, ResultAggregate}

func (k ResultKind) known() bool { return k == ResultGap || slices.Contains(observedKinds, k) }

func (k ResultKind) successful() bool { return k == ResultSucceeded || k == ResultAggregate }

const maxErrorClassBytes = 120

func (c ErrorClass) stored() string { return string(c)[:min(len(c), maxErrorClassBytes)] }

type Result struct {
	CustomID   string
	Kind       ResultKind
	StatusCode *int
	Usage      usage.AttemptUsage
	ErrorClass ErrorClass
	Evidence   []byte
}

type ResultCounts map[ResultKind]int64

func (c ResultCounts) Observed() int64 {
	var count int64
	for _, kind := range observedKinds {
		count += c[kind]
	}
	return count
}

func CustomIDDigest(auth *secrets.AuthKey, customID string) []byte {
	return auth.Digest(secrets.BatchLineDigest, customID)
}

var eventNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/tyk-swe/olp/batch-settlement/v1"))

func ResultEvent(template *usage.Event, resourceID uuid.UUID, customIDDigest []byte, result Result, now time.Time) (*usage.Event, error) {
	if template == nil || len(template.Attempts) != 1 || resourceID == uuid.Nil || len(customIDDigest) == 0 {
		return nil, errors.New("batch result identity is incomplete")
	}
	if !result.Kind.known() {
		return nil, errors.New("batch result kind is unknown")
	}
	evidence, err := resultUsage(template.Attempts[0], result)
	if err != nil {
		return nil, err
	}
	statusCode := result.StatusCode
	if statusCode == nil && result.Kind.successful() {
		statusCode = new(http.StatusOK)
	}
	errorClass := resultErrorClass(result)
	name := slices.Concat(resourceID[:], customIDDigest, []byte(result.Kind))
	eventID := uuid.NewSHA1(eventNamespace, name).String()

	attempt := template.Attempts[0]
	attempt.ID = uuid.NewSHA1(eventNamespace, slices.Concat(name, []byte("attempt"))).String()
	attempt.CompletedAt = now
	attempt.StatusCode = statusCode
	attempt.ErrorClass = errorClass
	attempt.Committed = true
	attempt.LatencyMS = 0
	attempt.FirstByteMS = nil
	attempt.Usage = &evidence

	event := *template
	event.EventID = eventID
	event.RequestID = eventID
	event.Version = usage.WireVersion
	event.RequestStartedAt = now
	event.RequestCompletedAt = now
	event.ObservedAt = now
	event.StatusCode = statusCode
	event.ErrorClass = errorClass
	event.Committed = true
	event.LatencyMS = 0
	event.FirstByteMS = nil
	event.Attempts = []usage.Attempt{attempt}
	event.UsageComplete = evidence.Complete
	event.InputTokens = evidence.InputTokens
	event.OutputTokens = evidence.OutputTokens
	event.CachedInputTokens = evidence.CachedInputTokens
	event.CacheWriteInputTokens = evidence.CacheWriteInputTokens
	event.CacheWrite5MInputTokens = evidence.CacheWrite5MInputTokens
	event.CacheWrite1HInputTokens = evidence.CacheWrite1HInputTokens
	event.AudioInputTokens = evidence.AudioInputTokens
	event.CachedAudioInputTokens = evidence.CachedAudioInputTokens
	event.AudioOutputTokens = evidence.AudioOutputTokens
	event.ImageInputTokens = evidence.ImageInputTokens
	event.CachedImageInputTokens = evidence.CachedImageInputTokens
	event.ImageOutputTokens = evidence.ImageOutputTokens
	event.MediaUnits = evidence.MediaUnits
	event.ToolCalls = evidence.ToolCalls
	event.Batch = true
	if _, err := usage.Validate(&event); err != nil {
		return nil, err
	}
	return &event, nil
}

func resultErrorClass(result Result) *string {
	if result.Kind.successful() {
		return nil
	}
	if result.ErrorClass == "" {
		return new(ErrorClass("batch_" + string(result.Kind)).stored())
	}
	return new(result.ErrorClass.stored())
}

func resultUsage(attempt usage.Attempt, result Result) (usage.AttemptUsage, error) {
	evidence := result.Usage
	evidence.Batch = true
	if result.Kind != ResultAggregate {
		return evidence, nil
	}
	uncertain, err := aggregatePricingUncertain(attempt)
	if err != nil {
		return usage.AttemptUsage{}, err
	}
	if uncertain {
		evidence.Complete = false
		evidence.BillingUncertain = true
	}
	return evidence, nil
}

func aggregatePricingUncertain(attempt usage.Attempt) (bool, error) {
	if attempt.Routing == nil || attempt.Routing.Policy == nil {
		return true, nil
	}
	var policy struct {
		Uncertain *bool `json:"batch_aggregate_pricing_uncertain"`
	}
	if err := json.Unmarshal(*attempt.Routing.Policy, &policy); err != nil {
		return false, errors.New("batch aggregate pricing provenance is malformed")
	}
	return policy.Uncertain == nil || *policy.Uncertain, nil
}
