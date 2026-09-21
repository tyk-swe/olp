package gateway

import (
	"log/slog"
	"time"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/usage"
)

// Envelope is the single terminal, metadata-only record of one inference
// request. It never carries prompts, outputs, tool data, headers, or secrets.
type Envelope struct {
	ClientIP  string
	RequestID string
	// AccountingID is the durable identity of this request. It is the request
	// id whenever the gateway minted it, and a fresh identifier when the
	// caller named its own: a client-supplied request id is echoed back for
	// correlation, but two callers may name the same one, and durable records
	// cannot be stored under a key their subject chooses.
	AccountingID string
	Actor        string
	KeyID        string

	BudgetGroupID *string

	Attribution map[string]string

	PolicyDecisions []contentpolicy.Decision
	UserID          string
	Route           string
	RouteRevisionID string
	ReleaseSequence int64
	Family          string
	Mode            string // unary or streaming
	Operation       string // shared operation
	Surface         string // client wire protocol
	Outcome         string // success, failure, or cancelled
	Status          int    // status sent to the client; 0 when nothing was sent
	// ErrorClass is the terminal failure code the client was given, such as
	// gateway_timeout or client_cancelled. It is empty on success.
	ErrorClass string
	Committed  bool
	StartedAt  time.Time
	// CompletedAt is when the request reached its terminal outcome.
	CompletedAt time.Time
	Duration    time.Duration
	// FirstByte is the time from the start of the request until the first
	// byte of the response payload reached the client. It is nil when no
	// payload was delivered, which includes every request answered with only
	// an error body.
	FirstByte *time.Duration
	// RuntimeGenerationID is the generation of the release this request was
	// pinned to; it is empty before the first release installs.
	RuntimeGenerationID string
	Usage               *openai.Usage // nil when the upstream reported no usage
	Attempts            []AttemptFact
}

// AttemptFact records one credential attempt against one target.
type AttemptFact struct {
	FirstOutput        *time.Duration
	Strategy           string
	PolicyDigest       string
	VendorID           string
	Price              *usage.RoutingPrice
	Ordinal            int
	TargetID           string
	ProviderID         string
	ProviderRevisionID string
	UpstreamModel      string
	SlotID             string
	CredentialID       string
	CredentialVersion  int
	Mode               string // unary or streaming
	Status             int    // upstream HTTP status; 0 when none was received
	Class              string // success or a failure class from the retry taxonomy
	Committed          bool
	StartedAt          time.Time
	Duration           time.Duration
	// FirstByte is the time from the start of the attempt until the upstream
	// response status was received. It is nil when no response arrived.
	FirstByte *time.Duration
	// RetryAfter is the delay the upstream asked for; nil unless it sent one.
	RetryAfter *time.Duration
	Usage      *openai.Usage
	// UsageObserved reports that the upstream told this gateway what the
	// attempt consumed, UsageComplete that no further consumption can be
	// attributed to it, and BillingUncertain that the upstream may have
	// served and billed work this attempt cannot account for. Exactly one of
	// UsageComplete and BillingUncertain holds when usage was not observed.
	UsageObserved    bool
	UsageComplete    bool
	BillingUncertain bool
}

// recordEvidence records what the attempt proves about upstream billing.
// Observed usage settles the attempt; otherwise the caller decides whether
// the upstream may still have billed for work that was never reported.
func (a *AttemptFact) recordEvidence(uncertain bool) {
	switch {
	case a.Usage != nil:
		a.UsageObserved, a.UsageComplete, a.BillingUncertain = true, true, false
	case uncertain:
		a.UsageObserved, a.UsageComplete, a.BillingUncertain = false, false, true
	default:
		a.UsageObserved, a.UsageComplete, a.BillingUncertain = false, true, false
	}
}

// Sink receives every terminal envelope exactly once.
type Sink interface {
	Terminal(Envelope)
}

// LogSink writes envelopes as structured metadata-only log records.
type LogSink struct{ Log *slog.Logger }

func (l LogSink) Terminal(e Envelope) {
	attempts := make([]map[string]any, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		attempts = append(attempts, map[string]any{
			"ordinal":              a.Ordinal,
			"provider_id":          a.ProviderID,
			"provider_revision_id": a.ProviderRevisionID,
			"upstream_model":       a.UpstreamModel,
			"credential_id":        a.CredentialID,
			"started_at":           a.StartedAt,
			"usage":                a.Usage,
			"target_id":            a.TargetID,
			"slot_id":              a.SlotID,
			"credential_version":   a.CredentialVersion,
			"status":               a.Status,
			"class":                a.Class,
			"committed":            a.Committed,
			"duration":             a.Duration,
			"mode":                 a.Mode,
			"first_byte":           a.FirstByte,
			"retry_after":          a.RetryAfter,
			"usage_observed":       a.UsageObserved,
			"usage_complete":       a.UsageComplete,
			"billing_uncertain":    a.BillingUncertain,
		})
	}
	args := []any{
		slog.String("request_id", e.RequestID),
		slog.String("accounting_id", e.AccountingID),
		slog.String("client_ip", e.ClientIP),
		slog.String("route_revision_id", e.RouteRevisionID),
		slog.Time("started_at", e.StartedAt),
		slog.Any("usage", e.Usage),
		slog.String("actor", e.Actor),
		slog.String("key_id", e.KeyID),
		slog.String("user_id", e.UserID),
		slog.String("route", e.Route),
		slog.Int64("release_sequence", e.ReleaseSequence),
		slog.String("family", e.Family),
		slog.String("mode", e.Mode),
		slog.String("operation", e.Operation),
		slog.String("surface", e.Surface),
		slog.String("runtime_generation_id", e.RuntimeGenerationID),
		slog.String("outcome", e.Outcome),
		slog.Int("status", e.Status),
		slog.String("error_class", e.ErrorClass),
		slog.Bool("committed", e.Committed),
		slog.Time("completed_at", e.CompletedAt),
		slog.Duration("duration", e.Duration),
		slog.Any("first_byte", e.FirstByte),
		slog.Int("attempt_count", len(e.Attempts)),
		slog.Any("attempts", attempts),
		slog.Bool("usage_known", e.Usage != nil),
	}
	if e.Usage != nil {
		args = append(args, slog.Int64("input_tokens", e.Usage.InputTokens), slog.Int64("output_tokens", e.Usage.OutputTokens))
	}
	l.Log.Info("inference request", args...)
}
