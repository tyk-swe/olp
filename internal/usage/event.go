package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
)

// WireVersion is the envelope version this build writes and accepts. Every
// event carries it; other versions wait for a compatible consumer.
const WireVersion = 1

var ErrUnsupportedVersion = errors.New("unsupported request metadata version")

// Origins name whom a request was made for. A caller request is made for the
// API key that sent it. The gateway makes the others on its own account:
// shadow mirrors of caller traffic, classifier calls of request selectors and
// health probes, and a console member makes Playground calls. Shadow, probe
// and Playground requests belong to the installation and carry no API key; a
// classifier or cache embedding call is made for, and charged to, the key whose
// request it routes or matches. Batch settlement records deferred work for its
// caller's key and links each result to the batch creation request.
type Origin string

const (
	OriginCaller             Origin = "caller"
	OriginShadow             Origin = "shadow"
	OriginClassifier         Origin = "classifier"
	OriginCacheEmbedding     Origin = "cache_embedding"
	OriginProbe              Origin = "probe"
	OriginPlayground         Origin = "playground"
	OriginBatchSettlement    Origin = "batch_settlement"
	OriginRetainedSettlement Origin = "retained_settlement"
)

func (o Origin) known() bool {
	switch o {
	case OriginCaller, OriginShadow, OriginClassifier, OriginCacheEmbedding, OriginProbe, OriginPlayground, OriginBatchSettlement, OriginRetainedSettlement:
		return true
	default:
		return false
	}
}

func (o Origin) Keyless() bool {
	return o == OriginShadow || o == OriginProbe || o == OriginPlayground
}

func (o Origin) derivesFromParent() bool {
	return o == OriginShadow || o == OriginClassifier || o == OriginCacheEmbedding || o == OriginBatchSettlement || o == OriginRetainedSettlement
}

func validateDelegation(event *Event) error {
	if event.AgentPrincipalID == "" && event.MemberPrincipalID == "" && event.DelegationPairDigest == "" {
		return nil
	}
	if _, err := uuid.Parse(event.AgentPrincipalID); err != nil || !delegationDigestPattern.MatchString(event.DelegationPairDigest) || event.Origin.Keyless() || event.APIKeyID == "" {
		return errors.New("request metadata delegation attribution is invalid")
	}
	if event.MemberPrincipalID != "" {
		if _, err := uuid.Parse(event.MemberPrincipalID); err != nil {
			return errors.New("request metadata delegation member is invalid")
		}
	}
	return nil
}

// Event is one request's content-free metadata. Every optional field is
// serialized explicitly, so a reader can tell "absent" from "not yet known".
type Event struct {
	TrainingTokens     *int64           `json:"training_tokens,omitempty"`
	StorageByteSeconds *string          `json:"storage_byte_seconds,omitempty"`
	CacheTokenSeconds  *string          `json:"cache_token_seconds,omitempty"`
	ContainerSessions  map[string]int64 `json:"container_sessions,omitempty"`

	CacheResult          string  `json:"cache_result,omitempty"`
	EvaluatorGuardrailID *string `json:"evaluator_guardrail_id,omitempty"`
	BudgetBoundary       string  `json:"budget_boundary,omitempty"`
	Version              int     `json:"version"`
	EventID              string  `json:"event_id"`
	RequestID            string  `json:"request_id"`
	RuntimeGenerationID  string  `json:"runtime_generation_id"`
	// EndUserDigest contains a project-scoped digest, never a caller identifier.
	EndUserDigest string `json:"end_user_digest,omitempty"`
	// APIKeyID is empty exactly when the origin is keyless.
	APIKeyID             string            `json:"api_key_id"`
	AgentPrincipalID     string            `json:"agent_principal_id,omitempty"`
	MemberPrincipalID    string            `json:"member_principal_id,omitempty"`
	DelegationPairDigest string            `json:"delegation_pair_digest,omitempty"`
	Attribution          map[string]string `json:"attribution,omitempty"`
	// Origin is empty for a caller request. ParentRequestID names the caller
	// request a shadow, classifier, cache embedding or batch settlement derives from.
	Origin          Origin  `json:"origin,omitempty"`
	ParentRequestID *string `json:"parent_request_id,omitempty"`

	GuardrailDecisions []guardrails.Evidence    `json:"guardrail_decisions,omitempty"`
	PolicyDecisions    []contentpolicy.Decision `json:"policy_decisions,omitempty"`
	BudgetGroupID      *string                  `json:"budget_group_id"`
	ProviderID         *string                  `json:"provider_id"`
	RouteSlug          string                   `json:"route_slug"`
	UpstreamModel      *string                  `json:"upstream_model"`
	Operation          string                   `json:"operation"`
	Surface            string                   `json:"surface"`
	RequestStartedAt   time.Time                `json:"request_started_at"`
	RequestCompletedAt time.Time                `json:"request_completed_at"`
	// HTTPOutcomePending is used only by retained generation templates. Their
	// financial evidence may settle before the original HTTP exchange ends.
	// Snapshot timestamps remain valid, but persistence leaves its final HTTP
	// metadata unknown until the gateway supplies the actual completion.
	HTTPOutcomePending      bool             `json:"http_outcome_pending,omitempty"`
	ObservedAt              time.Time        `json:"observed_at"`
	StatusCode              *int             `json:"status_code"`
	ErrorClass              *string          `json:"error_class"`
	Committed               bool             `json:"committed"`
	LatencyMS               int64            `json:"latency_ms"`
	FirstByteMS             *int64           `json:"first_byte_ms"`
	InputTokens             *int64           `json:"input_tokens"`
	OutputTokens            *int64           `json:"output_tokens"`
	CachedInputTokens       *int64           `json:"cached_input_tokens"`
	CacheWriteInputTokens   *int64           `json:"cache_write_input_tokens"`
	CacheWrite5MInputTokens *int64           `json:"cache_write_5m_input_tokens"`
	CacheWrite1HInputTokens *int64           `json:"cache_write_1h_input_tokens"`
	AudioInputTokens        *int64           `json:"audio_input_tokens"`
	CachedAudioInputTokens  *int64           `json:"cached_audio_input_tokens"`
	AudioOutputTokens       *int64           `json:"audio_output_tokens"`
	ImageInputTokens        *int64           `json:"image_input_tokens"`
	CachedImageInputTokens  *int64           `json:"cached_image_input_tokens"`
	ImageOutputTokens       *int64           `json:"image_output_tokens"`
	MediaUnits              *string          `json:"media_units"`
	ToolCalls               map[string]int64 `json:"tool_calls,omitempty"`
	Batch                   bool             `json:"batch,omitempty"`
	UsageComplete           bool             `json:"usage_complete"`
	Unpriced                bool             `json:"unpriced"`
	PayloadCaptured         bool             `json:"payload_captured"`
	Attempts                []Attempt        `json:"attempts"`
}

// Attempt is one provider attempt made while serving the request. Routing and
// usage are omitted entirely when absent: a pre-attempt event carries neither,
// and the event contract does not require routing.
type Attempt struct {
	Execution          *ExecutionReference   `json:"execution,omitempty"`
	GuardrailDecisions []guardrails.Evidence `json:"guardrail_decisions,omitempty"`
	Routing            *Routing              `json:"routing,omitempty"`
	ID                 string                `json:"id"`
	Ordinal            int                   `json:"ordinal"`
	ProviderID         string                `json:"provider_id"`
	UpstreamModel      string                `json:"upstream_model"`
	StartedAt          time.Time             `json:"started_at"`
	CompletedAt        time.Time             `json:"completed_at"`
	StatusCode         *int                  `json:"status_code"`
	ErrorClass         *string               `json:"error_class"`
	Committed          bool                  `json:"committed"`
	LatencyMS          int64                 `json:"latency_ms"`
	FirstByteMS        *int64                `json:"first_byte_ms"`
	Usage              *AttemptUsage         `json:"usage,omitempty"`
	// InputEstimate is what admission estimated this attempt's input to be
	// before the provider reported it, absent when no estimate was made, as for
	// a stored-response call, a realtime session or a job poll. ModelFamily
	// names the tokenizer family of the attempt's model, which an attempt
	// records whether or not it was estimated. It is empty only on events from
	// before families were recorded.
	*InputEstimate
	ModelFamily string `json:"model_family,omitempty"`
}

// InputEstimate pairs an estimated input size with how it was counted.
type InputEstimate struct {
	Tokens     int64              `json:"estimated_input_tokens"`
	Provenance EstimateProvenance `json:"estimate_provenance"`
}

func (e *InputEstimate) Columns() (*int64, EstimateProvenance) {
	if e == nil {
		return nil, ""
	}
	return &e.Tokens, e.Provenance
}

type EstimateProvenance string

// How an attempt's input estimate was produced.
const (
	// EstimateTokenizer is an exact count by the family's own tokenizer of a
	// prompt that is all text and message framing.
	EstimateTokenizer EstimateProvenance = "tokenizer"
	// EstimateCalibrated is a count that is partly a ratio or a guess: the
	// characters-per-token heuristic scaled by a per-family factor, a tokenizer
	// count extrapolated past its bound, or one that also charges images, media,
	// tool schemas or tool calls.
	EstimateCalibrated EstimateProvenance = "calibrated"
	// EstimateHeuristic is the unscaled characters-per-token heuristic, which
	// also prices the tail of a long prompt that follows too little exact text
	// to measure a ratio from.
	EstimateHeuristic EstimateProvenance = "heuristic"
)

var delegationDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// modelFamilyPattern bounds a model family label. The set of families grows
// with the reference catalogue, so the label is checked for shape here and the
// producer owns which names exist.
var modelFamilyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidEstimate reports whether an attempt's estimate can be recorded as it
// is, for producers that would rather leave an annotation off than lose the
// event it rides on.
func ValidEstimate(estimate *InputEstimate, family string) bool {
	return checkEstimate(estimate, family) == nil
}

// checkEstimate reports why an attempt's estimate cannot be stored. The fact
// table enforces the same rules, and an event it would refuse must never reach
// it: the persisting transaction would abort after the delivery was
// acknowledged and be retried forever.
func checkEstimate(estimate *InputEstimate, family string) error {
	if estimate != nil {
		if estimate.Tokens < 0 {
			return errors.New("input estimate is negative")
		}
		if estimate.Provenance != EstimateTokenizer && estimate.Provenance != EstimateCalibrated && estimate.Provenance != EstimateHeuristic {
			return errors.New("estimate provenance is unknown")
		}
	}
	if family != "" && !modelFamilyPattern.MatchString(family) {
		return errors.New("model family is malformed")
	}
	return nil
}

// AttemptUsage is the billing evidence one attempt produced.
type AttemptUsage struct {
	UsageQuantities
	Evidence UsageEvidence
	Deferred bool
}

type UsageQuantities struct {
	InvocationCount    *int64           `json:"invocation_count,omitempty"`
	TrainingTokens     *int64           `json:"training_tokens,omitempty"`
	StorageByteSeconds *string          `json:"storage_byte_seconds,omitempty"`
	CacheTokenSeconds  *string          `json:"cache_token_seconds,omitempty"`
	ContainerSessions  map[string]int64 `json:"container_sessions,omitempty"`

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

type PromptReference struct {
	ID         string `json:"id"`
	RevisionID string `json:"revision_id"`
}

func (p *PromptReference) validate() error {
	if p == nil {
		return nil
	}
	if _, err := uuid.Parse(p.ID); err != nil {
		return errors.New("routing.prompt.id is invalid")
	}
	if _, err := uuid.Parse(p.RevisionID); err != nil {
		return errors.New("routing.prompt.revision_id is invalid")
	}
	return nil
}

// Routing is the provenance of one attempt: which policy chose it, which
// credential served it, and which revisions were in force. It is stored beside
// the attempt so a request can be explained months later, when the revisions
// that produced it have long been superseded.
type Routing struct {
	Prompt               *PromptReference     `json:"prompt,omitempty"`
	Cache                *CacheEvidence       `json:"cache,omitempty"`
	BudgetExempt         bool                 `json:"budget_exempt,omitempty"`
	CredentialSource     string               `json:"credential_source,omitempty"`
	Interaction          *InteractionEvidence `json:"interaction,omitempty"`
	Policy               *json.RawMessage     `json:"policy,omitempty"`
	Mode                 *string              `json:"mode,omitempty"`
	FirstOutputMS        *int64               `json:"first_output_ms,omitempty"`
	StreamedOutputTokens *int64               `json:"streamed_output_tokens,omitempty"`
	CredentialSlotID     *string              `json:"credential_slot_id"`
	CredentialVersionID  *string              `json:"credential_version_id"`
	ProviderRevisionID   string               `json:"provider_revision_id"`
	PricingRevisionID    *string              `json:"pricing_revision_id"`
	// Leg is the route an attempt ran on when that is not the route the
	// caller named: a selector's delegate or a fallback. It is nil on the
	// named route.
	Leg *RouteLeg `json:"leg,omitempty"`
	// Selector names the route selector that narrowed the attempt's targets.
	Selector *string `json:"selector,omitempty"`
	// Retry numbers a same-target retry from 1; it is nil on a first try.
	Retry *int `json:"retry,omitempty"`
	// Budgets are the supply-side spend caps the attempt's cost counts
	// against: its connection, credential slot or route, by identifier.
	Budgets []string `json:"budgets,omitempty"`
	// Baseline is the most expensive target the selector avoided, which
	// accounting prices with the attempt's own usage to report the
	// selector's savings. It is nil without a narrowing selector.
	Baseline *Baseline `json:"baseline,omitempty"`
}

// Baseline names the provider model a selector's savings are measured
// against.
type Baseline struct {
	ProviderID    string  `json:"provider_id"`
	UpstreamModel string  `json:"upstream_model"`
	VendorID      *string `json:"vendor_id,omitempty"`
}

// RouteLeg is a route an attempt ran on in place of the named one, pinned to
// the revision the request's release carried.
type RouteLeg struct {
	Route      string `json:"route"`
	RevisionID string `json:"revision_id"`
	Via        LegVia `json:"via"`
}

// LegVia is how a request reached a route it ran on.
type LegVia string

const (
	ViaSelector LegVia = "selector"
	ViaFallback LegVia = "fallback"
)

// maxAttemptBudgets bounds the caps one attempt counts against: one each for
// its connection, slot and route.
const maxAttemptBudgets = 3

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
