package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/routeidentity"
)

// Decode parses a stream payload. Parsing is strict: the envelope version and
// every field the event contract requires must be present and well formed,
// because the alternative is a zero value silently entering an account.
func Decode(payload []byte) (*Event, error) {
	var probe struct {
		Version *uint64 `json:"version"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return nil, fmt.Errorf("decode request metadata envelope: %w", err)
	}
	if probe.Version == nil {
		return nil, errors.New("request metadata version is missing")
	}
	if *probe.Version != WireVersion {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedVersion, *probe.Version)
	}
	var wire wireEvent
	if err := json.Unmarshal(payload, &wire); err != nil {
		return nil, fmt.Errorf("decode request metadata event: %w", err)
	}
	return wire.decode()
}

// The wire types mirror the event shape with every required field behind a
// pointer, so an absent field is a decode failure instead of a zero value.
type wireEvent struct {
	TrainingTokens     *int64           `json:"training_tokens,omitempty"`
	StorageByteSeconds *string          `json:"storage_byte_seconds,omitempty"`
	CacheTokenSeconds  *string          `json:"cache_token_seconds,omitempty"`
	ContainerSessions  map[string]int64 `json:"container_sessions,omitempty"`

	CacheResult             string                `json:"cache_result,omitempty"`
	EvaluatorGuardrailID    *string               `json:"evaluator_guardrail_id"`
	HTTPOutcomePending      bool                  `json:"http_outcome_pending,omitempty"`
	PayloadCaptured         bool                  `json:"payload_captured"`
	BudgetBoundary          string                `json:"budget_boundary,omitempty"`
	EventID                 *string               `json:"event_id"`
	RequestID               *string               `json:"request_id"`
	RuntimeGenerationID     *string               `json:"runtime_generation_id"`
	EndUserDigest           *string               `json:"end_user_digest"`
	APIKeyID                *string               `json:"api_key_id"`
	AgentPrincipalID        *string               `json:"agent_principal_id"`
	MemberPrincipalID       *string               `json:"member_principal_id"`
	DelegationPairDigest    *string               `json:"delegation_pair_digest"`
	Attribution             *map[string]string    `json:"attribution"`
	Origin                  *string               `json:"origin"`
	ParentRequestID         *string               `json:"parent_request_id"`
	GuardrailDecisions      []guardrails.Evidence `json:"guardrail_decisions"`
	PolicyDecisions         *[]wireDecision       `json:"policy_decisions"`
	BudgetGroupID           *string               `json:"budget_group_id"`
	ProviderID              *string               `json:"provider_id"`
	RouteSlug               *string               `json:"route_slug"`
	UpstreamModel           *string               `json:"upstream_model"`
	Operation               *string               `json:"operation"`
	Surface                 *string               `json:"surface"`
	RequestStartedAt        *time.Time            `json:"request_started_at"`
	RequestCompletedAt      *time.Time            `json:"request_completed_at"`
	ObservedAt              *time.Time            `json:"observed_at"`
	StatusCode              *uint16               `json:"status_code"`
	ErrorClass              *string               `json:"error_class"`
	Committed               *bool                 `json:"committed"`
	LatencyMS               *uint64               `json:"latency_ms"`
	FirstByteMS             *uint64               `json:"first_byte_ms"`
	InputTokens             *int64                `json:"input_tokens"`
	OutputTokens            *int64                `json:"output_tokens"`
	CachedInputTokens       *int64                `json:"cached_input_tokens"`
	CacheWriteInputTokens   *int64                `json:"cache_write_input_tokens"`
	CacheWrite5MInputTokens *int64                `json:"cache_write_5m_input_tokens"`
	CacheWrite1HInputTokens *int64                `json:"cache_write_1h_input_tokens"`
	AudioInputTokens        *int64                `json:"audio_input_tokens"`
	CachedAudioInputTokens  *int64                `json:"cached_audio_input_tokens"`
	AudioOutputTokens       *int64                `json:"audio_output_tokens"`
	ImageInputTokens        *int64                `json:"image_input_tokens"`
	CachedImageInputTokens  *int64                `json:"cached_image_input_tokens"`
	ImageOutputTokens       *int64                `json:"image_output_tokens"`
	MediaUnits              *string               `json:"media_units"`
	ToolCalls               *map[string]int64     `json:"tool_calls"`
	Batch                   *bool                 `json:"batch"`
	UsageComplete           *bool                 `json:"usage_complete"`
	Unpriced                *bool                 `json:"unpriced"`
	Attempts                *[]wireAttempt        `json:"attempts"`
}

type wireAttempt struct {
	Execution          *ExecutionReference   `json:"execution,omitempty"`
	GuardrailDecisions []guardrails.Evidence `json:"guardrail_decisions"`
	Routing            *wireRouting          `json:"routing"`
	ID                 *string               `json:"id"`
	Ordinal            *uint16               `json:"ordinal"`
	ProviderID         *string               `json:"provider_id"`
	UpstreamModel      *string               `json:"upstream_model"`
	StartedAt          *time.Time            `json:"started_at"`
	CompletedAt        *time.Time            `json:"completed_at"`
	StatusCode         *uint16               `json:"status_code"`
	ErrorClass         *string               `json:"error_class"`
	Committed          *bool                 `json:"committed"`
	LatencyMS          *uint64               `json:"latency_ms"`
	FirstByteMS        *uint64               `json:"first_byte_ms"`
	Usage              *wireUsage            `json:"usage"`
	// The estimate fields are optional on the wire: an event written before
	// estimates were recorded carries none of them.
	EstimatedInputTokens *int64  `json:"estimated_input_tokens"`
	EstimateProvenance   *string `json:"estimate_provenance"`
	ModelFamily          *string `json:"model_family"`
}

func (usage AttemptUsage) MarshalJSON() ([]byte, error) {
	return json.Marshal(wireUsage{
		InvocationCount:         usage.InvocationCount,
		TrainingTokens:          usage.TrainingTokens,
		StorageByteSeconds:      usage.StorageByteSeconds,
		CacheTokenSeconds:       usage.CacheTokenSeconds,
		ContainerSessions:       usage.ContainerSessions,
		Deferred:                usage.Deferred,
		Observed:                &usage.Observed,
		Complete:                &usage.Complete,
		BillingUncertain:        &usage.BillingUncertain,
		InputTokens:             usage.InputTokens,
		OutputTokens:            usage.OutputTokens,
		CachedInputTokens:       usage.CachedInputTokens,
		CacheWriteInputTokens:   usage.CacheWriteInputTokens,
		CacheWrite5MInputTokens: usage.CacheWrite5MInputTokens,
		CacheWrite1HInputTokens: usage.CacheWrite1HInputTokens,
		AudioInputTokens:        usage.AudioInputTokens,
		CachedAudioInputTokens:  usage.CachedAudioInputTokens,
		AudioOutputTokens:       usage.AudioOutputTokens,
		ImageInputTokens:        usage.ImageInputTokens,
		CachedImageInputTokens:  usage.CachedImageInputTokens,
		ImageOutputTokens:       usage.ImageOutputTokens,
		MediaUnits:              usage.MediaUnits,
		ToolCalls:               usage.ToolCalls,
		Batch:                   usage.Batch,
	})
}

type wireUsage struct {
	InvocationCount    *int64           `json:"invocation_count,omitempty"`
	TrainingTokens     *int64           `json:"training_tokens,omitempty"`
	StorageByteSeconds *string          `json:"storage_byte_seconds,omitempty"`
	CacheTokenSeconds  *string          `json:"cache_token_seconds,omitempty"`
	ContainerSessions  map[string]int64 `json:"container_sessions,omitempty"`
	Deferred           bool             `json:"deferred,omitempty"`

	Observed                *bool   `json:"observed"`
	Complete                *bool   `json:"complete"`
	BillingUncertain        *bool   `json:"billing_uncertain"`
	InputTokens             *int64  `json:"input_tokens"`
	OutputTokens            *int64  `json:"output_tokens"`
	CachedInputTokens       *int64  `json:"cached_input_tokens"`
	CacheWriteInputTokens   *int64  `json:"cache_write_input_tokens"`
	CacheWrite5MInputTokens *int64  `json:"cache_write_5m_input_tokens"`
	CacheWrite1HInputTokens *int64  `json:"cache_write_1h_input_tokens"`
	AudioInputTokens        *int64  `json:"audio_input_tokens"`
	CachedAudioInputTokens  *int64  `json:"cached_audio_input_tokens"`
	AudioOutputTokens       *int64  `json:"audio_output_tokens"`
	ImageInputTokens        *int64  `json:"image_input_tokens"`
	CachedImageInputTokens  *int64  `json:"cached_image_input_tokens"`
	ImageOutputTokens       *int64  `json:"image_output_tokens"`
	MediaUnits              *string `json:"media_units"`
	// ToolCalls counts metered server-side tool invocations by tool name, and
	// Batch marks usage produced at batch terms, which a batch_multiplier
	// prices. Both are omitted when absent.
	ToolCalls map[string]int64 `json:"tool_calls,omitempty"`
	Batch     bool             `json:"batch,omitempty"`
}

type wireDecision struct {
	RuleID  *string `json:"rule_id"`
	Phase   *string `json:"phase"`
	Action  *string `json:"action"`
	Outcome *string `json:"outcome"`
}

type wireRouting struct {
	Prompt               *PromptReference     `json:"prompt,omitempty"`
	Cache                *CacheEvidence       `json:"cache,omitempty"`
	BudgetExempt         bool                 `json:"budget_exempt,omitempty"`
	CredentialSource     string               `json:"credential_source,omitempty"`
	Interaction          *InteractionEvidence `json:"interaction"`
	Policy               *json.RawMessage     `json:"policy"`
	Mode                 *string              `json:"mode"`
	FirstOutputMS        *uint64              `json:"first_output_ms"`
	StreamedOutputTokens *uint64              `json:"streamed_output_tokens"`
	CredentialSlotID     *string              `json:"credential_slot_id"`
	CredentialVersionID  *string              `json:"credential_version_id"`
	ProviderRevisionID   *string              `json:"provider_revision_id"`
	PricingRevisionID    *string              `json:"pricing_revision_id"`
	Leg                  *RouteLeg            `json:"leg"`
	Selector             *string              `json:"selector"`
	Retry                *int                 `json:"retry"`
	Budgets              []string             `json:"budgets"`
	Baseline             *Baseline            `json:"baseline"`
}

var knownSurfaces = map[string]struct{}{
	"openai": {}, "anthropic": {}, "gemini": {}, "bedrock": {}, "native": {}, "unknown": {},
}

var knownOrigins = map[string]struct{}{OriginCaller: {}, OriginShadow: {}, OriginClassifier: {}, OriginProbe: {}, OriginPlayground: {}, OriginCacheEmbedding: {}, OriginBatchSettlement: {}, OriginRetainedSettlement: {}}

func (w wireEvent) decode() (*Event, error) {
	if w.CacheResult != "" && w.CacheResult != "hit" && w.CacheResult != "semantic-hit" && w.CacheResult != "miss" && w.CacheResult != "bypass" {
		return nil, errors.New("invalid cache result")
	}
	event := &Event{CacheResult: w.CacheResult, Version: WireVersion, HTTPOutcomePending: w.HTTPOutcomePending, PayloadCaptured: w.PayloadCaptured}
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
	if w.Origin != nil && *w.Origin != OriginCaller {
		if _, known := knownOrigins[*w.Origin]; !known {
			return nil, errors.New("request metadata field origin is not supported")
		}
		event.Origin = *w.Origin
	}
	if Keyless(event.Origin) {
		if w.APIKeyID != nil && *w.APIKeyID != "" {
			return nil, fmt.Errorf("request metadata field api_key_id must be empty for %s requests", event.Origin)
		}
	} else if event.APIKeyID, err = requiredUUID("api_key_id", w.APIKeyID); err != nil {
		return nil, err
	}
	if w.AgentPrincipalID != nil {
		event.AgentPrincipalID = *w.AgentPrincipalID
	}
	if w.MemberPrincipalID != nil {
		event.MemberPrincipalID = *w.MemberPrincipalID
	}
	if w.DelegationPairDigest != nil {
		event.DelegationPairDigest = *w.DelegationPairDigest
	}
	if err := validateDelegation(event); err != nil {
		return nil, err
	}
	if event.ParentRequestID, err = optionalUUID("parent_request_id", w.ParentRequestID); err != nil {
		return nil, err
	}
	if event.EvaluatorGuardrailID, err = optionalUUID("evaluator_guardrail_id", w.EvaluatorGuardrailID); err != nil {
		return nil, err
	}
	if (event.ParentRequestID != nil) != (event.Origin == OriginShadow || event.Origin == OriginClassifier || event.Origin == OriginCacheEmbedding || event.Origin == OriginBatchSettlement || event.Origin == OriginRetainedSettlement || event.EvaluatorGuardrailID != nil) {
		return nil, errors.New("request metadata field parent_request_id belongs to child work requests")
	}
	if w.EndUserDigest != nil {
		event.EndUserDigest = *w.EndUserDigest
		if event.EndUserDigest != "" && (!authority.ValidEndUserDigest(event.EndUserDigest) || Keyless(event.Origin)) {
			return nil, errors.New("request metadata field end_user_digest is invalid")
		}
	}
	if w.Attribution != nil {
		event.Attribution = *w.Attribution
		if err := ValidateAttribution(event.Attribution); err != nil {
			return nil, fmt.Errorf("request metadata field attribution is invalid: %w", err)
		}
	}
	if err := guardrails.ValidateEvidence(w.GuardrailDecisions); err != nil {
		return nil, err
	}
	event.GuardrailDecisions = w.GuardrailDecisions
	if w.PolicyDecisions != nil {
		for i, d := range *w.PolicyDecisions {
			if d.RuleID == nil || d.Phase == nil || d.Action == nil || d.Outcome == nil {
				return nil, fmt.Errorf("request metadata field policy_decisions %d is incomplete", i)
			}
			event.PolicyDecisions = append(event.PolicyDecisions, contentpolicy.Decision{
				RuleID: *d.RuleID, Phase: *d.Phase, Action: *d.Action, Outcome: *d.Outcome,
			})
		}
		if err := contentpolicy.ValidateDecisions(event.PolicyDecisions); err != nil {
			return nil, fmt.Errorf("request metadata field policy_decisions is invalid: %w", err)
		}
	}
	if event.BudgetGroupID, err = optionalUUID("budget_group_id", w.BudgetGroupID); err != nil {
		return nil, err
	}
	if event.ProviderID, err = optionalUUID("provider_id", w.ProviderID); err != nil {
		return nil, err
	}
	if event.RouteSlug, err = requiredField("route_slug", w.RouteSlug); err != nil {
		return nil, err
	}
	event.UpstreamModel = w.UpstreamModel
	if event.Operation, err = requiredField("operation", w.Operation); err != nil {
		return nil, err
	}
	if event.Operation != "file" && event.Operation != "guardrail" && !validOperation(event.Operation) {
		return nil, errors.New("request metadata field operation has unknown value")
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
	event.BudgetBoundary = w.BudgetBoundary
	if !validBudgetBoundary(event.BudgetBoundary, event.ErrorClass) {
		return nil, errors.New("invalid budget boundary")
	}
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
	event.CacheWriteInputTokens = w.CacheWriteInputTokens
	event.CacheWrite5MInputTokens = w.CacheWrite5MInputTokens
	event.CacheWrite1HInputTokens = w.CacheWrite1HInputTokens
	event.AudioInputTokens = w.AudioInputTokens
	event.CachedAudioInputTokens = w.CachedAudioInputTokens
	event.AudioOutputTokens = w.AudioOutputTokens
	event.ImageInputTokens = w.ImageInputTokens
	event.CachedImageInputTokens = w.CachedImageInputTokens
	event.TrainingTokens = w.TrainingTokens
	event.StorageByteSeconds = w.StorageByteSeconds
	event.CacheTokenSeconds = w.CacheTokenSeconds
	event.ContainerSessions = w.ContainerSessions
	event.ImageOutputTokens = w.ImageOutputTokens
	if event.MediaUnits, err = optionalDecimal("media_units", w.MediaUnits); err != nil {
		return nil, err
	}
	if w.ToolCalls != nil {
		event.ToolCalls = *w.ToolCalls
	}
	if w.Batch != nil {
		event.Batch = *w.Batch
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
	attempt := &Attempt{GuardrailDecisions: w.GuardrailDecisions, Execution: w.Execution}
	if err := attempt.Execution.Validate(); err != nil {
		return nil, err
	}
	if err := guardrails.ValidateEvidence(w.GuardrailDecisions); err != nil {
		return nil, err
	}
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
	attempt.EstimatedInputTokens = w.EstimatedInputTokens
	if w.EstimateProvenance != nil {
		attempt.EstimateProvenance = *w.EstimateProvenance
	}
	if w.ModelFamily != nil {
		attempt.ModelFamily = *w.ModelFamily
	}
	if err = checkEstimate(attempt.EstimatedInputTokens, attempt.EstimateProvenance, attempt.ModelFamily); err != nil {
		return nil, fmt.Errorf("request metadata %w", err)
	}
	return attempt, nil
}

func (w wireUsage) decode() (*AttemptUsage, error) {
	usage := &AttemptUsage{Deferred: w.Deferred, UsageQuantities: UsageQuantities{InvocationCount: w.InvocationCount, TrainingTokens: w.TrainingTokens, StorageByteSeconds: w.StorageByteSeconds, CacheTokenSeconds: w.CacheTokenSeconds, ContainerSessions: w.ContainerSessions, InputTokens: w.InputTokens, OutputTokens: w.OutputTokens, CachedInputTokens: w.CachedInputTokens, CacheWriteInputTokens: w.CacheWriteInputTokens, CacheWrite5MInputTokens: w.CacheWrite5MInputTokens, CacheWrite1HInputTokens: w.CacheWrite1HInputTokens, AudioInputTokens: w.AudioInputTokens, CachedAudioInputTokens: w.CachedAudioInputTokens, AudioOutputTokens: w.AudioOutputTokens, ImageInputTokens: w.ImageInputTokens, CachedImageInputTokens: w.CachedImageInputTokens, ImageOutputTokens: w.ImageOutputTokens, ToolCalls: w.ToolCalls, Batch: w.Batch}}
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

func (r *Routing) validateCredentialSource() error {
	if r.BudgetExempt && (r.CredentialSource != "caller" || len(r.Budgets) != 0) {
		return errors.New("caller-paid attempts cannot claim operator budget owners")
	}
	if r.CredentialSource != "" && r.CredentialSource != "operator" && r.CredentialSource != "caller" {
		return errors.New("routing.credential_source is invalid")
	}
	if r.CredentialSource == "caller" && r.CredentialVersionID != nil {
		return errors.New("caller routing cannot claim a stored credential")
	}
	return nil
}

func (w wireRouting) decode() (*Routing, error) {
	routing := &Routing{Policy: w.Policy, Mode: w.Mode, Interaction: w.Interaction, Prompt: w.Prompt}
	if err := routing.Prompt.validate(); err != nil {
		return nil, err
	}
	if err := routing.Interaction.validate(); err != nil {
		return nil, err
	}
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
	if w.CredentialSource != "" && w.CredentialSource != "operator" && w.CredentialSource != "caller" {
		return nil, errors.New("routing.credential_source is invalid")
	}
	routing.CredentialSource = w.CredentialSource
	routing.BudgetExempt = w.BudgetExempt
	if err := w.Cache.Validate(); err != nil {
		return nil, err
	}
	routing.Cache = w.Cache
	if routing.CredentialVersionID, err = optionalUUID("routing.credential_version_id", w.CredentialVersionID); err != nil {
		return nil, err
	}
	if err = routing.validateCredentialSource(); err != nil {
		return nil, err
	}
	if routing.ProviderRevisionID, err = requiredUUID("routing.provider_revision_id", w.ProviderRevisionID); err != nil {
		return nil, err
	}
	if routing.PricingRevisionID, err = optionalUUID("routing.pricing_revision_id", w.PricingRevisionID); err != nil {
		return nil, err
	}
	if leg := w.Leg; leg != nil {
		if !routeidentity.ValidSlug(leg.Route) || (leg.Via != ViaSelector && leg.Via != ViaFallback) {
			return nil, errors.New("routing.leg is malformed")
		}
		if _, err = requiredUUID("routing.leg.revision_id", &leg.RevisionID); err != nil {
			return nil, err
		}
		routing.Leg = leg
	}
	if w.Selector != nil && !routeidentity.ValidSelectorID(*w.Selector) {
		return nil, errors.New("routing.selector is malformed")
	}
	if w.Retry != nil && *w.Retry < 1 {
		return nil, errors.New("routing.retry is out of range")
	}
	if len(w.Budgets) > maxAttemptBudgets {
		return nil, errors.New("routing.budgets names too many caps")
	}
	for _, owner := range w.Budgets {
		if _, err = requiredUUID("routing.budgets", &owner); err != nil {
			return nil, err
		}
	}
	if b := w.Baseline; b != nil {
		if w.Selector == nil || b.UpstreamModel == "" || len(b.UpstreamModel) > 512 {
			return nil, errors.New("routing.baseline is malformed")
		}
		if _, err = requiredUUID("routing.baseline.provider_id", &b.ProviderID); err != nil {
			return nil, err
		}
		routing.Baseline = b
	}
	routing.Selector, routing.Retry, routing.Budgets = w.Selector, w.Retry, w.Budgets
	if err := routing.validateCredentialSource(); err != nil {
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
