package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// WireVersion is the envelope version this build writes and accepts. A reader
// that meets another version leaves the delivery pending rather than guessing
// at a shape it does not know.
const WireVersion = 1

// Event is one request's content-free metadata. Every optional field is
// serialized explicitly, so a reader can tell "absent" from "not yet known".
type Event struct {
	Version             int       `json:"version"`
	EventID             string    `json:"event_id"`
	RequestID           string    `json:"request_id"`
	RuntimeGenerationID string    `json:"runtime_generation_id"`
	APIKeyID            string    `json:"api_key_id"`
	ProviderID          *string   `json:"provider_id"`
	RouteSlug           string    `json:"route_slug"`
	UpstreamModel       *string   `json:"upstream_model"`
	Operation           string    `json:"operation"`
	Surface             string    `json:"surface"`
	RequestStartedAt    time.Time `json:"request_started_at"`
	RequestCompletedAt  time.Time `json:"request_completed_at"`
	ObservedAt          time.Time `json:"observed_at"`
	StatusCode          *int      `json:"status_code"`
	ErrorClass          *string   `json:"error_class"`
	Committed           bool      `json:"committed"`
	LatencyMS           int64     `json:"latency_ms"`
	FirstByteMS         *int64    `json:"first_byte_ms"`
	InputTokens         *int64    `json:"input_tokens"`
	OutputTokens        *int64    `json:"output_tokens"`
	CachedInputTokens   *int64    `json:"cached_input_tokens"`
	MediaUnits          *string   `json:"media_units"`
	UsageComplete       bool      `json:"usage_complete"`
	Unpriced            bool      `json:"unpriced"`
	Attempts            []Attempt `json:"attempts"`
}

// Attempt is one provider attempt made while serving the request. Routing and
// usage are omitted entirely when absent: a pre-attempt event carries neither,
// and an older writer may not have sent routing at all.
type Attempt struct {
	Routing       *Routing      `json:"routing,omitempty"`
	ID            string        `json:"id"`
	Ordinal       int           `json:"ordinal"`
	ProviderID    string        `json:"provider_id"`
	UpstreamModel string        `json:"upstream_model"`
	StartedAt     time.Time     `json:"started_at"`
	CompletedAt   time.Time     `json:"completed_at"`
	StatusCode    *int          `json:"status_code"`
	ErrorClass    *string       `json:"error_class"`
	Committed     bool          `json:"committed"`
	LatencyMS     int64         `json:"latency_ms"`
	FirstByteMS   *int64        `json:"first_byte_ms"`
	Usage         *AttemptUsage `json:"usage,omitempty"`
}

// AttemptUsage is the billing evidence one attempt produced. `Observed` says
// the provider reported usage, `Complete` says the accounting for this attempt
// is settled, and `BillingUncertain` says the attempt may have been charged
// upstream without the evidence ever arriving.
type AttemptUsage struct {
	Observed          bool    `json:"observed"`
	Complete          bool    `json:"complete"`
	BillingUncertain  bool    `json:"billing_uncertain"`
	InputTokens       *int64  `json:"input_tokens"`
	OutputTokens      *int64  `json:"output_tokens"`
	CachedInputTokens *int64  `json:"cached_input_tokens"`
	MediaUnits        *string `json:"media_units"`
}

// Routing is the provenance of one attempt: which policy chose it, which
// credential served it, and which revisions were in force. It is stored beside
// the attempt so a request can be explained months later, when the revisions
// that produced it have long been superseded.
type Routing struct {
	Policy               *json.RawMessage `json:"policy,omitempty"`
	Mode                 *string          `json:"mode,omitempty"`
	FirstOutputMS        *int64           `json:"first_output_ms,omitempty"`
	StreamedOutputTokens *int64           `json:"streamed_output_tokens,omitempty"`
	CredentialSlotID     *string          `json:"credential_slot_id"`
	CredentialVersionID  *string          `json:"credential_version_id"`
	ProviderRevisionID   string           `json:"provider_revision_id"`
	PricingRevisionID    *string          `json:"pricing_revision_id"`
}

// Encode renders the event as a versioned stream payload.
func Encode(e *Event) ([]byte, error) {
	envelope := *e
	envelope.Version = WireVersion
	if envelope.Attempts == nil {
		// An empty list, never JSON null: a reader requires the field, and
		// "no attempts yet" is a meaningful state, not a missing one.
		envelope.Attempts = []Attempt{}
	}
	payload, err := json.Marshal(&envelope)
	if err != nil {
		return nil, fmt.Errorf("encode request metadata event: %w", err)
	}
	return payload, nil
}

// Decoded says what a payload turned out to be.
type Decoded int

const (
	// DecodedEvent is a payload this build understands.
	DecodedEvent Decoded = iota
	// DecodedUnsupported is a payload written by another version. The delivery
	// is left pending for a build that knows the shape; dropping it would lose
	// usage, and guessing at it would invent usage.
	DecodedUnsupported
)

// Decode parses a stream payload. Parsing is strict: every field the event
// contract requires must be present and well formed, because the alternative is
// a zero value silently entering an account.
func Decode(payload []byte) (*Event, Decoded, error) {
	var probe struct {
		Version *uint64 `json:"version"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return nil, DecodedEvent, fmt.Errorf("decode request metadata envelope: %w", err)
	}
	if probe.Version != nil && *probe.Version != WireVersion {
		return nil, DecodedUnsupported, nil
	}
	var wire wireEvent
	if err := json.Unmarshal(payload, &wire); err != nil {
		return nil, DecodedEvent, fmt.Errorf("decode request metadata event: %w", err)
	}
	event, err := wire.decode()
	if err != nil {
		return nil, DecodedEvent, err
	}
	return event, DecodedEvent, nil
}

// The wire types mirror the event shape with every required field behind a
// pointer, so an absent field is a decode failure instead of a zero value.
type wireEvent struct {
	EventID             *string        `json:"event_id"`
	RequestID           *string        `json:"request_id"`
	RuntimeGenerationID *string        `json:"runtime_generation_id"`
	APIKeyID            *string        `json:"api_key_id"`
	ProviderID          *string        `json:"provider_id"`
	RouteSlug           *string        `json:"route_slug"`
	UpstreamModel       *string        `json:"upstream_model"`
	Operation           *string        `json:"operation"`
	Surface             *string        `json:"surface"`
	RequestStartedAt    *time.Time     `json:"request_started_at"`
	RequestCompletedAt  *time.Time     `json:"request_completed_at"`
	ObservedAt          *time.Time     `json:"observed_at"`
	StatusCode          *uint16        `json:"status_code"`
	ErrorClass          *string        `json:"error_class"`
	Committed           *bool          `json:"committed"`
	LatencyMS           *uint64        `json:"latency_ms"`
	FirstByteMS         *uint64        `json:"first_byte_ms"`
	InputTokens         *int64         `json:"input_tokens"`
	OutputTokens        *int64         `json:"output_tokens"`
	CachedInputTokens   *int64         `json:"cached_input_tokens"`
	MediaUnits          *string        `json:"media_units"`
	UsageComplete       *bool          `json:"usage_complete"`
	Unpriced            *bool          `json:"unpriced"`
	Attempts            *[]wireAttempt `json:"attempts"`
}

type wireAttempt struct {
	Routing       *wireRouting `json:"routing"`
	ID            *string      `json:"id"`
	Ordinal       *uint16      `json:"ordinal"`
	ProviderID    *string      `json:"provider_id"`
	UpstreamModel *string      `json:"upstream_model"`
	StartedAt     *time.Time   `json:"started_at"`
	CompletedAt   *time.Time   `json:"completed_at"`
	StatusCode    *uint16      `json:"status_code"`
	ErrorClass    *string      `json:"error_class"`
	Committed     *bool        `json:"committed"`
	LatencyMS     *uint64      `json:"latency_ms"`
	FirstByteMS   *uint64      `json:"first_byte_ms"`
	Usage         *wireUsage   `json:"usage"`
}

type wireUsage struct {
	Observed          *bool   `json:"observed"`
	Complete          *bool   `json:"complete"`
	BillingUncertain  *bool   `json:"billing_uncertain"`
	InputTokens       *int64  `json:"input_tokens"`
	OutputTokens      *int64  `json:"output_tokens"`
	CachedInputTokens *int64  `json:"cached_input_tokens"`
	MediaUnits        *string `json:"media_units"`
}

type wireRouting struct {
	Policy               *json.RawMessage `json:"policy"`
	Mode                 *string          `json:"mode"`
	FirstOutputMS        *uint64          `json:"first_output_ms"`
	StreamedOutputTokens *uint64          `json:"streamed_output_tokens"`
	CredentialSlotID     *string          `json:"credential_slot_id"`
	CredentialVersionID  *string          `json:"credential_version_id"`
	ProviderRevisionID   *string          `json:"provider_revision_id"`
	PricingRevisionID    *string          `json:"pricing_revision_id"`
}

// knownOperations and knownSurfaces are the canonical labels; an event outside them would
// be rejected by the fact table's constraints long after the delivery was
// acknowledged, so it is rejected here instead.
var knownOperations = map[string]struct{}{
	"generation": {}, "embeddings": {}, "token_count": {}, "image_generation": {},
	"image_edit": {}, "image_variation": {}, "speech": {}, "transcription": {},
	"video_create": {}, "video_list": {}, "video_get": {}, "video_content": {},
	"video_delete": {}, "moderation": {}, "model_list": {}, "model_get": {},
}

var knownSurfaces = map[string]struct{}{
	"openai": {}, "anthropic": {}, "gemini": {}, "unknown": {},
}

func (w wireEvent) decode() (*Event, error) {
	event := &Event{Version: WireVersion}
	var err error
	if event.EventID, err = requiredUUID("event_id", w.EventID); err != nil {
		return nil, err
	}
	if event.RequestID, err = requiredUUID("request_id", w.RequestID); err != nil {
		return nil, err
	}
	if event.RuntimeGenerationID, err = requiredUUID("runtime_generation_id", w.RuntimeGenerationID); err != nil {
		return nil, err
	}
	if event.APIKeyID, err = requiredUUID("api_key_id", w.APIKeyID); err != nil {
		return nil, err
	}
	if event.ProviderID, err = optionalUUID("provider_id", w.ProviderID); err != nil {
		return nil, err
	}
	if event.RouteSlug, err = requiredField("route_slug", w.RouteSlug); err != nil {
		return nil, err
	}
	event.UpstreamModel = w.UpstreamModel
	if event.Operation, err = requiredLabel("operation", w.Operation, knownOperations); err != nil {
		return nil, err
	}
	if event.Surface, err = requiredLabel("surface", w.Surface, knownSurfaces); err != nil {
		return nil, err
	}
	if event.RequestStartedAt, err = requiredField("request_started_at", w.RequestStartedAt); err != nil {
		return nil, err
	}
	if event.RequestCompletedAt, err = requiredField("request_completed_at", w.RequestCompletedAt); err != nil {
		return nil, err
	}
	if event.ObservedAt, err = requiredField("observed_at", w.ObservedAt); err != nil {
		return nil, err
	}
	event.StatusCode = optionalStatus(w.StatusCode)
	event.ErrorClass = w.ErrorClass
	if event.Committed, err = requiredField("committed", w.Committed); err != nil {
		return nil, err
	}
	if event.LatencyMS, err = requiredMilliseconds("latency_ms", w.LatencyMS); err != nil {
		return nil, err
	}
	if event.FirstByteMS, err = optionalMilliseconds("first_byte_ms", w.FirstByteMS); err != nil {
		return nil, err
	}
	event.InputTokens = w.InputTokens
	event.OutputTokens = w.OutputTokens
	event.CachedInputTokens = w.CachedInputTokens
	if event.MediaUnits, err = optionalDecimal("media_units", w.MediaUnits); err != nil {
		return nil, err
	}
	if event.UsageComplete, err = requiredField("usage_complete", w.UsageComplete); err != nil {
		return nil, err
	}
	if event.Unpriced, err = requiredField("unpriced", w.Unpriced); err != nil {
		return nil, err
	}
	if w.Attempts == nil {
		return nil, missingField("attempts")
	}
	event.Attempts = make([]Attempt, 0, len(*w.Attempts))
	for index, wired := range *w.Attempts {
		attempt, err := wired.decode()
		if err != nil {
			return nil, fmt.Errorf("attempt %d: %w", index, err)
		}
		event.Attempts = append(event.Attempts, *attempt)
	}
	return event, nil
}

func (w wireAttempt) decode() (*Attempt, error) {
	attempt := &Attempt{}
	var err error
	if w.Routing != nil {
		if attempt.Routing, err = w.Routing.decode(); err != nil {
			return nil, err
		}
	}
	if attempt.ID, err = requiredUUID("id", w.ID); err != nil {
		return nil, err
	}
	ordinal, err := requiredField("ordinal", w.Ordinal)
	if err != nil {
		return nil, err
	}
	attempt.Ordinal = int(ordinal)
	if attempt.ProviderID, err = requiredUUID("provider_id", w.ProviderID); err != nil {
		return nil, err
	}
	if attempt.UpstreamModel, err = requiredField("upstream_model", w.UpstreamModel); err != nil {
		return nil, err
	}
	if attempt.StartedAt, err = requiredField("started_at", w.StartedAt); err != nil {
		return nil, err
	}
	if attempt.CompletedAt, err = requiredField("completed_at", w.CompletedAt); err != nil {
		return nil, err
	}
	attempt.StatusCode = optionalStatus(w.StatusCode)
	attempt.ErrorClass = w.ErrorClass
	if attempt.Committed, err = requiredField("committed", w.Committed); err != nil {
		return nil, err
	}
	if attempt.LatencyMS, err = requiredMilliseconds("latency_ms", w.LatencyMS); err != nil {
		return nil, err
	}
	if attempt.FirstByteMS, err = optionalMilliseconds("first_byte_ms", w.FirstByteMS); err != nil {
		return nil, err
	}
	if w.Usage != nil {
		if attempt.Usage, err = w.Usage.decode(); err != nil {
			return nil, err
		}
	}
	return attempt, nil
}

func (w wireUsage) decode() (*AttemptUsage, error) {
	usage := &AttemptUsage{
		InputTokens:       w.InputTokens,
		OutputTokens:      w.OutputTokens,
		CachedInputTokens: w.CachedInputTokens,
	}
	var err error
	if usage.Observed, err = requiredField("usage.observed", w.Observed); err != nil {
		return nil, err
	}
	if usage.Complete, err = requiredField("usage.complete", w.Complete); err != nil {
		return nil, err
	}
	if usage.BillingUncertain, err = requiredField("usage.billing_uncertain", w.BillingUncertain); err != nil {
		return nil, err
	}
	if usage.MediaUnits, err = optionalDecimal("usage.media_units", w.MediaUnits); err != nil {
		return nil, err
	}
	return usage, nil
}

func (w wireRouting) decode() (*Routing, error) {
	routing := &Routing{Policy: w.Policy, Mode: w.Mode}
	var err error
	if routing.FirstOutputMS, err = optionalMilliseconds("routing.first_output_ms", w.FirstOutputMS); err != nil {
		return nil, err
	}
	if w.StreamedOutputTokens != nil {
		if *w.StreamedOutputTokens > math.MaxInt64 {
			return nil, errors.New("routing.streamed_output_tokens is out of range")
		}
		tokens := int64(*w.StreamedOutputTokens)
		routing.StreamedOutputTokens = &tokens
	}
	if routing.CredentialSlotID, err = optionalUUID("routing.credential_slot_id", w.CredentialSlotID); err != nil {
		return nil, err
	}
	if routing.CredentialVersionID, err = optionalUUID("routing.credential_version_id", w.CredentialVersionID); err != nil {
		return nil, err
	}
	if routing.ProviderRevisionID, err = requiredUUID("routing.provider_revision_id", w.ProviderRevisionID); err != nil {
		return nil, err
	}
	if routing.PricingRevisionID, err = optionalUUID("routing.pricing_revision_id", w.PricingRevisionID); err != nil {
		return nil, err
	}
	return routing, nil
}

func missingField(field string) error {
	return fmt.Errorf("request metadata field %s is missing", field)
}

func requiredField[T any](field string, value *T) (T, error) {
	if value == nil {
		var zero T
		return zero, missingField(field)
	}
	return *value, nil
}

func requiredUUID(field string, value *string) (string, error) {
	text, err := requiredField(field, value)
	if err != nil {
		return "", err
	}
	parsed, err := uuid.Parse(text)
	if err != nil {
		return "", fmt.Errorf("request metadata field %s is not a uuid", field)
	}
	return parsed.String(), nil
}

func optionalUUID(field string, value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := requiredUUID(field, value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func requiredLabel(field string, value *string, allowed map[string]struct{}) (string, error) {
	label, err := requiredField(field, value)
	if err != nil {
		return "", err
	}
	if _, ok := allowed[label]; !ok {
		return "", fmt.Errorf("request metadata field %s has unknown value", field)
	}
	return label, nil
}

// optionalStatus widens the wire's u16 to the int the rest of the code carries;
// the HTTP range itself is a validation rule, not a decoding one.
func optionalStatus(value *uint16) *int {
	if value == nil {
		return nil
	}
	status := int(*value)
	return &status
}

func requiredMilliseconds(field string, value *uint64) (int64, error) {
	raw, err := requiredField(field, value)
	if err != nil {
		return 0, err
	}
	if raw > math.MaxInt64 {
		return 0, fmt.Errorf("request metadata field %s is out of range", field)
	}
	return int64(raw), nil
}

func optionalMilliseconds(field string, value *uint64) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	milliseconds, err := requiredMilliseconds(field, value)
	if err != nil {
		return nil, err
	}
	return &milliseconds, nil
}

func optionalDecimal(field string, value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	if _, err := parseWireDecimal(*value); err != nil {
		return nil, fmt.Errorf("request metadata field %s is not a decimal: %w", field, err)
	}
	return value, nil
}

// parseWireDecimal accepts the canonical decimal text media units travel as and
// reports whether it is below zero. The bounds are those of the numeric(24,6)
// column the value lands in: a wider value would abort the persisting
// transaction and the delivery would then be retried forever.
func parseWireDecimal(value string) (bool, error) {
	digits := value
	negative := false
	if strings.HasPrefix(digits, "+") || strings.HasPrefix(digits, "-") {
		negative = digits[0] == '-'
		digits = digits[1:]
	}
	integer, fraction, hasFraction := strings.Cut(digits, ".")
	if integer == "" || (hasFraction && fraction == "") {
		return false, errors.New("empty decimal")
	}
	if len(integer) > 18 || len(fraction) > 6 {
		return false, errors.New("decimal is out of range")
	}
	nonZero := false
	for _, part := range [2]string{integer, fraction} {
		for index := 0; index < len(part); index++ {
			if part[index] < '0' || part[index] > '9' {
				return false, errors.New("decimal has a non-digit")
			}
			if part[index] != '0' {
				nonZero = true
			}
		}
	}
	return negative && nonZero, nil
}

// ErrInvalidEvent marks an event that decoded but cannot be accounted for. The
// consumer records it as a gap and acknowledges the delivery: retrying it would
// never succeed, and silently dropping it would hide the loss.
var ErrInvalidEvent = errors.New("invalid request metadata event")

// Validated is an event whose internal consistency has been checked and whose
// values have been narrowed to the widths the accounting tables store.
type Validated struct {
	HasAttempts  bool
	StatusCode   *int32
	LatencyMS    int32
	FirstByteMS  *int32
	AttemptCount int16
	Attempts     []ValidatedAttempt
}

// ValidatedAttempt pairs the attempt as it arrived with its narrowed values and
// the usage evidence that must accompany every attempt.
type ValidatedAttempt struct {
	Attempt     *Attempt
	Usage       AttemptUsage
	Ordinal     int16
	StatusCode  *int32
	LatencyMS   int32
	FirstByteMS *int32
}

// Validate checks one event against the rules that make it accountable: the
// request timings and status must be coherent, the last attempt must be the
// target the event reports, an event without attempts must not carry target or
// usage metadata, and every attempt must carry usage evidence in one of the
// three states the charge model recognizes. Anything else is rejected whole:
// half an event is worse than none, because the missing half is invisible.
func Validate(e *Event) (*Validated, error) {
	hasAttempts := len(e.Attempts) > 0
	finalTargetMatches := true
	if hasAttempts {
		final := e.Attempts[len(e.Attempts)-1]
		finalTargetMatches = e.ProviderID != nil && *e.ProviderID == final.ProviderID &&
			e.UpstreamModel != nil && *e.UpstreamModel == final.UpstreamModel &&
			final.Committed == e.Committed
	}
	emptyAttemptMetadataIsValid := hasAttempts ||
		(e.ProviderID == nil && e.UpstreamModel == nil && !e.Committed &&
			e.FirstByteMS == nil && e.InputTokens == nil && e.OutputTokens == nil &&
			e.CachedInputTokens == nil && e.MediaUnits == nil && !e.UsageComplete)
	switch {
	case e.RequestCompletedAt.Before(e.RequestStartedAt):
		return nil, fmt.Errorf("%w: request completed before it started", ErrInvalidEvent)
	case strings.TrimSpace(e.RouteSlug) == "":
		return nil, fmt.Errorf("%w: blank route", ErrInvalidEvent)
	case !validStatus(e.StatusCode):
		return nil, fmt.Errorf("%w: request status outside the HTTP range", ErrInvalidEvent)
	case !finalTargetMatches:
		return nil, fmt.Errorf("%w: final attempt is not the reported target", ErrInvalidEvent)
	case !emptyAttemptMetadataIsValid:
		return nil, fmt.Errorf("%w: target metadata without an attempt", ErrInvalidEvent)
	}

	validated := &Validated{HasAttempts: hasAttempts, StatusCode: narrowStatus(e.StatusCode)}
	var err error
	if validated.LatencyMS, err = narrowMilliseconds(e.LatencyMS); err != nil {
		return nil, fmt.Errorf("%w: request latency is out of range", ErrInvalidEvent)
	}
	if validated.FirstByteMS, err = narrowOptionalMilliseconds(e.FirstByteMS); err != nil {
		return nil, fmt.Errorf("%w: request first byte is out of range", ErrInvalidEvent)
	}
	if len(e.Attempts) > math.MaxInt16 {
		return nil, fmt.Errorf("%w: too many attempts", ErrInvalidEvent)
	}
	validated.AttemptCount = int16(len(e.Attempts))
	validated.Attempts = make([]ValidatedAttempt, 0, len(e.Attempts))
	for index := range e.Attempts {
		attempt, err := validateAttempt(&e.Attempts[index], index)
		if err != nil {
			return nil, err
		}
		validated.Attempts = append(validated.Attempts, *attempt)
	}
	return validated, nil
}

func validateAttempt(attempt *Attempt, index int) (*ValidatedAttempt, error) {
	switch {
	case attempt.Ordinal != index+1:
		return nil, fmt.Errorf("%w: attempt %d has a non-contiguous ordinal", ErrInvalidEvent, index)
	case attempt.CompletedAt.Before(attempt.StartedAt):
		return nil, fmt.Errorf("%w: attempt %d completed before it started", ErrInvalidEvent, index)
	case !validStatus(attempt.StatusCode):
		return nil, fmt.Errorf("%w: attempt %d status outside the HTTP range", ErrInvalidEvent, index)
	}
	// Every attempt carries its own evidence; an attempt without it cannot be
	// classified as billable, not billable, or uncertain, and an unclassified
	// attempt has no honest place in an account.
	usage, err := validateUsage(attempt.Usage, index)
	if err != nil {
		return nil, err
	}
	validated := &ValidatedAttempt{
		Attempt:    attempt,
		Usage:      *usage,
		Ordinal:    int16(attempt.Ordinal),
		StatusCode: narrowStatus(attempt.StatusCode),
	}
	if validated.LatencyMS, err = narrowMilliseconds(attempt.LatencyMS); err != nil {
		return nil, fmt.Errorf("%w: attempt %d latency is out of range", ErrInvalidEvent, index)
	}
	if validated.FirstByteMS, err = narrowOptionalMilliseconds(attempt.FirstByteMS); err != nil {
		return nil, fmt.Errorf("%w: attempt %d first byte is out of range", ErrInvalidEvent, index)
	}
	return validated, nil
}

func validateUsage(usage *AttemptUsage, index int) (*AttemptUsage, error) {
	if usage == nil {
		return nil, fmt.Errorf("%w: attempt %d has no usage evidence", ErrInvalidEvent, index)
	}
	// Observed usage settles the attempt; unobserved usage is either settled as
	// not billable, or explicitly uncertain. Nothing else is a state the charge
	// model can price.
	stateIsValid := (!usage.Observed && usage.Complete && !usage.BillingUncertain) ||
		(!usage.Observed && !usage.Complete && usage.BillingUncertain) ||
		(usage.Observed && !usage.BillingUncertain)
	if !stateIsValid {
		return nil, fmt.Errorf("%w: attempt %d usage state is inconsistent", ErrInvalidEvent, index)
	}
	hasValues := usage.InputTokens != nil || usage.OutputTokens != nil ||
		usage.CachedInputTokens != nil || usage.MediaUnits != nil
	if !usage.Observed && hasValues {
		return nil, fmt.Errorf("%w: attempt %d reports usage it never observed", ErrInvalidEvent, index)
	}
	for _, tokens := range [3]*int64{usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens} {
		if tokens != nil && *tokens < 0 {
			return nil, fmt.Errorf("%w: attempt %d has a negative token count", ErrInvalidEvent, index)
		}
	}
	if usage.MediaUnits != nil {
		negative, err := parseWireDecimal(*usage.MediaUnits)
		if err != nil {
			return nil, fmt.Errorf("%w: attempt %d has malformed media units", ErrInvalidEvent, index)
		}
		if negative {
			return nil, fmt.Errorf("%w: attempt %d has negative media units", ErrInvalidEvent, index)
		}
	}
	return usage, nil
}

func validStatus(status *int) bool {
	return status == nil || (*status >= 100 && *status <= 599)
}

func narrowStatus(status *int) *int32 {
	if status == nil {
		return nil
	}
	narrowed := int32(*status)
	return &narrowed
}

func narrowMilliseconds(value int64) (int32, error) {
	if value < 0 || value > math.MaxInt32 {
		return 0, errors.New("milliseconds out of range")
	}
	return int32(value), nil
}

func narrowOptionalMilliseconds(value *int64) (*int32, error) {
	if value == nil {
		return nil, nil
	}
	narrowed, err := narrowMilliseconds(*value)
	if err != nil {
		return nil, err
	}
	return &narrowed, nil
}
