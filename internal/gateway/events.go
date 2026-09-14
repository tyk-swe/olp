package gateway

import (
	"log/slog"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Envelope is the single terminal, metadata-only record of one inference
// request. It never carries prompts, outputs, tool data, headers, or secrets.
type Envelope struct {
	ClientIP        string
	RequestID       string
	Actor           string // api_key or playground
	KeyID           string
	UserID          string
	Route           string
	RouteRevisionID string
	ReleaseSequence int64
	Family          string
	Mode            string
	Outcome         string // success, failure, or cancelled
	Status          int    // status sent to the client; 0 when nothing was sent
	Committed       bool
	StartedAt       time.Time
	Duration        time.Duration
	Usage           *openai.Usage // nil when the upstream reported no usage
	Attempts        []AttemptFact
}

// AttemptFact records one credential attempt against one target.
type AttemptFact struct {
	Ordinal            int
	TargetID           string
	ProviderID         string
	ProviderRevisionID string
	UpstreamModel      string
	SlotID             string
	CredentialID       string
	CredentialVersion  int
	Status             int    // upstream HTTP status; 0 when none was received
	Class              string // success or a failure class from the retry taxonomy
	Committed          bool
	StartedAt          time.Time
	Duration           time.Duration
	Usage              *openai.Usage
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
		})
	}
	args := []any{
		slog.String("request_id", e.RequestID),
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
		slog.String("outcome", e.Outcome),
		slog.Int("status", e.Status),
		slog.Bool("committed", e.Committed),
		slog.Duration("duration", e.Duration),
		slog.Int("attempt_count", len(e.Attempts)),
		slog.Any("attempts", attempts),
		slog.Bool("usage_known", e.Usage != nil),
	}
	if e.Usage != nil {
		args = append(args, slog.Int64("input_tokens", e.Usage.InputTokens), slog.Int64("output_tokens", e.Usage.OutputTokens))
	}
	l.Log.Info("inference request", args...)
}
