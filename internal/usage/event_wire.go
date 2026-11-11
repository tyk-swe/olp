package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tyk-swe/olp/internal/guardrails"
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
	Origin                  *Origin               `json:"origin"`
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
	EstimatedInputTokens *int64              `json:"estimated_input_tokens"`
	EstimateProvenance   *EstimateProvenance `json:"estimate_provenance"`
	ModelFamily          *string             `json:"model_family"`
}

func (usage AttemptUsage) MarshalJSON() ([]byte, error) {
	return json.Marshal(usage.WireWith(usage.Evidence.Observed(), usage.Evidence.Complete(), usage.Evidence.Uncertain()))
}

func (usage AttemptUsage) WireWith(observed, complete, uncertain bool) wireUsage {
	return wireUsage{
		InvocationCount:         usage.InvocationCount,
		TrainingTokens:          usage.TrainingTokens,
		StorageByteSeconds:      usage.StorageByteSeconds,
		CacheTokenSeconds:       usage.CacheTokenSeconds,
		ContainerSessions:       usage.ContainerSessions,
		Deferred:                usage.Deferred,
		Observed:                &observed,
		Complete:                &complete,
		BillingUncertain:        &uncertain,
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
	}
}

func (usage *AttemptUsage) UnmarshalJSON(payload []byte) error {
	var wire wireUsage
	if err := json.Unmarshal(payload, &wire); err != nil {
		return err
	}
	*usage = wire.attemptUsage()
	usage.Evidence = EvidenceFrom(flag(wire.Observed), flag(wire.Complete), flag(wire.BillingUncertain))
	return nil
}

func flag(value *bool) bool { return value != nil && *value }

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

func (w wireUsage) attemptUsage() AttemptUsage {
	return AttemptUsage{Deferred: w.Deferred, UsageQuantities: UsageQuantities{InvocationCount: w.InvocationCount, TrainingTokens: w.TrainingTokens, StorageByteSeconds: w.StorageByteSeconds, CacheTokenSeconds: w.CacheTokenSeconds, ContainerSessions: w.ContainerSessions, InputTokens: w.InputTokens, OutputTokens: w.OutputTokens, CachedInputTokens: w.CachedInputTokens, CacheWriteInputTokens: w.CacheWriteInputTokens, CacheWrite5MInputTokens: w.CacheWrite5MInputTokens, CacheWrite1HInputTokens: w.CacheWrite1HInputTokens, AudioInputTokens: w.AudioInputTokens, CachedAudioInputTokens: w.CachedAudioInputTokens, AudioOutputTokens: w.AudioOutputTokens, ImageInputTokens: w.ImageInputTokens, CachedImageInputTokens: w.CachedImageInputTokens, ImageOutputTokens: w.ImageOutputTokens, MediaUnits: w.MediaUnits, ToolCalls: w.ToolCalls, Batch: w.Batch}}
}
