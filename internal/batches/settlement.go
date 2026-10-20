package batches

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

type State string

const (
	StateSubmitting        State = "submitting"
	StateSubmissionUnknown State = "submission_unknown"
	StateActive            State = "active"
	StateSettling          State = "settling"
	StateSettled           State = "settled"
	StateUncertain         State = "uncertain"
	StateConflict          State = "conflict"
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

type Settlement struct {
	ResourceID         uuid.UUID
	Kind               string
	Adapter            string
	APIKeyID           string
	RouteSlug          string
	RouteRevisionID    string
	ProviderID         string
	ProviderRevisionID string
	TargetID           string
	SlotID             string
	CredentialID       *string
	UpstreamID         *string
	Surface            string
	SubmissionDigest   []byte
	LineOperation      string
	Template           *usage.Event
	State              State
	ExpectedResults    *int64
	SettledResults     int64
	UncertainResults   int64
	Cursor             []byte
	ClaimID            *uuid.UUID
	ClaimedUntil       *time.Time
	Attempts           int
	NextAttemptAt      time.Time
	LastErrorClass     *string
	CloseAfter         time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ClosedAt           *time.Time
}

type Result struct {
	CustomID   string
	Kind       ResultKind
	StatusCode *int
	Usage      usage.AttemptUsage
	ErrorClass string
	Evidence   []byte
}

type ResultCounts map[ResultKind]int64

func (c ResultCounts) Observed() int64 {
	var count int64
	for _, kind := range []ResultKind{
		ResultSucceeded, ResultErrored, ResultCancelled, ResultExpired, ResultAggregate,
	} {
		count += c[kind]
	}
	return count
}

var eventNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/tyk-swe/olp/batch-settlement/v1"))

func ResultEvent(template *usage.Event, resourceID uuid.UUID, customIDDigest []byte, result Result, now time.Time) (*usage.Event, error) {
	if template == nil || len(template.Attempts) != 1 || resourceID == uuid.Nil || len(customIDDigest) == 0 {
		return nil, errors.New("batch result identity is incomplete")
	}
	if result.Kind != ResultSucceeded && result.Kind != ResultErrored &&
		result.Kind != ResultCancelled && result.Kind != ResultExpired &&
		result.Kind != ResultAggregate && result.Kind != ResultGap {
		return nil, errors.New("batch result kind is unknown")
	}

	name := make([]byte, 0, len(resourceID)+len(customIDDigest)+len(result.Kind))
	name = append(name, resourceID[:]...)
	name = append(name, customIDDigest...)
	name = append(name, result.Kind...)
	eventID := uuid.NewSHA1(eventNamespace, name)
	attemptName := append(append([]byte(nil), name...), []byte("attempt")...)
	attemptID := uuid.NewSHA1(eventNamespace, attemptName)

	event := *template
	event.EventID = eventID.String()
	event.RequestID = eventID.String()
	event.Version = usage.WireVersion
	event.RequestStartedAt = now
	event.RequestCompletedAt = now
	event.ObservedAt = now
	event.StatusCode = cloneInt(result.StatusCode)
	successful := result.Kind == ResultSucceeded || result.Kind == ResultAggregate
	if successful && event.StatusCode == nil {
		event.StatusCode = new(http.StatusOK)
	}
	event.ErrorClass = nil
	event.Committed = true
	event.LatencyMS = 0
	event.FirstByteMS = nil
	event.Attempts = append([]usage.Attempt(nil), template.Attempts...)

	attempt := event.Attempts[0]
	attempt.ID = attemptID.String()
	// Windowed prices are selected at submission; observation may happen days
	// later and must not change the applicable time-of-day or weekday window.
	attempt.StartedAt = template.Attempts[0].StartedAt
	attempt.CompletedAt = now
	attempt.StatusCode = cloneInt(event.StatusCode)
	attempt.ErrorClass = nil
	attempt.Committed = true
	attempt.LatencyMS = 0
	attempt.FirstByteMS = nil
	usageEvidence := result.Usage
	usageEvidence.Batch = true
	attempt.Usage = &usageEvidence
	if !successful {
		errorClass := result.ErrorClass
		if errorClass == "" {
			errorClass = "batch_" + string(result.Kind)
		}
		if len(errorClass) > 120 {
			errorClass = errorClass[:120]
		}
		attempt.ErrorClass = &errorClass
		event.ErrorClass = &errorClass
	}
	event.Attempts[0] = attempt
	event.UsageComplete = usageEvidence.Complete
	event.InputTokens = cloneInt64(usageEvidence.InputTokens)
	event.OutputTokens = cloneInt64(usageEvidence.OutputTokens)
	event.CachedInputTokens = cloneInt64(usageEvidence.CachedInputTokens)
	event.CacheWriteInputTokens = cloneInt64(usageEvidence.CacheWriteInputTokens)
	event.CacheWrite5MInputTokens = cloneInt64(usageEvidence.CacheWrite5MInputTokens)
	event.CacheWrite1HInputTokens = cloneInt64(usageEvidence.CacheWrite1HInputTokens)
	event.AudioInputTokens = cloneInt64(usageEvidence.AudioInputTokens)
	event.CachedAudioInputTokens = cloneInt64(usageEvidence.CachedAudioInputTokens)
	event.AudioOutputTokens = cloneInt64(usageEvidence.AudioOutputTokens)
	event.ImageInputTokens = cloneInt64(usageEvidence.ImageInputTokens)
	event.CachedImageInputTokens = cloneInt64(usageEvidence.CachedImageInputTokens)
	event.ImageOutputTokens = cloneInt64(usageEvidence.ImageOutputTokens)
	event.MediaUnits = cloneString(usageEvidence.MediaUnits)
	event.ToolCalls = cloneToolCalls(usageEvidence.ToolCalls)
	event.Batch = true
	if _, err := usage.Validate(&event); err != nil {
		return nil, err
	}
	return &event, nil
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneToolCalls(value map[string]int64) map[string]int64 {
	if value == nil {
		return nil
	}
	copy := make(map[string]int64, len(value))
	for key, count := range value {
		copy[key] = count
	}
	return copy
}

func CustomIDDigest(auth *secrets.AuthKey, customID string) []byte {
	return auth.Digest(secrets.BatchLineDigest, customID)
}

type Decision struct {
	State State
	Gap   int64
}

func CloseDecision(expected *int64, counts ResultCounts, conflict, terminal bool, now, closeAfter time.Time) Decision {
	observed := counts.Observed()
	missing := int64(1)
	if expected != nil {
		missing = max(*expected-observed, 0)
	}
	if conflict {
		if !now.Before(closeAfter) {
			return Decision{State: StateUncertain, Gap: max(missing, 1)}
		}
		return Decision{State: StateConflict}
	}
	if expected != nil && observed >= *expected && (terminal || *expected > 0) {
		return Decision{State: StateSettled}
	}
	if terminal || !now.Before(closeAfter) {
		return Decision{State: StateUncertain, Gap: max(missing, 1)}
	}
	return Decision{State: StateActive}
}
