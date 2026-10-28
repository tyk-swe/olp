package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/mcpservers"
)

type toolGuardrailState struct {
	engine    *guardrails.Engine
	masks     *guardrails.Masking
	decisions []guardrails.Evidence
}

func guardrailRefusal(err error) *apiError {
	var refusal *guardrails.Error
	if errors.As(err, &refusal) {
		return fail(refusal.Status, refusal.Code, "invalid_request_error", "The tool call could not satisfy the configured guardrails.")
	}
	return fail(http.StatusInternalServerError, "guardrail_unavailable", "api_error", "The guardrail could not be evaluated.")
}

func (s *Server) toolGuardrails(in executeInput) *toolGuardrailState {
	if in.snapshot == nil {
		return nil
	}
	engine := in.snapshot.ToolGuardrailEngine(in.resolved.id, in.authority.ID)
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

func (s *Server) evaluateToolPhase(ctx context.Context, in executeInput, reservation *Reservation, state *toolGuardrailState, phase string, segments []guardrails.Segment) (guardrails.Result, error) {
	if s.Guardrails == nil {
		return state.engine.Unavailable(phase)
	}
	return s.Guardrails(ctx, GuardrailEvaluation{
		Authority: in.authority, Release: in.release, Engine: state.engine,
		RequestID: reservation.AttemptID, Phase: phase, Segments: segments, Masks: state.masks,
	})
}

func (s *Server) refuseGuardedCall(ctx context.Context, in executeInput, reservation *Reservation, state *toolGuardrailState) {
	now := s.now()
	evidence := AttemptEvidence{
		ServerID: in.tool.ServerID, ServerRevisionID: in.tool.ServerRevisionID,
		Status: StatusGuardrailBlocked, StartedAt: now, CompletedAt: now,
		GuardrailDecisions: stateDecisions(state),
	}
	if err := s.Ledger.RefuseUndispatched(context.WithoutCancel(ctx), reservation, evidence); err != nil {
		s.release(reservation)
	}
}

func (s *Server) guardArguments(ctx context.Context, in executeInput, reservation *Reservation, state *toolGuardrailState) (json.RawMessage, *apiError) {
	if state == nil {
		return in.arguments, nil
	}
	for _, phase := range []string{guardrails.InputParallel, guardrails.OutputStream} {
		result, err := state.engine.Unavailable(phase)
		state.decisions = append(state.decisions, result.Decisions...)
		if err != nil {
			s.refuseGuardedCall(ctx, in, reservation, state)
			return nil, guardrailRefusal(err)
		}
	}
	segments := []guardrails.Segment{{ID: "arguments", Kind: "tool_call", ToolName: in.tool.Alias, Text: string(in.arguments)}}
	result, err := s.evaluateToolPhase(ctx, in, reservation, state, guardrails.Input, segments)
	state.decisions = append(state.decisions, result.Decisions...)
	if err != nil {
		s.refuseGuardedCall(ctx, in, reservation, state)
		return nil, guardrailRefusal(err)
	}
	if !result.Changed {
		return in.arguments, nil
	}
	if len(result.Segments) != 1 {
		s.refuseGuardedCall(ctx, in, reservation, state)
		return nil, errGuardrailBlocked
	}
	transformed := json.RawMessage(result.Segments[0].Text)
	if len(transformed) > maxCallBodyBytes || in.tool.ValidateArguments(transformed) != nil {
		s.refuseGuardedCall(ctx, in, reservation, state)
		return nil, errGuardrailBlocked
	}
	return transformed, nil
}

type toolResultSurface struct {
	segments []guardrails.Segment
	opaque   bool
}

func projectToolResult(in executeInput, result mcpservers.ToolResult) toolResultSurface {
	surface := toolResultSurface{segments: []guardrails.Segment{}}
	for index, block := range result.Content {
		if block.Type != "text" {
			surface.opaque = true
			continue
		}
		surface.segments = append(surface.segments, guardrails.Segment{
			ID: "content" + strconv.Itoa(index), Kind: "tool_result",
			ToolName: in.tool.Alias, Untrusted: true, Text: block.Text,
		})
	}
	if len(result.StructuredContent) > 0 && string(result.StructuredContent) != "null" {
		surface.segments = append(surface.segments, guardrails.Segment{
			ID: "structured", Kind: "tool_result",
			ToolName: in.tool.Alias, Untrusted: true, Text: string(result.StructuredContent),
		})
	}
	return surface
}

func applyToolResult(result mcpservers.ToolResult, segments []guardrails.Segment) (mcpservers.ToolResult, error) {
	texts := make(map[string]string, len(segments))
	for _, segment := range segments {
		texts[segment.ID] = segment.Text
	}
	applied := mcpservers.ToolResult{
		Content:           slices.Clone(result.Content),
		StructuredContent: slices.Clone(result.StructuredContent),
		IsError:           result.IsError,
	}
	for index, block := range applied.Content {
		if block.Type != "text" {
			continue
		}
		text, ok := texts["content"+strconv.Itoa(index)]
		if !ok {
			return result, guardrails.ErrSurface
		}
		applied.Content[index].Text = text
	}
	if len(result.StructuredContent) > 0 && string(result.StructuredContent) != "null" {
		text, ok := texts["structured"]
		if !ok || !json.Valid([]byte(text)) {
			return result, guardrails.ErrSurface
		}
		applied.StructuredContent = json.RawMessage(text)
	}
	return applied, nil
}

func (s *Server) guardResult(ctx context.Context, in executeInput, reservation *Reservation, state *toolGuardrailState, result mcpservers.ToolResult) (mcpservers.ToolResult, error) {
	if state == nil {
		return result, nil
	}
	surface := projectToolResult(in, result)
	if surface.opaque {
		unavailable, err := state.engine.Unavailable(guardrails.Output)
		state.decisions = append(state.decisions, unavailable.Decisions...)
		if err != nil {
			return result, err
		}
		return result, nil
	}
	segments := surface.segments
	restored := false
	for index := range segments {
		value := state.masks.Restore(segments[index].Text)
		if value != segments[index].Text {
			segments[index].Text = value
			restored = true
		}
	}
	evaluated, err := s.evaluateToolPhase(ctx, in, reservation, state, guardrails.Output, segments)
	state.decisions = append(state.decisions, evaluated.Decisions...)
	if err != nil {
		return result, err
	}
	if !evaluated.Changed && !restored {
		return result, nil
	}
	applied, err := applyToolResult(result, evaluated.Segments)
	if err != nil {
		return result, err
	}
	encoded, _ := json.Marshal(applied)
	if len(encoded) > maxCallBodyBytes {
		return result, guardrails.ErrBlocked
	}
	return applied, nil
}
