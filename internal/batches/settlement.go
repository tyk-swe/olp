package batches

import (
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
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

type Surface string

const (
	SurfaceOpenAI    Surface = "openai"
	SurfaceAnthropic Surface = "anthropic"
	SurfaceGemini    Surface = "gemini"
	SurfaceNative    Surface = "native"
)

type ErrorClass string

const (
	ErrorSubmissionUnknown           ErrorClass = "submission_unknown"
	ErrorSubmissionCommitUnavailable ErrorClass = "submission_commit_unavailable"
)

type Binding struct {
	Kind               string
	APIKeyID           string
	RouteSlug          string
	RouteRevisionID    string
	ProviderID         string
	ProviderRevisionID string
	TargetID           string
	SlotID             string
	CredentialID       *string
}

func (b Binding) Resource(id uuid.UUID, upstreamID string) resources.Resource {
	return resources.Resource{
		UUID: id, Kind: b.Kind, APIKeyID: b.APIKeyID, RouteSlug: b.RouteSlug,
		RouteRevisionID: b.RouteRevisionID, ProviderID: b.ProviderID, ProviderRevisionID: b.ProviderRevisionID,
		TargetID: b.TargetID, SlotID: b.SlotID, CredentialID: b.CredentialID, UpstreamID: upstreamID,
	}
}

type Intent struct {
	ResourceID       uuid.UUID
	Adapter          connectors.BatchAdapter
	Binding          Binding
	Surface          Surface
	SubmissionDigest []byte
	LineOperation    wire.LineOperation
	Template         *usage.Event
	ExpectedResults  *int64
	Submission       Submission
	CreatorDeadline  time.Time
	CloseAfter       time.Time
}

type Settlement struct {
	Intent
	ClaimID    uuid.UUID
	UpstreamID string
	State      State
	Cursor     Cursor
	Attempts   int
	CreatedAt  time.Time
}

func (s Settlement) Activated(upstreamID string) Settlement {
	s.UpstreamID, s.State = upstreamID, StateActive
	return s
}

func (s Settlement) WithExpected(expected *int64) Settlement {
	s.ExpectedResults = expected
	return s
}

func (s Settlement) Model() string {
	if s.Template == nil || s.Template.UpstreamModel == nil {
		return ""
	}
	return *s.Template.UpstreamModel
}
