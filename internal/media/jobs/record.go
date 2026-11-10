package jobs

import (
	"errors"
	"math"
	"time"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type Lifecycle string

const (
	Creating        Lifecycle = "creating"
	Active          Lifecycle = "active"
	CreateAmbiguous Lifecycle = "create_ambiguous"
	CleanupPending  Lifecycle = "create_cleanup_pending"
	DeletePending   Lifecycle = "delete_pending"
	Deleted         Lifecycle = "deleted"
)

func ParseLifecycle(raw string) (Lifecycle, bool) {
	switch lifecycle := Lifecycle(raw); lifecycle {
	case Creating, Active, CreateAmbiguous, CleanupPending, DeletePending, Deleted:
		return lifecycle, true
	}
	return "", false
}

type State string

const (
	Queued    = State(wire.VideoJobQueued)
	Running   = State(wire.VideoJobRunning)
	Succeeded = State(wire.VideoJobSucceeded)
	Failed    = State(wire.VideoJobFailed)
	Cancelled = State("cancelled")
)

func ParseState(raw string) (State, bool) {
	switch state := State(raw); state {
	case Queued, Running, Succeeded, Failed, Cancelled:
		return state, true
	}
	return "", false
}

func (s State) Polling() bool { return s == Queued || s == Running }

type Record struct {
	ID                     string
	StrictContract         bool
	CallerCredential       bool
	UpstreamJobID          *string
	APIKeyID               string
	ProjectID              *string
	ProviderID             string
	ProviderName           string
	UpstreamModel          string
	RouteSlug              string
	Operation              media.Operation
	Surface                string
	State                  State
	Lifecycle              Lifecycle
	ProgressPercent        *float32
	ExpiresAt              *time.Time
	ErrorClass             *string
	CompletedAt            *time.Time
	LastPolledAt           *time.Time
	ReconciliationError    *string
	DeletedAt              *time.Time
	RuntimeGenerationID    string
	ProviderRevisionID     string
	CredentialVersionID    *string
	SlotID                 string
	ReconciliationClaimID  *string
	ReconciliationAttempts int
	NextReconciliationAt   time.Time
	LastReconciliationAt   *time.Time
	ETag                   string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

func (r Record) ContentAvailable() bool { return r.State == Succeeded && r.Lifecycle != Deleted }

func (r Record) Upstream() (string, bool) {
	if r.UpstreamJobID == nil || !media.ValidUpstreamJobID(*r.UpstreamJobID) {
		return "", false
	}
	return *r.UpstreamJobID, true
}

type Claimed struct {
	Record
	claim string
}

func (c Claimed) Fence() Fence { return Fence{claim: &c.claim} }

type Fence struct{ claim *string }

var Unfenced = Fence{}

type Reservation struct {
	ID                  string
	StrictContract      bool
	RuntimeGenerationID string
	ProviderRevisionID  string
	APIKeyID            string
	ProviderID          string
	UpstreamModel       string
	RouteSlug           string
	Surface             string
	CredentialVersionID *string
	SlotID              string
	StaleAfter          time.Time
}

type Update struct {
	state    State
	progress *float32
	expires  *time.Time
	failure  *string
	polled   time.Time
}

func Observe(video *media.VideoJobResult, at time.Time) (Update, bool) {
	if video == nil {
		return Update{}, false
	}
	observed, ok := wire.VideoJobStateOf(video.Status)
	if !ok {
		return Update{}, false
	}
	if p := video.Progress; p != nil && (math.IsNaN(float64(*p)) || *p < 0 || *p > 100) {
		return Update{}, false
	}
	state := State(observed)
	failure := video.ErrorCode
	if state != Failed {
		failure = nil
	}
	return Update{state: state, progress: video.Progress, expires: media.UnixTime(video.ExpiresAt), failure: failure, polled: at}, true
}

type Position struct {
	CreatedAt time.Time
	ID        string
}

type Scope struct {
	ids        []string
	restricted bool
}

var Everything = Scope{}

func Only(ids []string) Scope { return Scope{ids: append([]string{}, ids...), restricted: true} }

func (s Scope) param() []string {
	if !s.restricted {
		return nil
	}
	return s.ids
}

type Query struct {
	Projects      Scope
	Routes        Scope
	APIKeyID      *string
	ProviderID    *string
	Operation     *media.Operation
	Surface       *string
	State         *State
	Lifecycle     *Lifecycle
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	After         *Position
	Order         media.Order
	Limit         int
}

type Page struct {
	Items []Record
	More  bool
}

type Summary struct {
	Pending         int64
	Stale           int64
	Failed          int64
	OldestPendingAt *time.Time
}

type Kind int

const (
	KindDatabase Kind = iota
	KindNotFound
	KindChanged
	KindConflict
	KindInvalid
	KindBusy
	KindCallerCredential
)

type Error struct {
	Kind    Kind
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Message + ": " + e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func KindOf(err error) (Kind, bool) {
	if failure, ok := errors.AsType[*Error](err); ok {
		return failure.Kind, true
	}
	return 0, false
}

type Unavailable struct{ Code string }

func (u *Unavailable) Error() string { return u.Code }

var (
	ErrNoContent        = errors.New("media job has no downloadable content")
	errClaimLost        = errors.New("reconciliation_claim_lost")
	errNotFound         = &Error{Kind: KindNotFound, Message: "media job was not found"}
	errChanged          = &Error{Kind: KindChanged, Message: "media job changed; refresh and retry"}
	errCallerCredential = &Error{Kind: KindCallerCredential, Message: "media job runs on the caller's credential"}
)

func invalid(message string) *Error { return &Error{Kind: KindInvalid, Message: message} }

func busy(message string) *Error { return &Error{Kind: KindBusy, Message: message} }
