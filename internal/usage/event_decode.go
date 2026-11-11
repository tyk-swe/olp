package usage

import (
	"errors"
	"fmt"
	"math"

	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/routeidentity"
)

var knownSurfaces = map[string]struct{}{
	"openai": {}, "anthropic": {}, "gemini": {}, "bedrock": {}, "native": {}, "unknown": {},
}

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
		if !w.Origin.known() {
			return nil, errors.New("request metadata field origin is not supported")
		}
		event.Origin = *w.Origin
	}
	if event.Origin.Keyless() {
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
	if (event.ParentRequestID != nil) != (event.Origin.derivesFromParent() || event.EvaluatorGuardrailID != nil) {
		return nil, errors.New("request metadata field parent_request_id belongs to child work requests")
	}
	if w.EndUserDigest != nil {
		event.EndUserDigest = *w.EndUserDigest
		if event.EndUserDigest != "" && (!authority.ValidEndUserDigest(event.EndUserDigest) || event.Origin.Keyless()) {
			return nil, errors.New("request metadata field end_user_digest is invalid")
		}
	}
	if w.Attribution != nil {
		event.Attribution = *w.Attribution
		if err := attribution.Validate(event.Attribution); err != nil {
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
	if event.Operation != "file" && event.Operation != "guardrail" && !ValidOperation(event.Operation) {
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
	hasProvenance := w.EstimateProvenance != nil && *w.EstimateProvenance != ""
	if (w.EstimatedInputTokens != nil) != hasProvenance {
		return nil, errors.New("request metadata input estimate and its provenance must appear together")
	}
	if hasProvenance {
		attempt.InputEstimate = &InputEstimate{Tokens: *w.EstimatedInputTokens, Provenance: *w.EstimateProvenance}
	}
	if w.ModelFamily != nil {
		attempt.ModelFamily = *w.ModelFamily
	}
	if err = checkEstimate(attempt.InputEstimate, attempt.ModelFamily); err != nil {
		return nil, fmt.Errorf("request metadata %w", err)
	}
	return attempt, nil
}

func (w wireUsage) decode() (*AttemptUsage, error) {
	usage := w.attemptUsage()
	observed, err := requiredField("usage.observed", w.Observed)
	if err != nil {
		return nil, err
	}
	complete, err := requiredField("usage.complete", w.Complete)
	if err != nil {
		return nil, err
	}
	uncertain, err := requiredField("usage.billing_uncertain", w.BillingUncertain)
	if err != nil {
		return nil, err
	}
	usage.Evidence = EvidenceFrom(observed, complete, uncertain)
	if usage.MediaUnits, err = optionalDecimal("usage.media_units", w.MediaUnits); err != nil {
		return nil, err
	}
	return &usage, nil
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
