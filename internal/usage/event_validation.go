package usage

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/pricingpolicy"
	"github.com/tyk-swe/olp/internal/toolidentity"
)

// ErrInvalidEvent marks an event that decoded but cannot be accounted for. The
// consumer records it as a gap and acknowledges the delivery: retrying it would
// never succeed, and silently dropping it would hide the loss.
var ErrInvalidEvent = errors.New("invalid request metadata event")

type Validated struct {
	HasAttempts  bool
	StatusCode   *int32
	LatencyMS    int32
	FirstByteMS  *int32
	AttemptCount int16
	Attempts     []ValidatedAttempt
}

type ValidatedAttempt struct {
	Attempt     *Attempt
	Usage       AttemptUsage
	Ordinal     int16
	StatusCode  *int32
	LatencyMS   int32
	FirstByteMS *int32
}

func Validate(e *Event) (*Validated, error) {
	if !validBudgetBoundary(e.BudgetBoundary, e.ErrorClass) {
		return nil, fmt.Errorf("%w: invalid budget boundary", ErrInvalidEvent)
	}
	hasAttempts := len(e.Attempts) > 0
	if e.HTTPOutcomePending {
		if !hasAttempts || e.StatusCode != nil || e.ErrorClass != nil || e.Committed || e.FirstByteMS != nil || e.PayloadCaptured || e.BudgetBoundary != "" {
			return nil, fmt.Errorf("%w: pending HTTP outcome carries finalized request metadata", ErrInvalidEvent)
		}
		final := e.Attempts[len(e.Attempts)-1]
		if final.Committed || final.ErrorClass != nil {
			return nil, fmt.Errorf("%w: pending HTTP outcome carries a finalized attempt", ErrInvalidEvent)
		}
	}
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
			e.CachedInputTokens == nil && e.CacheWriteInputTokens == nil &&
			e.CacheWrite5MInputTokens == nil && e.CacheWrite1HInputTokens == nil &&
			e.AudioInputTokens == nil && e.CachedAudioInputTokens == nil &&
			e.AudioOutputTokens == nil && e.ImageInputTokens == nil &&
			e.CachedImageInputTokens == nil && e.ImageOutputTokens == nil &&
			e.MediaUnits == nil && len(e.ToolCalls) == 0 && !e.Batch && !e.UsageComplete && e.TrainingTokens == nil && e.StorageByteSeconds == nil && e.CacheTokenSeconds == nil && len(e.ContainerSessions) == 0)
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
	if e.EndUserDigest != "" && (!authority.ValidEndUserDigest(e.EndUserDigest) || e.Origin.Keyless()) {
		return nil, fmt.Errorf("%w: invalid end-user digest", ErrInvalidEvent)
	}
	if err := validateDelegation(e); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	if err := attribution.Validate(e.Attribution); err != nil {
		return nil, fmt.Errorf("%w: attribution %w", ErrInvalidEvent, err)
	}
	if err := guardrails.ValidateEvidence(e.GuardrailDecisions); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	if err := contentpolicy.ValidateDecisions(e.PolicyDecisions); err != nil {
		return nil, fmt.Errorf("%w: policy decisions %w", ErrInvalidEvent, err)
	}
	if err := validateUsageDimensions(e.tokenQuantities()); err != nil {
		return nil, fmt.Errorf("%w: request %w", ErrInvalidEvent, err)
	}
	if err := validateToolCalls(e.ToolCalls); err != nil {
		return nil, fmt.Errorf("%w: request %w", ErrInvalidEvent, err)
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
	if err := attempt.Execution.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	if err := guardrails.ValidateEvidence(attempt.GuardrailDecisions); err != nil {
		return nil, fmt.Errorf("%w: attempt %d %w", ErrInvalidEvent, index, err)
	}
	if attempt.Routing != nil {
		if err := attempt.Routing.Prompt.validate(); err != nil {
			return nil, fmt.Errorf("%w: attempt %d %w", ErrInvalidEvent, index, err)
		}
		if err := attempt.Routing.validateCredentialSource(); err != nil {
			return nil, fmt.Errorf("%w: attempt %d %w", ErrInvalidEvent, index, err)
		}
		if err := attempt.Routing.Interaction.validate(); err != nil {
			return nil, fmt.Errorf("%w: attempt %d %w", ErrInvalidEvent, index, err)
		}
	}
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
	usage, err := ValidateUsage(attempt.Usage, index)
	if err != nil {
		return nil, err
	}
	if err = checkEstimate(attempt.InputEstimate, attempt.ModelFamily); err != nil {
		return nil, fmt.Errorf("%w: attempt %d %w", ErrInvalidEvent, index, err)
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

func (q UsageQuantities) tokenCounts() [12]*int64 {
	return [12]*int64{q.InputTokens, q.OutputTokens, q.CachedInputTokens,
		q.CacheWriteInputTokens, q.CacheWrite5MInputTokens, q.CacheWrite1HInputTokens,
		q.AudioInputTokens, q.CachedAudioInputTokens, q.AudioOutputTokens,
		q.ImageInputTokens, q.CachedImageInputTokens, q.ImageOutputTokens}
}

func (q UsageQuantities) hasTokenCounts() bool {
	for _, tokens := range q.tokenCounts() {
		if tokens != nil {
			return true
		}
	}
	return false
}

func (e *Event) tokenQuantities() UsageQuantities {
	return UsageQuantities{InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CachedInputTokens: e.CachedInputTokens,
		CacheWriteInputTokens: e.CacheWriteInputTokens, CacheWrite5MInputTokens: e.CacheWrite5MInputTokens, CacheWrite1HInputTokens: e.CacheWrite1HInputTokens,
		AudioInputTokens: e.AudioInputTokens, CachedAudioInputTokens: e.CachedAudioInputTokens, AudioOutputTokens: e.AudioOutputTokens,
		ImageInputTokens: e.ImageInputTokens, CachedImageInputTokens: e.CachedImageInputTokens, ImageOutputTokens: e.ImageOutputTokens}
}

func ValidateUsage(usage *AttemptUsage, index int) (*AttemptUsage, error) {
	if usage == nil {
		return nil, fmt.Errorf("%w: attempt %d has no usage evidence", ErrInvalidEvent, index)
	}
	if !usage.Evidence.Valid() {
		return nil, fmt.Errorf("%w: attempt %d usage state is inconsistent", ErrInvalidEvent, index)
	}
	hasValues := usage.hasTokenCounts() ||
		usage.MediaUnits != nil || len(usage.ToolCalls) > 0 || RetainedValues(*usage)
	if !usage.Evidence.Observed() && hasValues {
		return nil, fmt.Errorf("%w: attempt %d reports usage it never observed", ErrInvalidEvent, index)
	}
	for _, tokens := range usage.tokenCounts() {
		if tokens != nil && *tokens < 0 {
			return nil, fmt.Errorf("%w: attempt %d has a negative token count", ErrInvalidEvent, index)
		}
	}
	if err := validateUsageDimensions(usage.UsageQuantities); err != nil {
		return nil, fmt.Errorf("%w: attempt %d %w", ErrInvalidEvent, index, err)
	}
	if err := ValidateRetainedUsage(*usage); err != nil {
		return nil, fmt.Errorf("%w: attempt %d %w", ErrInvalidEvent, index, err)
	}
	if err := validateToolCalls(usage.ToolCalls); err != nil {
		return nil, fmt.Errorf("%w: attempt %d %w", ErrInvalidEvent, index, err)
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

// validateUsageDimensions checks the counters against the subset invariants the
// fact table enforces: every share belongs to a total that was reported, and no
// combination of shares exceeds its total. The modality shares behave exactly
// like the cache shares: they are subsets of the input and output counts.
func validateUsageDimensions(q UsageQuantities) error {
	input, output, read, write, write5m, write1h := q.InputTokens, q.OutputTokens, q.CachedInputTokens, q.CacheWriteInputTokens, q.CacheWrite5MInputTokens, q.CacheWrite1HInputTokens
	audio, cachedAudio, audioOut, image, cachedImage, imageOut := q.AudioInputTokens, q.CachedAudioInputTokens, q.AudioOutputTokens, q.ImageInputTokens, q.CachedImageInputTokens, q.ImageOutputTokens
	count := func(value *int64) int64 {
		if value == nil {
			return 0
		}
		return *value
	}
	hasInputShare := read != nil || write != nil || write5m != nil || write1h != nil ||
		audio != nil || cachedAudio != nil || image != nil || cachedImage != nil
	if hasInputShare && input == nil {
		return errors.New("usage shares without input tokens")
	}
	if (audioOut != nil || imageOut != nil) && output == nil {
		return errors.New("usage shares without output tokens")
	}
	if (write5m != nil || write1h != nil) && write == nil {
		return errors.New("cache write detail without a cache write total")
	}
	if count(cachedAudio) > count(audio) {
		return errors.New("cached audio exceeds the audio input total")
	}
	if count(cachedImage) > count(image) {
		return errors.New("cached image exceeds the image input total")
	}
	if count(cachedAudio)+count(cachedImage) > count(read) {
		return errors.New("cached modality tokens exceed the cached input total")
	}
	// Counts are non-negative, so subtracting cannot underflow where adding could overflow.
	if input != nil {
		shares := count(read) + count(write) + count(audio) - count(cachedAudio) + count(image) - count(cachedImage)
		if count(read) > *input || shares-count(read) > *input-count(read) {
			return errors.New("usage shares exceed the input total")
		}
	}
	if count(write5m) > count(write) || count(write1h) > count(write)-count(write5m) {
		return errors.New("cache write detail exceeds the cache write total")
	}
	if output != nil && count(audioOut)+count(imageOut) > *output {
		return errors.New("modality shares exceed the output total")
	}
	return nil
}

func ConsistentUsage(usage *AttemptUsage) bool {
	if ValidateRetainedUsage(*usage) != nil {
		return false
	}
	for _, tokens := range usage.tokenCounts() {
		if tokens != nil && *tokens < 0 {
			return false
		}
	}
	if err := validateUsageDimensions(usage.UsageQuantities); err != nil {
		return false
	}
	return validateToolCalls(usage.ToolCalls) == nil
}

const MaxToolCalls = 16

func validateToolCalls(calls map[string]int64) error {
	if len(calls) > MaxToolCalls {
		return errors.New("too many tool call counters")
	}
	for name, count := range calls {
		if !toolidentity.ValidMeteredName(name) {
			return errors.New("a tool call name is malformed")
		}
		if count < 0 {
			return errors.New("a tool call count is negative")
		}
	}
	return nil
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

func validBudgetBoundary(boundary string, code *string) bool {
	if boundary == "" {
		return true
	}
	if code == nil || *code != "budget_exhausted" {
		return false
	}
	return limits.ValidBudgetBoundary(boundary)
}

// priceOperations are the operations a price may be scoped to, and the
// operations a usage filter may name.
var priceOperations = []string{"generation", "embeddings", "token_count", "image_generation",
	"image_edit", "image_variation", "speech", "transcription", "translation", "video_create", "video_list",
	"video_get", "video_content", "video_delete", "moderation", "model_list", "model_get", "rerank",
	"batch", "realtime", "bedrock_invoke", "agent_resource", "fine_tuning", "vector_store", "retrieval",
	"conversation", "container", "eval", "skill", "context_cache", "passthrough"}

func ValidOperation(value string) bool {
	if slices.Contains(priceOperations, value) {
		return true
	}
	return operationregistry.Catalog().HasOperation(value)
}

func RetainedValues(u AttemptUsage) bool {
	return u.InvocationCount != nil || u.TrainingTokens != nil || u.StorageByteSeconds != nil || u.CacheTokenSeconds != nil || len(u.ContainerSessions) > 0
}

func ValidateRetainedUsage(u AttemptUsage) error {
	if u.InvocationCount != nil && *u.InvocationCount < 0 {
		return errors.New("negative invocation count")
	}
	if u.TrainingTokens != nil && *u.TrainingTokens < 0 {
		return errors.New("negative training tokens")
	}
	for _, quantity := range []*string{u.StorageByteSeconds, u.CacheTokenSeconds} {
		if quantity == nil {
			continue
		}
		if len(*quantity) > 128 {
			return errors.New("retained quantity too large")
		}
		if !retainedQuantityPattern.MatchString(*quantity) {
			return errors.New("invalid retained quantity")
		}
	}
	if len(u.ContainerSessions) > pricingpolicy.MaxContainerTiers {
		return errors.New("too many memory tiers")
	}
	for tier, count := range u.ContainerSessions {
		if !pricingpolicy.ValidContainerTier(tier) || count < 0 {
			return errors.New("invalid container memory tier or count")
		}
	}
	return nil
}

var retainedQuantityPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)
