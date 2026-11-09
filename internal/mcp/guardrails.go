package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/oif"
)

type ToolPolicies interface {
	ToolGuardrailEngine(string, string) *guardrails.Engine
}

type toolGuardrailState struct {
	engine    *guardrails.Engine
	masks     *guardrails.Masking
	decisions []guardrails.Evidence
}

func guardrailRefusal(err error) *Failure {
	var policyFailure guardrails.Failure
	if errors.As(err, &policyFailure) {
		return &Failure{Code: string(policyFailure), Message: "The tool call could not satisfy the configured guardrails.", Guardrail: policyFailure}
	}
	return refusal("guardrail_unavailable", "The guardrail could not be evaluated.")
}

func (s *Executor) toolGuardrails(in CallRequest) *toolGuardrailState {
	if in.Policies == nil {
		return nil
	}
	engine := in.Policies.ToolGuardrailEngine(in.ToolsetID, in.Authority.ID)
	if engine == nil {
		return nil
	}
	return &toolGuardrailState{engine: engine, masks: &guardrails.Masking{}}
}

func stateDecisions(state *toolGuardrailState) []guardrails.Evidence {
	if state == nil {
		return nil
	}
	return slices.Clone(state.decisions)
}

func (s *Executor) evaluateToolPhase(ctx context.Context, in CallRequest, reservation *accounting.Reservation, state *toolGuardrailState, phase guardrails.Phase, segments []guardrails.Segment) (guardrails.Result, error) {
	if s.guardrails == nil {
		return state.engine.Unavailable(phase)
	}
	return s.guardrails(ctx, GuardrailEvaluation{
		Authority: in.Authority, Release: in.Release, Engine: state.engine,
		RequestID: reservation.AttemptID, Phase: phase, Segments: segments, Masks: state.masks,
	})
}

func (s *Executor) refuseGuardedCall(ctx context.Context, in CallRequest, reservation *accounting.Reservation, state *toolGuardrailState) {
	now := s.now()
	evidence := accounting.AttemptEvidence{IdentityDecisions: toolIdentityDecisions(in.Tool, nil),
		ServerID: in.Tool.ServerID, ServerRevisionID: in.Tool.ServerRevisionID,
		Status: accounting.StatusGuardrailBlocked, StartedAt: now, CompletedAt: now,
		GuardrailDecisions: stateDecisions(state),
	}
	if err := s.ledger.RefuseUndispatched(context.WithoutCancel(ctx), reservation, evidence); err != nil {
		s.release(reservation)
	}
}

func unsupportedToolPhases(engine *guardrails.Engine, phases []guardrails.Phase) ([]guardrails.Evidence, error) {
	if len(phases) == 0 {
		return nil, nil
	}
	result, err := engine.Unavailable(phases[0])
	if err != nil {
		return result.Decisions, err
	}
	next, failure := unsupportedToolPhases(engine, phases[1:])
	return slices.Concat(result.Decisions, next), failure
}

func (s *Executor) guardArguments(ctx context.Context, in CallRequest, reservation *accounting.Reservation, state *toolGuardrailState) (json.RawMessage, *toolGuardrailState, *Failure) {
	if state == nil {
		return in.Arguments, nil, nil
	}
	unavailable, failure := unsupportedToolPhases(state.engine, []guardrails.Phase{guardrails.InputParallel, guardrails.OutputStream})
	if failure != nil {
		refused := &toolGuardrailState{engine: state.engine, masks: state.masks, decisions: unavailable}
		s.refuseGuardedCall(ctx, in, reservation, refused)
		return nil, refused, guardrailRefusal(failure)
	}
	document, parseErr := oif.ParseJSON(in.Arguments, oif.Limits{MaxBytes: maxCallBodyBytes})
	if parseErr != nil {
		s.refuseGuardedCall(ctx, in, reservation, state)
		return nil, state, failureGuardrailBlocked
	}
	surface := guardrails.ToolJSONSurface(document, in.Tool.Alias, false)
	result, err := s.evaluateToolPhase(ctx, in, reservation, state, guardrails.Input, surface.Segments())
	guarded := &toolGuardrailState{engine: state.engine, masks: state.masks, decisions: slices.Concat(unavailable, result.Decisions)}
	if err != nil {
		s.refuseGuardedCall(ctx, in, reservation, guarded)
		return nil, guarded, guardrailRefusal(err)
	}
	if !result.Changed {
		return in.Arguments, guarded, nil
	}
	applied, applyErr := surface.Apply(result.Segments)
	if applyErr != nil {
		s.refuseGuardedCall(ctx, in, reservation, guarded)
		return nil, guarded, failureGuardrailBlocked
	}
	transformed := json.RawMessage(applied.Bytes())
	if len(transformed) > maxCallBodyBytes || in.Tool.ValidateArguments(transformed) != nil {
		s.refuseGuardedCall(ctx, in, reservation, guarded)
		return nil, guarded, failureGuardrailBlocked
	}
	return transformed, guarded, nil
}

type toolResultSurface struct {
	segments   []guardrails.Segment
	structured *guardrails.JSONSurface
	opaque     bool
}

func projectToolResult(in CallRequest, result upstream.ToolResult) (toolResultSurface, error) {
	content := slices.Collect(func(yield func(guardrails.Segment) bool) {
		for index, block := range result.Content {
			if block.Type == "text" && !yield(guardrails.Segment{
				ID: "content" + strconv.Itoa(index), Kind: "tool_result",
				ToolName: in.Tool.Alias, Untrusted: true, Text: block.Text,
			}) {
				return
			}
		}
	})
	opaque := slices.ContainsFunc(result.Content, func(block upstream.ToolContent) bool { return block.Type != "text" })
	if len(result.StructuredContent) == 0 || string(result.StructuredContent) == "null" {
		return toolResultSurface{segments: content, opaque: opaque}, nil
	}
	document, err := oif.ParseJSON(result.StructuredContent, oif.Limits{MaxBytes: maxCallBodyBytes})
	if err != nil {
		return toolResultSurface{}, guardrails.ErrSurface
	}
	structured := guardrails.ToolJSONSurface(document, in.Tool.Alias, true)
	return toolResultSurface{segments: slices.Concat(content, structured.Segments()), structured: structured, opaque: opaque}, nil
}

func applyToolResult(result upstream.ToolResult, surface toolResultSurface, segments []guardrails.Segment) (upstream.ToolResult, error) {
	texts := maps.Collect(func(yield func(string, string) bool) {
		for _, segment := range segments {
			if !yield(segment.ID, segment.Text) {
				return
			}
		}
	})
	for index, block := range result.Content {
		if block.Type != "text" {
			continue
		}
		_, ok := texts["content"+strconv.Itoa(index)]
		if !ok {
			return result, guardrails.ErrSurface
		}
	}
	content := slices.Collect(func(yield func(upstream.ToolContent) bool) {
		for index, block := range result.Content {
			if block.Type != "text" {
				if !yield(block) {
					return
				}
				continue
			}
			if !yield(upstream.ToolContent{Type: block.Type, Text: texts["content"+strconv.Itoa(index)], Data: block.Data, MimeType: block.MimeType}) {
				return
			}
		}
	})
	if surface.structured == nil {
		return upstream.ToolResult{Content: content, StructuredContent: result.StructuredContent, IsError: result.IsError}, nil
	}
	document, err := surface.structured.Apply(segments)
	if err != nil {
		return result, err
	}
	return upstream.ToolResult{Content: content, StructuredContent: document.Bytes(), IsError: result.IsError}, nil
}

func (s *Executor) guardResult(ctx context.Context, in CallRequest, reservation *accounting.Reservation, state *toolGuardrailState, result upstream.ToolResult) (upstream.ToolResult, []guardrails.Evidence, error) {
	if state == nil {
		return result, nil, nil
	}
	surface, projectErr := projectToolResult(in, result)
	if projectErr != nil {
		return result, stateDecisions(state), projectErr
	}
	if surface.opaque {
		unavailable, err := state.engine.Unavailable(guardrails.Output)
		return result, slices.Concat(state.decisions, unavailable.Decisions), err
	}
	segments := slices.Collect(func(yield func(guardrails.Segment) bool) {
		for _, segment := range surface.segments {
			if !yield(segment.Restore(state.masks)) {
				return
			}
		}
	})
	evaluated, err := s.evaluateToolPhase(ctx, in, reservation, state, guardrails.Output, segments)
	decisions := slices.Concat(state.decisions, evaluated.Decisions)
	if err != nil {
		return result, decisions, err
	}
	if !evaluated.Changed && slices.Equal(surface.segments, segments) {
		return result, decisions, nil
	}
	applied, err := applyToolResult(result, surface, evaluated.Segments)
	if err != nil {
		return result, decisions, err
	}
	encoded, _ := json.Marshal(applied)
	if len(encoded) > maxCallBodyBytes {
		return result, decisions, guardrails.ErrBlocked
	}
	return applied, decisions, nil
}
